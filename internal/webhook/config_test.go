package webhook

import (
	"context"
	"errors"
	"testing"

	"cel.dev/cel-go/cel"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubeamber/kubeamber/pkg/policy"
)

const exprPolicy = `
apiVersion: kubeamber.dev/v1alpha1
kind: Policy
metadata: {name: test}
spec:
  agents:
    - name: in-cluster
      serviceAccounts: ["agents/*", "holmes/holmes-sa"]
    - name: workstation
      users: ["agent:claude-code", "https://idp.example.com#bot.*"]
      groups: ["kubeamber:agents", "o'brien"]
`

// evalExpr compiles a match condition the way the API server would see it
// (request as a dynamic object) and evaluates it for one identity.
func evalExpr(t *testing.T, expr, user string, groups []string) bool {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		t.Fatalf("expression does not compile: %v\n%s", iss.Err(), expr)
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	gs := make([]any, len(groups))
	for i, g := range groups {
		gs[i] = g
	}
	out, _, err := prg.Eval(map[string]any{"request": map[string]any{
		"userInfo": map[string]any{"username": user, "groups": gs},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return out.Value().(bool)
}

func TestAgentExpression(t *testing.T) {
	p, err := policy.Parse([]byte(exprPolicy))
	if err != nil {
		t.Fatal(err)
	}
	expr := AgentExpression(p, selfUser)
	cases := []struct {
		user   string
		groups []string
		want   bool
	}{
		{"system:serviceaccount:agents:sre", nil, true},
		{"system:serviceaccount:holmes:holmes-sa", nil, true},
		{"system:serviceaccount:holmes:other", nil, false},
		{"agent:claude-code", nil, true},
		{"https://idp.example.com#bot.42", nil, true},
		{"https://idp-example.com#bot.42", nil, false}, // dots are literal
		{"alice", []string{"kubeamber:agents"}, true},
		{"alice", []string{"o'brien"}, true},
		{"alice", []string{"system:authenticated"}, false},
		{selfUser, []string{"kubeamber:agents"}, false},
	}
	for _, c := range cases {
		if got := evalExpr(t, expr, c.user, c.groups); got != c.want {
			t.Errorf("%s %v: matched = %v, want %v", c.user, c.groups, got, c.want)
		}
	}
}

func TestConfiguratorSync(t *testing.T) {
	p, err := policy.Parse([]byte(exprPolicy))
	if err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset(&admissionregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeamber"},
		Webhooks: []admissionregv1.ValidatingWebhook{
			{Name: AgentsWebhook, MatchConditions: []admissionregv1.MatchCondition{
				{Name: AgentMatchCondition, Expression: "false"},
				{Name: "user-extra", Expression: "true"},
			}},
			{Name: "changerequests.kubeamber.dev"},
		},
	}, &admissionregv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeamber-protect-admission"},
		Spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConditions: []admissionregv1.MatchCondition{
			{Name: AgentMatchCondition, Expression: "false"},
		}},
	})
	c := &Configurator{Client: cs, Name: "kubeamber", AdmissionPolicy: "kubeamber-protect-admission", SelfUser: selfUser}
	ctx := context.Background()
	changed, err := c.Sync(ctx, []byte("CA"), p)
	if err != nil || !changed {
		t.Fatalf("first sync: changed=%v err=%v", changed, err)
	}
	cfg, _ := cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, "kubeamber", metav1.GetOptions{})
	for _, w := range cfg.Webhooks {
		if string(w.ClientConfig.CABundle) != "CA" {
			t.Errorf("%s: CA bundle not set", w.Name)
		}
	}
	mc := cfg.Webhooks[0].MatchConditions
	if len(mc) != 2 || mc[0].Name != AgentMatchCondition || mc[0].Expression != AgentExpression(p, selfUser) || mc[1].Name != "user-extra" {
		t.Errorf("match conditions: %+v", mc)
	}
	if len(cfg.Webhooks[1].MatchConditions) != 0 {
		t.Error("match condition added to the change request webhook")
	}
	vap, _ := cs.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, "kubeamber-protect-admission", metav1.GetOptions{})
	if len(vap.Spec.MatchConditions) != 1 || vap.Spec.MatchConditions[0].Expression != AgentExpression(p, selfUser) {
		t.Errorf("admission policy match conditions: %+v", vap.Spec.MatchConditions)
	}
	if changed, err := c.Sync(ctx, []byte("CA"), p); err != nil || changed {
		t.Errorf("second sync should be a no-op: changed=%v err=%v", changed, err)
	}
}

// Two replicas syncing at once make one update fail with a conflict; Sync
// reads again and retries instead of logging an error.
func TestConfiguratorSyncRetriesConflicts(t *testing.T) {
	p, err := policy.Parse([]byte(exprPolicy))
	if err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset(&admissionregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeamber"},
		Webhooks:   []admissionregv1.ValidatingWebhook{{Name: AgentsWebhook}},
	})
	conflicts := 1
	cs.PrependReactor("update", "validatingwebhookconfigurations", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			return false, nil, nil
		}
		conflicts--
		gr := schema.GroupResource{Group: "admissionregistration.k8s.io", Resource: "validatingwebhookconfigurations"}
		return true, nil, apierrors.NewConflict(gr, "kubeamber", errors.New("the object has been modified"))
	})
	c := &Configurator{Client: cs, Name: "kubeamber", SelfUser: selfUser}
	ctx := context.Background()
	if changed, err := c.Sync(ctx, []byte("CA"), p); err != nil || !changed {
		t.Fatalf("sync after a conflict: changed=%v err=%v", changed, err)
	}
	cfg, _ := cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, "kubeamber", metav1.GetOptions{})
	if string(cfg.Webhooks[0].ClientConfig.CABundle) != "CA" {
		t.Error("CA bundle not written after the retry")
	}
}
