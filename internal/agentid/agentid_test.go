package agentid

import (
	"context"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
)

func TestName(t *testing.T) {
	cases := map[[2]string]string{
		{"claude-code", "alice@contoso.com"}: "claude-code-alice-contoso-com",
		{"codex", "kubernetes-admin"}:        "codex-kubernetes-admin",
		{"holmes", "0B2E7D41-AA10"}:          "holmes-0b2e7d41-aa10",
	}
	for in, want := range cases {
		if got, err := Name(in[0], in[1]); err != nil || got != want {
			t.Errorf("Name(%q, %q) = %q, %v; want %q", in[0], in[1], got, err, want)
		}
	}
	long, err := Name("claude-code", strings.Repeat("very-long-identity-", 5)+"@example.com")
	if err != nil || len(long) > 63 {
		t.Errorf("long name %q (%d chars): %v", long, len(long), err)
	}
	other, _ := Name("claude-code", strings.Repeat("very-long-identity-", 5)+"@example.org")
	if other == long {
		t.Error("two long owners collided")
	}
	for _, bad := range []string{"Claude", "claude_code", "-x", ""} {
		if _, err := Name(bad, "alice"); err == nil {
			t.Errorf("agent name %q accepted", bad)
		}
	}
}

func TestEnsureAndRevoke(t *testing.T) {
	cs := fake.NewClientset()
	ctx := context.Background()
	id := Identity{Namespace: "kubeamber-agents", Agent: "claude-code", Owner: "alice", Role: "edit", Namespaces: []string{"dev", "staging"}}

	sa, err := Ensure(ctx, cs, id)
	if err != nil {
		t.Fatal(err)
	}
	if sa.Name != "claude-code-alice" || sa.Annotations[OwnerAnnotation] != "alice" || sa.Labels[AgentLabel] != "claude-code" {
		t.Errorf("service account %+v", sa.ObjectMeta)
	}
	for _, ns := range []string{"dev", "staging"} {
		rb, err := cs.RbacV1().RoleBindings(ns).Get(ctx, "claude-code-alice-edit", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("%s: %v", ns, err)
		}
		if rb.RoleRef.Name != "edit" || rb.Subjects[0].Name != "claude-code-alice" || rb.Subjects[0].Namespace != "kubeamber-agents" {
			t.Errorf("%s binding %+v", ns, rb)
		}
	}
	// Running it again is a no-op.
	if _, err := Ensure(ctx, cs, id); err != nil {
		t.Errorf("second Ensure: %v", err)
	}

	// Someone whose name slugs to the same thing cannot take it over.
	if _, err := Ensure(ctx, cs, Identity{Namespace: "kubeamber-agents", Agent: "claude-code", Owner: "ALICE"}); err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Errorf("takeover: %v", err)
	}
	if err := Revoke(ctx, cs, "kubeamber-agents", "claude-code-alice", "bob", false); err == nil {
		t.Error("bob revoked alice's agent without --force")
	}

	if err := Revoke(ctx, cs, "kubeamber-agents", "claude-code-alice", "alice", false); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().ServiceAccounts("kubeamber-agents").Get(ctx, "claude-code-alice", metav1.GetOptions{}); err == nil {
		t.Error("service account survived revoke")
	}
	if rbs, _ := cs.RbacV1().RoleBindings("").List(ctx, metav1.ListOptions{}); len(rbs.Items) != 0 {
		t.Errorf("%d role bindings survived revoke", len(rbs.Items))
	}
}

func TestClusterWide(t *testing.T) {
	cs := fake.NewClientset()
	ctx := context.Background()
	if _, err := Ensure(ctx, cs, Identity{Namespace: "kubeamber-agents", Agent: "holmes", Owner: "sre", Role: "view", ClusterWide: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, "holmes-sre-view", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Revoke(ctx, cs, "kubeamber-agents", "holmes-sre", "sre", false); err != nil {
		t.Fatal(err)
	}
	if crbs, _ := cs.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{}); len(crbs.Items) != 0 {
		t.Error("cluster role binding survived revoke")
	}
}

func TestTokenAndKubeconfig(t *testing.T) {
	cs := fake.NewClientset()
	ctx := context.Background()
	var asked int64
	cs.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenRequest)
		asked = *tr.Spec.ExpirationSeconds
		tr.Status = authnv1.TokenRequestStatus{Token: "tok-123", ExpirationTimestamp: metav1.NewTime(time.Unix(1_800_000_000, 0))}
		return true, tr, nil
	})
	token, exp, err := Token(ctx, cs, "kubeamber-agents", "claude-code-alice", 8*time.Hour)
	if err != nil || token != "tok-123" || asked != 8*3600 || exp.Unix() != 1_800_000_000 {
		t.Fatalf("token %q exp %v asked %d err %v", token, exp, asked, err)
	}

	data, err := Kubeconfig(&rest.Config{Host: "https://10.0.0.1:6443", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("CA")}}, "claude-code", "dev", token)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := clientcmd.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	c := kc.Contexts[kc.CurrentContext]
	if kc.Clusters[c.Cluster].Server != "https://10.0.0.1:6443" || string(kc.Clusters[c.Cluster].CertificateAuthorityData) != "CA" ||
		kc.AuthInfos[c.AuthInfo].Token != "tok-123" || c.Namespace != "dev" || kc.AuthInfos[c.AuthInfo].ClientCertificateData != nil {
		t.Errorf("kubeconfig %+v", kc)
	}
}

func TestOwners(t *testing.T) {
	cs := fake.NewClientset()
	ctx := context.Background()
	if _, err := Ensure(ctx, cs, Identity{Namespace: "kubeamber-agents", Agent: "claude-code", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	o := &Owners{Client: cs}
	if got := o.Owner(ctx, "system:serviceaccount:kubeamber-agents:claude-code-alice"); got != "alice" {
		t.Errorf("owner = %q", got)
	}
	for _, u := range []string{"alice", "system:serviceaccount:kubeamber-agents:missing", "system:serviceaccount:broken"} {
		if got := o.Owner(ctx, u); got != "" {
			t.Errorf("Owner(%q) = %q", u, got)
		}
	}
}
