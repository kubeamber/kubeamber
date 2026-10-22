package risk

import (
	"strings"
	"testing"
)

func deployment(ns string, replicas int, image string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "checkout", "namespace": ns},
		"spec": map[string]any{
			"replicas": float64(replicas),
			"template": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "app", "image": image}},
			}},
		},
	}
}

func protected(ns string) bool { return ns == "prod" || ns == "kube-system" }

// podWithEphemeral is a Pod, with a debug container added if debug is set.
func podWithEphemeral(debug, privileged bool) map[string]any {
	spec := map[string]any{"containers": []any{map[string]any{"name": "app", "image": "shop:1"}}}
	if debug {
		c := map[string]any{"name": "debugger", "image": "busybox"}
		if privileged {
			c["securityContext"] = map[string]any{"privileged": true}
		}
		spec["ephemeralContainers"] = []any{c}
	}
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "p"}, "spec": spec}
}

func TestAssess(t *testing.T) {
	cases := []struct {
		name       string
		change     Change
		want       int
		wantReason string
	}{
		{
			name:   "scale up in a normal namespace",
			change: Change{Operation: "UPDATE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", OldObject: deployment("shop", 3, "v1"), Object: deployment("shop", 4, "v1")},
			want:   15,
		},
		{
			name:       "scale to zero in prod",
			change:     Change{Operation: "UPDATE", Group: "apps", Kind: "Deployment", Namespace: "prod", Name: "checkout", OldObject: deployment("prod", 3, "v1"), Object: deployment("prod", 0, "v1")},
			want:       75,
			wantReason: "takes it offline",
		},
		{
			name: "scale subresource to zero",
			change: Change{Operation: "UPDATE", Group: "apps", Kind: "Deployment", Resource: "deployments", SubResource: "scale", Namespace: "shop", Name: "checkout",
				OldObject: map[string]any{"kind": "Scale", "spec": map[string]any{"replicas": float64(5)}},
				Object:    map[string]any{"kind": "Scale", "spec": map[string]any{}}},
			want:       50,
			wantReason: "from 5 to 0",
		},
		{
			name:       "halve replicas",
			change:     Change{Operation: "UPDATE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", OldObject: deployment("shop", 10, "v1"), Object: deployment("shop", 4, "v1")},
			want:       40,
			wantReason: "cuts replicas",
		},
		{
			name:       "image change in prod",
			change:     Change{Operation: "UPDATE", Group: "apps", Kind: "Deployment", Namespace: "prod", Name: "checkout", OldObject: deployment("prod", 3, "v1"), Object: deployment("prod", 3, "v2")},
			want:       50,
			wantReason: "app: v1 → v2",
		},
		{
			name:       "delete a namespace",
			change:     Change{Operation: "DELETE", Kind: "Namespace", Name: "shop"},
			want:       80,
			wantReason: "everything in it",
		},
		{
			name:   "delete a protected namespace caps at 100",
			change: Change{Operation: "DELETE", Kind: "Namespace", Name: "prod"},
			want:   100,
		},
		{
			name: "delete a managed pod",
			change: Change{Operation: "DELETE", Kind: "Pod", Namespace: "shop", Name: "checkout-abc",
				OldObject: map[string]any{"metadata": map[string]any{"ownerReferences": []any{map[string]any{"controller": true}}}}},
			want:       10,
			wantReason: "will be recreated",
		},
		{
			name:   "delete a bare pod",
			change: Change{Operation: "DELETE", Kind: "Pod", Namespace: "shop", Name: "debug"},
			want:   30,
		},
		{
			name:       "exec into prod",
			change:     Change{Operation: "CONNECT", Kind: "Pod", Resource: "pods", SubResource: "exec", Namespace: "prod", Name: "checkout-abc"},
			want:       60,
			wantReason: "interactive session (exec)",
		},
		{
			name:   "delete a PVC",
			change: Change{Operation: "DELETE", Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data"},
			want:   75,
		},
		{
			name:   "create a cluster role binding",
			change: Change{Operation: "CREATE", Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding", Name: "x"},
			want:   60,
		},
		{
			name:   "delete an admission webhook",
			change: Change{Operation: "DELETE", Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration", Name: "kyverno"},
			want:   90,
		},
		{
			name: "privileged pod",
			change: Change{Operation: "CREATE", Kind: "Pod", Namespace: "shop", Name: "p",
				Object: map[string]any{"kind": "Pod", "spec": map[string]any{
					"hostNetwork": true,
					"containers":  []any{map[string]any{"name": "c", "securityContext": map[string]any{"privileged": true}}},
				}}},
			want:       45,
			wantReason: "hostNetwork, privileged",
		},
		{
			name: "host access that was already there scores nothing extra",
			change: Change{Operation: "UPDATE", Kind: "Pod", Namespace: "shop", Name: "p",
				OldObject: map[string]any{"kind": "Pod", "spec": map[string]any{"hostPID": true}},
				Object:    map[string]any{"kind": "Pod", "spec": map[string]any{"hostPID": true}}},
			want: 10,
		},
		{
			name: "critical label",
			change: Change{Operation: "UPDATE", Kind: "ConfigMap", Namespace: "shop", Name: "c",
				Object: map[string]any{"metadata": map[string]any{"labels": map[string]any{CriticalLabel: "true"}}}},
			want: 35,
		},
		{
			name:       "large blast radius",
			change:     Change{Operation: "DELETE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "web", OldObject: deployment("shop", 60, "v1")},
			want:       55,
			wantReason: "about 60 pods",
		},
		{
			name:       "mint a token for another service account",
			change:     Change{Operation: "CREATE", Kind: "ServiceAccount", Resource: "serviceaccounts", SubResource: "token", Namespace: "shop", Name: "deployer"},
			want:       50,
			wantReason: "acts as that identity",
		},
		{
			name:       "repoint an API group",
			change:     Change{Operation: "UPDATE", Group: "apiregistration.k8s.io", Kind: "APIService", Name: "v1beta1.metrics.k8s.io"},
			want:       70,
			wantReason: "which server answers an API group",
		},
		{
			name:   "evict a pod",
			change: Change{Operation: "CREATE", Kind: "Pod", Resource: "pods", SubResource: "eviction", Namespace: "shop", Name: "p"},
			want:   25,
		},
		{
			name: "debug container in a prod pod",
			change: Change{Operation: "UPDATE", Kind: "Pod", Resource: "pods", SubResource: "ephemeralcontainers", Namespace: "prod", Name: "p",
				OldObject: podWithEphemeral(false, false), Object: podWithEphemeral(true, false)},
			want:       70,
			wantReason: "ephemeral container",
		},
		{
			name: "privileged debug container",
			change: Change{Operation: "UPDATE", Kind: "Pod", Resource: "pods", SubResource: "ephemeralcontainers", Namespace: "shop", Name: "p",
				OldObject: podWithEphemeral(false, false), Object: podWithEphemeral(true, true)},
			want:       85,
			wantReason: "privileged",
		},
		{
			name:       "proxy into a pod",
			change:     Change{Operation: "CONNECT", Kind: "Pod", Resource: "pods", SubResource: "proxy", Namespace: "shop", Name: "p"},
			want:       35,
			wantReason: "(proxy)",
		},
		{
			name:   "proxy into a prod service",
			change: Change{Operation: "CONNECT", Kind: "Service", Resource: "services", SubResource: "proxy", Namespace: "prod", Name: "checkout"},
			want:   60,
		},
		{
			name:       "proxy into a node's kubelet",
			change:     Change{Operation: "CONNECT", Kind: "Node", Resource: "nodes", SubResource: "proxy", Name: "node-1"},
			want:       85,
			wantReason: "kubelet",
		},
		{
			name:       "request a client certificate",
			change:     Change{Operation: "CREATE", Group: "certificates.k8s.io", Kind: "CertificateSigningRequest", Resource: "certificatesigningrequests", Name: "csr-1"},
			want:       40,
			wantReason: "client certificate",
		},
		{
			name: "approve a certificate request",
			change: Change{Operation: "UPDATE", Group: "certificates.k8s.io", Kind: "CertificateSigningRequest", Resource: "certificatesigningrequests",
				SubResource: "approval", Name: "csr-1"},
			want:       65,
			wantReason: "acts as its identity",
		},
		{
			name:       "remove a namespace's finalizers",
			change:     Change{Operation: "UPDATE", Kind: "Namespace", Resource: "namespaces", SubResource: "finalize", Name: "shop"},
			want:       50,
			wantReason: "force-deletes",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Assess(c.change, Options{IsProtected: protected})
			if a.Score != c.want {
				t.Errorf("score = %d, want %d; factors: %+v", a.Score, c.want, a.Factors)
			}
			if c.wantReason != "" && !hasReason(a, c.wantReason) {
				t.Errorf("no factor mentions %q: %+v", c.wantReason, a.Factors)
			}
		})
	}
}

func hasReason(a Assessment, s string) bool {
	for _, f := range a.Factors {
		if strings.Contains(f.Reason, s) {
			return true
		}
	}
	return false
}

func TestSummaryAndPods(t *testing.T) {
	a := Assess(Change{Operation: "UPDATE", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout",
		OldObject: deployment("shop", 3, "v1"), Object: deployment("shop", 0, "v1")}, Options{})
	if a.Summary != "replicas 3 → 0" || a.PodsAffected != 3 {
		t.Errorf("summary %q pods %d", a.Summary, a.PodsAffected)
	}
}

func TestParentKind(t *testing.T) {
	if g, k := ParentKind("deployments", "scale", "autoscaling", "Scale"); g != "apps" || k != "Deployment" {
		t.Errorf("deployments/scale -> %s %s", g, k)
	}
	if g, k := ParentKind("pods", "exec", "", "PodExecOptions"); g != "" || k != "Pod" {
		t.Errorf("pods/exec -> %s %s", g, k)
	}
	if g, k := ParentKind("widgets", "", "example.com", "Widget"); g != "example.com" || k != "Widget" {
		t.Errorf("widgets -> %s %s", g, k)
	}
	for res, want := range map[string][2]string{
		"nodes":                      {"", "Node"},
		"services":                   {"", "Service"},
		"namespaces":                 {"", "Namespace"},
		"certificatesigningrequests": {"certificates.k8s.io", "CertificateSigningRequest"},
	} {
		if g, k := ParentKind(res, "proxy", "", "SomeOptions"); g != want[0] || k != want[1] {
			t.Errorf("%s subresource -> %s %s, want %v", res, g, k, want)
		}
	}
}

func TestTarget(t *testing.T) {
	c := Change{Kind: "Pod", Namespace: "shop", Name: "p", SubResource: "exec"}
	if got := c.Target(); got != "Pod shop/p [exec]" {
		t.Errorf("Target = %q", got)
	}
}
