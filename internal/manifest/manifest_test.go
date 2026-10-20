package manifest

import "testing"

func TestParse(t *testing.T) {
	objs, err := Parse([]byte(`
apiVersion: v1
kind: ConfigMap
metadata: {name: a}
---
---
apiVersion: v1
kind: List
items:
  - {apiVersion: v1, kind: Secret, metadata: {name: b}}
  - {apiVersion: v1, kind: Service, metadata: {name: c}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 || objs[2]["kind"] != "Service" {
		t.Fatalf("got %d objects: %v", len(objs), objs)
	}
}

func TestToChange(t *testing.T) {
	deploy := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "web"}}
	c, err := ToChange(deploy, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Operation != "CREATE" || c.Group != "apps" || c.Namespace != "shop" || c.Resource != "deployments" || c.Object == nil {
		t.Errorf("create: %+v", c)
	}

	c, _ = ToChange(deploy, Options{})
	if c.Namespace != "default" {
		t.Errorf("namespaced object without namespace landed in %q", c.Namespace)
	}

	c, _ = ToChange(deploy, Options{Old: deploy})
	if c.Operation != "UPDATE" || c.OldObject == nil {
		t.Errorf("with old: %+v", c)
	}

	c, _ = ToChange(deploy, Options{Operation: "delete"})
	if c.Operation != "DELETE" || c.Object != nil || c.OldObject == nil {
		t.Errorf("delete should move the manifest to OldObject: %+v", c)
	}

	ns := map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "prod", "namespace": "ignored"}}
	c, _ = ToChange(ns, Options{Namespace: "shop"})
	if c.Namespace != "" || c.Group != "" {
		t.Errorf("cluster-scoped: %+v", c)
	}

	if _, err := ToChange(map[string]any{"kind": "Pod"}, Options{}); err == nil {
		t.Error("missing apiVersion accepted")
	}
}

func TestPlural(t *testing.T) {
	for kind, want := range map[string]string{
		"Deployment": "deployments", "NetworkPolicy": "networkpolicies", "Ingress": "ingresses",
		"Gateway": "gateways", "Endpoints": "endpoints", "StorageClass": "storageclasses",
	} {
		if got := Plural(kind); got != want {
			t.Errorf("Plural(%s) = %s, want %s", kind, got, want)
		}
	}
}
