// Package fingerprint identifies a change so that a human approval applies to
// exactly that change and nothing else. Two requests share a fingerprint only
// if the same identity makes the same operation on the same object with the
// same content.
package fingerprint

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/kubeamber/kubeamber/pkg/risk"
)

// volatileMetadata changes between otherwise identical requests.
var volatileMetadata = []string{
	"resourceVersion", "uid", "generation", "creationTimestamp", "managedFields",
	"selfLink", "deletionTimestamp", "deletionGracePeriodSeconds",
}

// volatileAnnotations are dropped wherever they appear.
var volatileAnnotations = map[string]bool{
	"kubectl.kubernetes.io/last-applied-configuration": true,
	"deployment.kubernetes.io/revision":                true,
}

// wildcardAnnotations keep their presence but not their value, so a retried
// `kubectl rollout restart` (which stamps the pod template) matches the
// approved one.
var wildcardAnnotations = map[string]bool{"kubectl.kubernetes.io/restartedAt": true}

// Of returns a hex SHA-256 fingerprint.
func Of(user string, c risk.Change) string {
	key := struct {
		User        string `json:"user"`
		Operation   string `json:"op"`
		Group       string `json:"group"`
		Kind        string `json:"kind"`
		Resource    string `json:"resource"`
		SubResource string `json:"subResource"`
		Namespace   string `json:"namespace"`
		Name        string `json:"name"`
		Object      any    `json:"object,omitempty"`
	}{user, c.Operation, c.Group, c.Kind, c.Resource, c.SubResource, c.Namespace, c.Name, nil}
	if c.Operation != "DELETE" && c.Object != nil {
		key.Object = normalize(c.Object)
	}
	// encoding/json sorts map keys, so the encoding is canonical.
	data, err := json.Marshal(key)
	if err != nil {
		panic(fmt.Sprintf("fingerprint: %v", err)) // decoded JSON always re-encodes
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func normalize(obj map[string]any) map[string]any {
	out := scrub(obj).(map[string]any)
	delete(out, "status")
	if meta, ok := out["metadata"].(map[string]any); ok {
		for _, k := range volatileMetadata {
			delete(meta, k)
		}
	}
	return out
}

// scrub deep-copies decoded JSON, dropping or wildcarding volatile annotations.
func scrub(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			switch {
			case volatileAnnotations[k]:
			case wildcardAnnotations[k]:
				out[k] = "*"
			default:
				out[k] = scrub(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = scrub(val)
		}
		return out
	}
	return v
}
