package fingerprint

import (
	"testing"

	"github.com/kubeamber/kubeamber/pkg/risk"
)

func object(replicas float64, rv, restartedAt string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{
			"name": "checkout", "namespace": "prod", "resourceVersion": rv, "generation": float64(7),
			"annotations": map[string]any{"kubectl.kubernetes.io/last-applied-configuration": rv},
		},
		"spec": map[string]any{
			"replicas": replicas,
			"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{
				"kubectl.kubernetes.io/restartedAt": restartedAt,
			}}},
		},
		"status": map[string]any{"readyReplicas": replicas},
	}
}

func change(obj map[string]any) risk.Change {
	return risk.Change{Operation: "UPDATE", Group: "apps", Kind: "Deployment", Resource: "deployments",
		Namespace: "prod", Name: "checkout", Object: obj}
}

func TestStableAcrossVolatileFields(t *testing.T) {
	a := Of("bot", change(object(0, "100", "2026-10-21T10:00:00Z")))
	b := Of("bot", change(object(0, "205", "2026-10-21T10:05:00Z")))
	if a != b {
		t.Error("resourceVersion, last-applied or restartedAt changed the fingerprint")
	}
	if len(a) != 64 {
		t.Errorf("fingerprint %q is not hex sha256", a)
	}
}

func TestDistinguishesChanges(t *testing.T) {
	base := Of("bot", change(object(0, "1", "")))
	others := map[string]string{
		"content":  Of("bot", change(object(1, "1", ""))),
		"identity": Of("other-bot", change(object(0, "1", ""))),
	}
	del := change(object(0, "1", ""))
	del.Operation = "DELETE"
	others["operation"] = Of("bot", del)
	for name, fp := range others {
		if fp == base {
			t.Errorf("a different %s produced the same fingerprint", name)
		}
	}
}

func TestDoesNotMutateInput(t *testing.T) {
	obj := object(0, "1", "x")
	Of("bot", change(obj))
	if obj["status"] == nil || obj["metadata"].(map[string]any)["resourceVersion"] != "1" {
		t.Error("Of modified the object it was given")
	}
}
