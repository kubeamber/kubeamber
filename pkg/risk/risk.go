// Package risk scores a proposed Kubernetes change from 0 (harmless) to 100
// (dangerous). Scoring is deterministic and explainable: every point comes
// from a Factor with a sentence an operator, or an agent, can read.
package risk

import (
	"fmt"
	"sort"
	"strings"
)

// Change describes one request to the Kubernetes API.
type Change struct {
	// Operation is CREATE, UPDATE, DELETE or CONNECT.
	Operation string
	// Group and Kind identify the target resource (for subresources such as
	// deployments/scale, the parent: apps Deployment).
	Group string
	Kind  string
	// Resource and SubResource are the API path segments, e.g. "pods" and "exec".
	Resource    string
	SubResource string
	Namespace   string
	Name        string
	// Object and OldObject are the decoded request bodies. Either may be nil.
	Object    map[string]any
	OldObject map[string]any
}

// GroupKind returns "group/Kind", or "Kind" for the core group.
func (c Change) GroupKind() string {
	if c.Group == "" {
		return c.Kind
	}
	return c.Group + "/" + c.Kind
}

// Target is a short human-readable name for the changed object.
func (c Change) Target() string {
	name := c.Name
	if name == "" {
		name = "(new)"
	}
	t := c.Kind + " " + name
	if c.Namespace != "" {
		t = c.Kind + " " + c.Namespace + "/" + name
	}
	if c.SubResource != "" {
		t += " [" + c.SubResource + "]"
	}
	return t
}

// Factor is one contribution to the score.
type Factor struct {
	Points int    `json:"points"`
	Reason string `json:"reason"`
}

// Assessment is the scored result.
type Assessment struct {
	Score        int      `json:"score"`
	Factors      []Factor `json:"factors"`
	PodsAffected int      `json:"podsAffected,omitempty"`
	// Summary is a one-line description of what changes, e.g. "replicas 3 → 0".
	Summary string `json:"summary,omitempty"`
}

// Options carries the policy context the scorer needs.
type Options struct {
	// IsProtected reports whether a namespace is protected.
	IsProtected func(namespace string) bool
	// TrustedImage reports whether an image repository is trusted. Nil turns
	// the untrusted-image factor off.
	TrustedImage func(repository string) bool
	// RequireDigest scores images that are not pinned by digest.
	RequireDigest bool
}

// CriticalLabel marks an object as critical; changes to it score higher.
const CriticalLabel = "kubeamber.dev/critical"

// Assess scores a change.
func Assess(c Change, opts Options) Assessment {
	a := &Assessment{}
	operationFactors(a, c)
	kindFactors(a, c)
	workloadFactors(a, c)
	podSpecFactors(a, c)
	imageFactors(a, c, opts)
	scopeFactors(a, c, opts)

	for _, f := range a.Factors {
		a.Score += f.Points
	}
	a.Score = max(0, min(100, a.Score))
	return *a
}

func (a *Assessment) add(points int, format string, args ...any) {
	a.Factors = append(a.Factors, Factor{Points: points, Reason: fmt.Sprintf(format, args...)})
}

func (a *Assessment) summarize(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	if a.Summary == "" {
		a.Summary = s
	} else {
		a.Summary += "; " + s
	}
}

func operationFactors(a *Assessment, c Change) {
	switch c.Operation {
	case "CREATE":
		if c.SubResource == "eviction" {
			a.add(25, "evicts a pod")
			return
		}
		a.add(5, "creates a resource")
	case "UPDATE":
		a.add(10, "modifies a resource")
	case "DELETE":
		a.add(30, "deletes a resource")
	case "CONNECT":
		a.add(35, "opens an interactive session (%s) into a running workload", c.SubResource)
	}
}

func kindFactors(a *Assessment, c Change) {
	del := c.Operation == "DELETE"
	switch c.GroupKind() {
	case "Namespace":
		if del {
			a.add(50, "deletes namespace %q and everything in it", c.Name)
		} else {
			a.add(10, "changes a namespace")
		}
	case "apiextensions.k8s.io/CustomResourceDefinition":
		if del {
			a.add(60, "deletes a CRD and every custom resource of that type")
		} else {
			a.add(35, "changes a cluster-wide API definition")
		}
	case "rbac.authorization.k8s.io/ClusterRole", "rbac.authorization.k8s.io/ClusterRoleBinding":
		a.add(45, "changes cluster-wide permissions (possible privilege escalation)")
	case "rbac.authorization.k8s.io/Role", "rbac.authorization.k8s.io/RoleBinding":
		a.add(35, "changes namespace permissions (possible privilege escalation)")
	case "admissionregistration.k8s.io/ValidatingWebhookConfiguration",
		"admissionregistration.k8s.io/MutatingWebhookConfiguration",
		"admissionregistration.k8s.io/ValidatingAdmissionPolicy",
		"admissionregistration.k8s.io/ValidatingAdmissionPolicyBinding",
		"admissionregistration.k8s.io/MutatingAdmissionPolicy",
		"admissionregistration.k8s.io/MutatingAdmissionPolicyBinding":
		a.add(50, "changes admission control, which can switch off cluster safety checks")
	case "Secret":
		a.add(25, "touches a Secret")
	case "Node":
		a.add(40, "changes a node")
	case "PersistentVolume", "PersistentVolumeClaim":
		if del {
			a.add(45, "deletes storage (possible data loss)")
		} else {
			a.add(10, "changes storage")
		}
	case "networking.k8s.io/NetworkPolicy":
		a.add(20, "changes network isolation")
	case "Service", "networking.k8s.io/Ingress",
		"gateway.networking.k8s.io/Gateway", "gateway.networking.k8s.io/HTTPRoute",
		"gateway.networking.k8s.io/GRPCRoute":
		a.add(10, "changes how traffic is routed")
	case "apps/StatefulSet":
		a.add(15, "changes a stateful workload")
	case "apps/DaemonSet":
		a.add(15, "changes a workload that runs on every node")
	case "apps/Deployment", "apps/ReplicaSet", "batch/Job", "batch/CronJob":
		a.add(5, "changes a workload")
	case "ConfigMap":
		a.add(5, "changes configuration")
	case "ServiceAccount":
		if c.SubResource == "token" {
			a.add(45, "mints a credential for service account %s (whoever holds it acts as that identity)", c.Name)
		}
	case "Pod":
		if del && hasController(c.OldObject) {
			a.add(-20, "pod is managed by a controller and will be recreated")
		}
	}
}

func isScalable(c Change) bool {
	switch c.GroupKind() {
	case "apps/Deployment", "apps/StatefulSet", "apps/ReplicaSet":
		return true
	}
	return false
}

func workloadFactors(a *Assessment, c Change) {
	if !isScalable(c) {
		return
	}
	oldR, hasOld := replicas(c.OldObject)
	newR, hasNew := replicas(c.Object)

	switch c.Operation {
	case "DELETE":
		if hasOld {
			a.PodsAffected = oldR
			a.summarize("removes %d pod(s)", oldR)
		}
	case "UPDATE":
		if hasOld && hasNew && oldR != newR {
			a.summarize("replicas %d → %d", oldR, newR)
			switch {
			case newR == 0:
				a.add(35, "scales %s from %d to 0 replicas (takes it offline)", c.Name, oldR)
			case newR < oldR && (oldR-newR)*2 >= oldR:
				a.add(15, "cuts replicas from %d to %d", oldR, newR)
			}
			a.PodsAffected = max(oldR, newR)
		}
		if changed := imageChanges(c.OldObject, c.Object); len(changed) > 0 {
			a.add(10, "changes container image (%s)", strings.Join(changed, ", "))
			a.summarize("image %s", strings.Join(changed, ", "))
			if hasNew {
				a.PodsAffected = max(a.PodsAffected, newR)
			}
		}
	case "CREATE":
		if hasNew {
			a.PodsAffected = newR
		}
	}

	switch {
	case a.PodsAffected >= 50:
		a.add(20, "affects about %d pods", a.PodsAffected)
	case a.PodsAffected >= 10:
		a.add(10, "affects about %d pods", a.PodsAffected)
	}
}

// replicas reads spec.replicas from a workload or Scale object. A workload
// without the field defaults to 1, as the API server would.
func replicas(obj map[string]any) (int, bool) {
	if obj == nil {
		return 0, false
	}
	spec, _ := obj["spec"].(map[string]any)
	switch v := spec["replicas"].(type) {
	case float64:
		return int(v), true
	case int64:
		return int(v), true
	case int:
		return v, true
	}
	// Scale omits replicas when it is zero.
	if obj["kind"] == "Scale" {
		return 0, true
	}
	if spec == nil {
		return 0, false
	}
	return 1, true
}

// podSpecFactors flags host-level access requested by a new or changed pod spec.
func podSpecFactors(a *Assessment, c Change) {
	if c.Operation != "CREATE" && c.Operation != "UPDATE" {
		return
	}
	risky := hostAccess(podSpec(c.Object))
	if len(risky) == 0 {
		return
	}
	before := map[string]bool{}
	for _, r := range hostAccess(podSpec(c.OldObject)) {
		before[r] = true
	}
	var added []string
	for _, r := range risky {
		if !before[r] {
			added = append(added, r)
		}
	}
	if len(added) > 0 {
		a.add(40, "requests host-level access (%s)", strings.Join(added, ", "))
	}
}

func scopeFactors(a *Assessment, c Change, opts Options) {
	ns := c.Namespace
	if c.GroupKind() == "Namespace" {
		ns = c.Name
	}
	if opts.IsProtected != nil && ns != "" && opts.IsProtected(ns) {
		a.add(25, "namespace %q is protected", ns)
	}
	if c.Namespace == "" && c.GroupKind() != "Namespace" {
		a.add(10, "cluster-scoped resource (affects every namespace)")
	}
	if label(c.Object, CriticalLabel) == "true" || label(c.OldObject, CriticalLabel) == "true" {
		a.add(20, "object is labelled %s=true", CriticalLabel)
	}
}

// podSpec returns the pod spec of a Pod or of a workload's pod template.
func podSpec(obj map[string]any) map[string]any {
	if obj == nil {
		return nil
	}
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		return nil
	}
	if obj["kind"] == "Pod" {
		return spec
	}
	if jt, ok := spec["jobTemplate"].(map[string]any); ok { // CronJob
		spec, _ = jt["spec"].(map[string]any)
		if spec == nil {
			return nil
		}
	}
	tmpl, _ := spec["template"].(map[string]any)
	ps, _ := tmpl["spec"].(map[string]any)
	return ps
}

func hostAccess(ps map[string]any) []string {
	if ps == nil {
		return nil
	}
	var out []string
	for _, f := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if ps[f] == true {
			out = append(out, f)
		}
	}
	vols, _ := ps["volumes"].([]any)
	for _, v := range vols {
		if vm, _ := v.(map[string]any); vm != nil && vm["hostPath"] != nil {
			out = append(out, "hostPath")
			break
		}
	}
	for _, ctr := range containers(ps) {
		sc, _ := ctr["securityContext"].(map[string]any)
		if sc == nil {
			continue
		}
		if sc["privileged"] == true {
			out = append(out, "privileged")
		}
		caps, _ := sc["capabilities"].(map[string]any)
		add, _ := caps["add"].([]any)
		for _, c := range add {
			switch c {
			case "SYS_ADMIN", "NET_ADMIN", "ALL", "SYS_PTRACE":
				out = append(out, "CAP_"+c.(string))
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

func containers(ps map[string]any) []map[string]any {
	var out []map[string]any
	for _, key := range []string{"initContainers", "containers", "ephemeralContainers"} {
		list, _ := ps[key].([]any)
		for _, c := range list {
			if cm, ok := c.(map[string]any); ok {
				out = append(out, cm)
			}
		}
	}
	return out
}

// imageChanges lists "container: old → new" for containers whose image changed.
func imageChanges(oldObj, newObj map[string]any) []string {
	oldPS, newPS := podSpec(oldObj), podSpec(newObj)
	if oldPS == nil || newPS == nil {
		return nil
	}
	before := map[string]string{}
	for _, c := range containers(oldPS) {
		name, _ := c["name"].(string)
		before[name], _ = c["image"].(string)
	}
	var out []string
	for _, c := range containers(newPS) {
		name, _ := c["name"].(string)
		img, _ := c["image"].(string)
		if prev, ok := before[name]; ok && prev != img {
			out = append(out, fmt.Sprintf("%s: %s → %s", name, prev, img))
		}
	}
	return out
}

func hasController(obj map[string]any) bool {
	meta, _ := obj["metadata"].(map[string]any)
	refs, _ := meta["ownerReferences"].([]any)
	for _, r := range refs {
		if rm, _ := r.(map[string]any); rm != nil && rm["controller"] == true {
			return true
		}
	}
	return false
}

func label(obj map[string]any, key string) string {
	meta, _ := obj["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	v, _ := labels[key].(string)
	return v
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
