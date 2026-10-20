// Package manifest turns Kubernetes manifests into changes that can be
// scored offline (by `kubeamber check`) or previewed by an agent over MCP.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/kubeamber/kubeamber/pkg/risk"
)

// Parse decodes one or more YAML or JSON documents. Empty documents are
// skipped and `kind: List` is expanded into its items.
func Parse(data []byte) ([]map[string]any, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []map[string]any
	for {
		var obj map[string]any
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(obj) == 0 {
			continue
		}
		if kind, _ := obj["kind"].(string); strings.HasSuffix(kind, "List") {
			items, _ := obj["items"].([]any)
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					out = append(out, m)
				}
			}
			continue
		}
		out = append(out, obj)
	}
	return out, nil
}

// Options describe how a manifest is being applied.
type Options struct {
	// Operation is CREATE, UPDATE, DELETE or CONNECT; empty means CREATE, or
	// UPDATE when Old is set.
	Operation   string
	SubResource string
	// Namespace is used for namespaced objects that do not set one.
	Namespace string
	// Old is the current object, if known.
	Old map[string]any
	// Resource overrides the guessed plural resource name.
	Resource string
	// Namespaced overrides the built-in table of cluster-scoped kinds.
	Namespaced *bool
}

// ToChange builds the change that applying obj would make.
func ToChange(obj map[string]any, o Options) (risk.Change, error) {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	if apiVersion == "" || kind == "" {
		return risk.Change{}, fmt.Errorf("manifest needs apiVersion and kind")
	}
	group := ""
	if g, _, ok := strings.Cut(apiVersion, "/"); ok {
		group = g
	}
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	ns, _ := meta["namespace"].(string)

	namespaced := !risk.IsClusterScoped(group, kind)
	if o.Namespaced != nil {
		namespaced = *o.Namespaced
	}
	switch {
	case !namespaced:
		ns = ""
	case ns == "" && o.Namespace != "":
		ns = o.Namespace
	case ns == "":
		ns = "default"
	}

	op := strings.ToUpper(o.Operation)
	if op == "" {
		op = "CREATE"
		if o.Old != nil {
			op = "UPDATE"
		}
	}
	resource := o.Resource
	if resource == "" {
		resource = Plural(kind)
	}
	c := risk.Change{
		Operation: op, Group: group, Kind: kind, Resource: resource, SubResource: o.SubResource,
		Namespace: ns, Name: name, Object: obj, OldObject: o.Old,
	}
	if op == "DELETE" || op == "CONNECT" {
		// For a delete the manifest describes what is being removed.
		c.Object = nil
		if c.OldObject == nil && op == "DELETE" {
			c.OldObject = obj
		}
	}
	return c, nil
}

// Plural guesses the resource name for a kind, e.g. NetworkPolicy -> networkpolicies.
func Plural(kind string) string {
	k := strings.ToLower(kind)
	switch {
	case k == "endpoints":
		return k
	case strings.HasSuffix(k, "y") && !strings.HasSuffix(k, "ay") && !strings.HasSuffix(k, "ey"):
		return k[:len(k)-1] + "ies"
	case strings.HasSuffix(k, "s"), strings.HasSuffix(k, "x"), strings.HasSuffix(k, "ch"), strings.HasSuffix(k, "sh"):
		return k + "es"
	}
	return k + "s"
}
