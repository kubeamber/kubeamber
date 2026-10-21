// Package decide turns a proposed change into a verdict (allow, hold or deny)
// by combining built-in self-protection, policy rules, the risk score, freeze
// windows and live guards. The strictest outcome wins.
package decide

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kubeamber/kubeamber/pkg/policy"
	"github.com/kubeamber/kubeamber/pkg/risk"
)

// Self identifies KubeAmber's own installation so agents cannot tamper with it.
type Self struct {
	Namespace   string
	WebhookName string
}

// GuardResult is a guard that fired for a change.
type GuardResult struct {
	Name    string
	Verdict policy.Verdict
	Reason  string
}

// Guard evaluates live signals such as SLO burn rate.
type Guard interface {
	Evaluate(ctx context.Context, cfg *policy.PrometheusGuard, namespace string) []GuardResult
}

// Request is a change attributed to an identity.
type Request struct {
	User   string
	Groups []string
	// Agent evaluates the change as the named agent profile, bypassing
	// identity matching. Used for offline checks.
	Agent  string
	Change risk.Change
	Now    time.Time
}

// Decision explains a verdict.
type Decision struct {
	Verdict      policy.Verdict    `json:"verdict"`
	Agent        string            `json:"agent,omitempty"`
	Score        int               `json:"score"`
	Thresholds   policy.Thresholds `json:"thresholds"`
	Reasons      []string          `json:"reasons"`
	Factors      []risk.Factor     `json:"factors,omitempty"`
	Summary      string            `json:"summary,omitempty"`
	PodsAffected int               `json:"podsAffected,omitempty"`
	Rule         string            `json:"rule,omitempty"`
}

// IsAgent reports whether the request came from an agent identity.
func (d Decision) IsAgent() bool { return d.Agent != "" }

func (d *Decision) escalate(v policy.Verdict, reason string) {
	d.Verdict = policy.Stricter(d.Verdict, v)
	d.Reasons = append(d.Reasons, reason)
}

// SelfProtectionRule names the built-in rule that guards KubeAmber itself.
const SelfProtectionRule = "builtin:self-protection"

// Engine evaluates requests against the current policy.
type Engine struct {
	// Policy returns the policy in force. It is called once per request so
	// that a reloaded policy takes effect immediately.
	Policy func() *policy.Policy
	// Guard is optional; without it, spec.guards is ignored.
	Guard Guard
	Self  Self
}

// Evaluate decides a single request.
func (e *Engine) Evaluate(ctx context.Context, r Request) Decision {
	p := e.Policy()
	var agent *policy.Agent
	if r.Agent != "" {
		agent = p.Agent(r.Agent)
	} else {
		agent = p.MatchAgent(r.User, r.Groups)
	}
	if agent == nil {
		return Decision{Verdict: policy.Allow, Reasons: []string{"not an agent identity"}}
	}

	c := r.Change
	d := Decision{Verdict: policy.Allow, Agent: agent.Name, Thresholds: p.Spec.Thresholds}
	if reason := e.selfProtection(c); reason != "" {
		d.Rule = SelfProtectionRule
		d.escalate(policy.Deny, reason)
		return d
	}

	a := risk.Assess(c, risk.Options{IsProtected: p.IsProtectedNamespace})
	d.Score, d.Factors, d.Summary, d.PodsAffected = a.Score, a.Factors, a.Summary, a.PodsAffected
	ns := effectiveNamespace(c)

	scoreGoverns := true
	for _, rule := range p.Spec.Rules {
		if !rule.Match.Matches(agent.Name, c.Operation, c.Kind, ns, c.Name) {
			continue
		}
		d.Rule = rule.Name
		reason := rule.Reason
		if reason == "" {
			reason = fmt.Sprintf("matched rule %q", rule.Name)
		}
		if rule.Action == policy.Allow {
			scoreGoverns = false
			d.Reasons = append(d.Reasons, reason)
		} else {
			d.escalate(rule.Action, reason)
		}
		break
	}

	t := p.Spec.Thresholds
	if scoreGoverns {
		var v policy.Verdict
		var reason string
		switch {
		case d.Score >= t.Deny:
			v, reason = policy.Deny, fmt.Sprintf("risk %d/100 is at or above the deny threshold (%d)", d.Score, t.Deny)
		case d.Score >= t.Hold:
			v, reason = policy.Hold, fmt.Sprintf("risk %d/100 is at or above the hold threshold (%d)", d.Score, t.Hold)
		}
		// A rule that already reached this verdict or a stricter one explains it.
		if v != "" && (d.Rule == "" || policy.Stricter(d.Verdict, v) != d.Verdict) {
			d.escalate(v, reason)
		}
	}

	now := r.Now
	if now.IsZero() {
		now = time.Now()
	}
	for i := range p.Spec.FreezeWindows {
		if w := &p.Spec.FreezeWindows[i]; w.Active(now, ns) {
			d.escalate(w.Action, fmt.Sprintf("change freeze %q is in effect", w.Name))
		}
	}

	if g := p.Spec.Guards.Prometheus; g != nil && e.Guard != nil && d.Verdict != policy.Deny {
		for _, res := range e.Guard.Evaluate(ctx, g, ns) {
			d.escalate(res.Verdict, res.Reason)
		}
	}

	if d.Verdict == policy.Allow && scoreGoverns {
		d.Reasons = append(d.Reasons, fmt.Sprintf("risk %d/100 is below the hold threshold (%d)", d.Score, t.Hold))
	}
	return d
}

// selfProtection returns a reason if the change targets KubeAmber itself.
// These checks cannot be disabled by policy.
func (e *Engine) selfProtection(c risk.Change) string {
	switch {
	case c.Group == "kubeamber.dev":
		return "agents cannot create, approve, modify or delete KubeAmber resources"
	case e.Self.Namespace != "" && effectiveNamespace(c) == e.Self.Namespace:
		return fmt.Sprintf("agents cannot modify KubeAmber's namespace %q", e.Self.Namespace)
	case c.Group == "admissionregistration.k8s.io" && (c.Name == e.Self.WebhookName || strings.HasPrefix(c.Name, "kubeamber-")):
		return "agents cannot modify KubeAmber's admission configuration"
	case c.GroupKind() == "apiextensions.k8s.io/CustomResourceDefinition" && strings.HasSuffix(c.Name, ".kubeamber.dev"):
		return "agents cannot modify KubeAmber's API definitions"
	case c.Group == "rbac.authorization.k8s.io" && (c.Name == "kubeamber" || strings.HasPrefix(c.Name, "kubeamber-")):
		return "agents cannot modify KubeAmber's permissions"
	case c.GroupKind() == "ServiceAccount" && c.SubResource == "" && annotation(c.Object, OwnerAnnotation) != annotation(c.OldObject, OwnerAnnotation):
		return "agents cannot create, change or delete agent identities owned by a person (" + OwnerAnnotation + ")"
	}
	return ""
}

// OwnerAnnotation records, on an agent's service account, the person who
// launched the agent (set by `kubeamber agent-kubeconfig`).
const OwnerAnnotation = "kubeamber.dev/owner"

func annotation(obj map[string]any, key string) string {
	meta, _ := obj["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	v, _ := ann[key].(string)
	return v
}

// effectiveNamespace is the namespace a change lands in; for a Namespace
// object it is the namespace itself.
func effectiveNamespace(c risk.Change) string {
	if c.GroupKind() == "Namespace" {
		return c.Name
	}
	return c.Namespace
}

// Message renders a decision as a single line an agent can act on. It is
// what kubectl prints when KubeAmber denies or holds a request.
func Message(d Decision) string {
	if d.Rule == SelfProtectionRule {
		return fmt.Sprintf("kubeamber: %s: %s", strings.ToUpper(string(d.Verdict)), strings.Join(d.Reasons, "; "))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "kubeamber: %s (risk %d/100", strings.ToUpper(string(d.Verdict)), d.Score)
	if d.Summary != "" {
		fmt.Fprintf(&b, ", %s", d.Summary)
	}
	b.WriteString("): ")
	b.WriteString(strings.Join(d.Reasons, "; "))
	if len(d.Factors) > 0 {
		b.WriteString(". Factors: ")
		parts := make([]string, 0, len(d.Factors))
		for _, f := range d.Factors {
			parts = append(parts, fmt.Sprintf("%+d %s", f.Points, f.Reason))
		}
		b.WriteString(strings.Join(parts, ", "))
	}
	return b.String()
}
