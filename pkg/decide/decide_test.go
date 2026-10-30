package decide

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kubeamber/kubeamber/pkg/policy"
	"github.com/kubeamber/kubeamber/pkg/risk"
)

const doc = `
apiVersion: kubeamber.dev/v1alpha1
kind: Policy
metadata: {name: test}
spec:
  agents:
    - name: bot
      serviceAccounts: ["agents/*"]
  protectedNamespaces: [prod]
  rules:
    - name: no-rbac
      match: {kinds: [ClusterRoleBinding, RoleBinding]}
      action: deny
      reason: agents may not change RBAC
    - name: sandbox
      match: {namespaces: ["sandbox-*"]}
      action: allow
  freezeWindows:
    - name: launch
      from: "2026-11-01T00:00:00Z"
      until: "2026-11-02T00:00:00Z"
      namespaces: [shop]
  guards:
    prometheus:
      url: http://prometheus
      checks:
        - name: slo
          query: up
`

const agentUser = "system:serviceaccount:agents:bot"

var normalDay = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)

type fakeGuard struct{ fire bool }

func (g fakeGuard) Evaluate(context.Context, *policy.PrometheusGuard, string) []GuardResult {
	if !g.fire {
		return nil
	}
	return []GuardResult{{Name: "slo", Verdict: policy.Hold, Reason: `guard "slo" fired`}}
}

func engine(t *testing.T, g Guard) *Engine {
	t.Helper()
	p, err := policy.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Policy: func() *policy.Policy { return p }, Guard: g,
		Self: Self{Namespace: "kubeamber-system", WebhookName: "kubeamber"}}
}

func TestEvaluate(t *testing.T) {
	cm := func(ns string) risk.Change {
		return risk.Change{Operation: "UPDATE", Kind: "ConfigMap", Namespace: ns, Name: "c"}
	}
	cases := []struct {
		name       string
		user       string
		change     risk.Change
		now        time.Time
		guard      bool
		want       policy.Verdict
		wantReason string
		wantRule   string
	}{
		{name: "humans are never evaluated", user: "alice", change: risk.Change{Operation: "DELETE", Kind: "Namespace", Name: "prod"}, want: policy.Allow, wantReason: "not an agent"},
		{name: "low risk", change: cm("shop"), want: policy.Allow, wantReason: "below the hold threshold"},
		{name: "hold on score", change: risk.Change{Operation: "DELETE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "d"}, want: policy.Hold},
		{name: "deny on score", change: risk.Change{Operation: "DELETE", Kind: "Namespace", Name: "shop"}, want: policy.Deny},
		{name: "deny rule", change: risk.Change{Operation: "CREATE", Group: "rbac.authorization.k8s.io", Kind: "RoleBinding", Namespace: "shop", Name: "r"}, want: policy.Deny, wantRule: "no-rbac", wantReason: "may not change RBAC"},
		{name: "allow rule overrides score", change: risk.Change{Operation: "DELETE", Kind: "Namespace", Name: "sandbox-1"}, want: policy.Allow, wantRule: "sandbox"},
		{name: "freeze holds", change: cm("shop"), now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), want: policy.Hold, wantReason: `freeze "launch"`},
		{name: "freeze is scoped", change: cm("other"), now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), want: policy.Allow},
		{name: "guard escalates", change: cm("shop"), guard: true, want: policy.Hold, wantReason: `guard "slo"`},
		{name: "guard applies despite allow rule", change: cm("sandbox-1"), guard: true, want: policy.Hold},
		{name: "self: namespace", change: cm("kubeamber-system"), want: policy.Deny, wantRule: SelfProtectionRule},
		{name: "self: webhook", change: risk.Change{Operation: "DELETE", Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration", Name: "kubeamber"}, want: policy.Deny, wantRule: SelfProtectionRule},
		{name: "self: admission policy", change: risk.Change{Operation: "DELETE", Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicyBinding", Name: "kubeamber-protect-admission"}, want: policy.Deny, wantRule: SelfProtectionRule},
		{name: "self: change requests", change: risk.Change{Operation: "UPDATE", Group: "kubeamber.dev", Kind: "ChangeRequest", Name: "cr-1"}, want: policy.Deny, wantRule: SelfProtectionRule},
		{name: "self: CRD", change: risk.Change{Operation: "DELETE", Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition", Name: "changerequests.kubeamber.dev"}, want: policy.Deny, wantRule: SelfProtectionRule},
		{name: "self: namespace object", change: risk.Change{Operation: "DELETE", Kind: "Namespace", Name: "kubeamber-system"}, want: policy.Deny, wantRule: SelfProtectionRule},
		{name: "self: RBAC", change: risk.Change{Operation: "UPDATE", Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "kubeamber-approver"}, want: policy.Deny, wantRule: SelfProtectionRule},
		{name: "self: mint a token for KubeAmber's bridge", change: risk.Change{Operation: "CREATE", Kind: "ServiceAccount", Resource: "serviceaccounts", SubResource: "token", Namespace: "kubeamber-system", Name: "kubeamber-bridge"}, want: policy.Deny, wantRule: SelfProtectionRule},
		{name: "mint a token elsewhere is held", change: risk.Change{Operation: "CREATE", Kind: "ServiceAccount", Resource: "serviceaccounts", SubResource: "token", Namespace: "shop", Name: "deployer"}, want: policy.Hold},
		{name: "self: take over an agent identity", change: risk.Change{Operation: "UPDATE", Kind: "ServiceAccount", Namespace: "agents", Name: "claude-code-alice",
			OldObject: map[string]any{"metadata": map[string]any{"annotations": map[string]any{OwnerAnnotation: "alice"}}},
			Object:    map[string]any{"metadata": map[string]any{"annotations": map[string]any{OwnerAnnotation: "mallory"}}}}, want: policy.Deny, wantRule: SelfProtectionRule},
		{name: "self: forge an agent identity", change: risk.Change{Operation: "CREATE", Kind: "ServiceAccount", Namespace: "agents", Name: "claude-code-bob",
			Object: map[string]any{"metadata": map[string]any{"annotations": map[string]any{OwnerAnnotation: "bob"}}}}, want: policy.Deny, wantRule: SelfProtectionRule},
		{name: "plain service account changes are scored", change: risk.Change{Operation: "UPDATE", Kind: "ServiceAccount", Namespace: "shop", Name: "web",
			Object: map[string]any{"metadata": map[string]any{"labels": map[string]any{"x": "y"}}}}, want: policy.Allow},
		{name: "self-protection beats allow rule", change: risk.Change{Operation: "DELETE", Kind: "Namespace", Name: "kubeamber-system"}, want: policy.Deny},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			user := c.user
			if user == "" {
				user = agentUser
			}
			now := c.now
			if now.IsZero() {
				now = normalDay
			}
			d := engine(t, fakeGuard{c.guard}).Evaluate(context.Background(), Request{User: user, Change: c.change, Now: now})
			if d.Verdict != c.want {
				t.Errorf("verdict = %s, want %s (score %d, reasons %q)", d.Verdict, c.want, d.Score, d.Reasons)
			}
			if c.wantReason != "" && !strings.Contains(strings.Join(d.Reasons, "|"), c.wantReason) {
				t.Errorf("reasons %q do not mention %q", d.Reasons, c.wantReason)
			}
			if c.wantRule != "" && d.Rule != c.wantRule {
				t.Errorf("rule = %q, want %q", d.Rule, c.wantRule)
			}
		})
	}
}

func TestImagePolicy(t *testing.T) {
	p, err := policy.Parse([]byte(doc + "  images: {trusted: ['cgr.dev/chainguard/*'], requireDigest: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Policy: func() *policy.Policy { return p }}
	deploy := func(image string) risk.Change {
		return risk.Change{Operation: "CREATE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "web",
			Object: map[string]any{"kind": "Deployment", "spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "web", "image": image}},
			}}}}}
	}
	d := e.Evaluate(context.Background(), Request{User: agentUser, Change: deploy("nginx:latest"), Now: normalDay})
	if d.Verdict != policy.Hold || d.Score != 50 {
		t.Errorf("untrusted image: verdict %s, score %d, want hold at 50; factors %+v", d.Verdict, d.Score, d.Factors)
	}
	d = e.Evaluate(context.Background(), Request{User: agentUser, Change: deploy("cgr.dev/chainguard/nginx:latest@sha256:fc42af33"), Now: normalDay})
	if d.Verdict != policy.Allow || d.Score != 10 {
		t.Errorf("pinned Chainguard image: verdict %s, score %d, want allow at 10; factors %+v", d.Verdict, d.Score, d.Factors)
	}
}

func TestAgentOverride(t *testing.T) {
	d := engine(t, nil).Evaluate(context.Background(), Request{Agent: "bot", Change: risk.Change{Operation: "UPDATE", Kind: "ConfigMap", Namespace: "shop"}, Now: normalDay})
	if d.Agent != "bot" || d.Verdict != policy.Allow {
		t.Errorf("got %+v", d)
	}
}

func TestRuleReasonIsNotRepeated(t *testing.T) {
	d := engine(t, nil).Evaluate(context.Background(), Request{User: agentUser, Now: normalDay,
		Change: risk.Change{Operation: "CREATE", Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding", Name: "x"}})
	if d.Verdict != policy.Deny || len(d.Reasons) != 1 {
		t.Errorf("a deny rule should be the only reason, got %q", d.Reasons)
	}
}

func TestSelfProtectionMessage(t *testing.T) {
	d := engine(t, nil).Evaluate(context.Background(), Request{User: agentUser, Now: normalDay,
		Change: risk.Change{Operation: "DELETE", Kind: "Namespace", Name: "kubeamber-system"}})
	if got, want := Message(d), `kubeamber: DENY: agents cannot modify KubeAmber's namespace "kubeamber-system"`; got != want {
		t.Errorf("Message = %q, want %q", got, want)
	}
}

func TestMessage(t *testing.T) {
	d := Decision{Verdict: policy.Hold, Score: 75, Summary: "replicas 3 → 0", Reasons: []string{"risk 75/100 is at or above the hold threshold (30)"},
		Factors: []risk.Factor{{Points: 35, Reason: "scales checkout from 3 to 0 replicas"}, {Points: -20, Reason: "managed"}}}
	got := Message(d)
	want := "kubeamber: HOLD (risk 75/100, replicas 3 → 0): risk 75/100 is at or above the hold threshold (30). Factors: +35 scales checkout from 3 to 0 replicas, -20 managed"
	if got != want {
		t.Errorf("Message =\n%s\nwant\n%s", got, want)
	}
}
