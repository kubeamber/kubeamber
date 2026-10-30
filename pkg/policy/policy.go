// Package policy defines the KubeAmber policy document: which identities are AI
// agents, and how the changes they make to a cluster are judged.
package policy

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	APIVersion = "kubeamber.dev/v1alpha1"
	Kind       = "Policy"
)

// Verdict is the outcome KubeAmber reaches for a change.
type Verdict string

const (
	Allow Verdict = "allow"
	Hold  Verdict = "hold"
	Deny  Verdict = "deny"
)

func (v Verdict) severity() int {
	switch v {
	case Deny:
		return 2
	case Hold:
		return 1
	default:
		return 0
	}
}

// Stricter returns whichever of a and b is more restrictive.
func Stricter(a, b Verdict) Verdict {
	if b.severity() > a.severity() {
		return b
	}
	return a
}

func (v Verdict) valid() bool { return v == Allow || v == Hold || v == Deny }

// Policy is the top-level document loaded from disk (usually a mounted ConfigMap).
type Policy struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`

	// Checksum identifies the exact bytes the policy was parsed from.
	Checksum string `json:"-"`
}

type Metadata struct {
	Name string `json:"name"`
}

type Spec struct {
	// Agents lists the identities KubeAmber treats as AI agents. Requests from
	// any other identity are always allowed without evaluation.
	Agents []Agent `json:"agents"`
	// Thresholds map a risk score to a verdict.
	Thresholds Thresholds `json:"thresholds,omitempty"`
	// ProtectedNamespaces are glob patterns for namespaces where changes
	// carry extra risk.
	ProtectedNamespaces []string `json:"protectedNamespaces,omitempty"`
	// Images adds where container images come from to the risk score. Off
	// unless set.
	Images Images `json:"images,omitempty"`
	// Rules are evaluated in order; the first match applies.
	Rules []Rule `json:"rules,omitempty"`
	// FreezeWindows hold or deny agent changes during set periods.
	FreezeWindows []FreezeWindow `json:"freezeWindows,omitempty"`
	Approvals     Approvals      `json:"approvals,omitempty"`
	Guards        Guards         `json:"guards,omitempty"`
}

// Agent is a named group of identities. Each list is optional; an identity
// matches the agent if it matches any entry in any list. Entries may use '*'
// as a wildcard.
type Agent struct {
	Name string `json:"name"`
	// Users are Kubernetes usernames, e.g. "agent:claude-code".
	Users []string `json:"users,omitempty"`
	// Groups are Kubernetes group names, e.g. "kubeamber:agents".
	Groups []string `json:"groups,omitempty"`
	// ServiceAccounts are "namespace/name" pairs; name may be "*".
	ServiceAccounts []string `json:"serviceAccounts,omitempty"`
}

// Images scores the images a change adds to a pod spec, so an agent that
// swaps in an image from an unknown source, or one a tag can silently move,
// needs a human.
type Images struct {
	// Trusted are glob patterns for image repositories, without tag or
	// digest, e.g. "cgr.dev/chainguard/*". Docker Hub short names are
	// expanded first ("nginx" is docker.io/library/nginx). Empty turns the
	// untrusted-image factor off.
	Trusted []string `json:"trusted,omitempty"`
	// RequireDigest scores images that are not pinned by digest.
	RequireDigest bool `json:"requireDigest,omitempty"`
}

type Thresholds struct {
	// Hold is the lowest risk score that requires human approval.
	Hold int `json:"hold,omitempty"`
	// Deny is the lowest risk score that is rejected outright. Set it to
	// 101 to never deny on score alone.
	Deny int `json:"deny,omitempty"`
}

// Rule pins a verdict for changes that match. An "allow" rule overrides the
// risk score but never overrides freeze windows, guards or built-in
// protections.
type Rule struct {
	Name   string  `json:"name"`
	Match  Match   `json:"match"`
	Action Verdict `json:"action"`
	Reason string  `json:"reason,omitempty"`
}

// Match selects changes. Empty fields match everything.
type Match struct {
	Agents     []string `json:"agents,omitempty"`
	Operations []string `json:"operations,omitempty"`
	Kinds      []string `json:"kinds,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
	Names      []string `json:"names,omitempty"`
}

// FreezeWindow is either a weekly recurring window (Days, Start, End) or a
// one-off window (From, Until).
type FreezeWindow struct {
	Name       string       `json:"name"`
	Days       []string     `json:"days,omitempty"`
	Start      string       `json:"start,omitempty"`
	End        string       `json:"end,omitempty"`
	From       *metav1.Time `json:"from,omitempty"`
	Until      *metav1.Time `json:"until,omitempty"`
	TimeZone   string       `json:"timeZone,omitempty"`
	Namespaces []string     `json:"namespaces,omitempty"`
	Action     Verdict      `json:"action,omitempty"`

	loc *time.Location
}

type Approvals struct {
	// TTL is how long an approval stays valid for the agent to retry.
	TTL metav1.Duration `json:"ttl,omitempty"`
	// PendingTTL is how long a held change waits for a human before it
	// expires. A change linked to a ticket waits until the ticket's planned
	// window closes instead.
	PendingTTL metav1.Duration `json:"pendingTTL,omitempty"`
	// RequireExternal makes the change-management system (through
	// `kubeamber bridge`) the only place a held change can be approved.
	// Rejecting in the cluster still works, because that fails safe.
	RequireExternal bool `json:"requireExternal,omitempty"`
	// AllowOwnerApproval lets the person who launched an agent (its
	// kubeamber.dev/owner) approve that agent's held changes. Off by default:
	// approving your own agent's change is approving your own change.
	AllowOwnerApproval bool `json:"allowOwnerApproval,omitempty"`
}

type Guards struct {
	Prometheus *PrometheusGuard `json:"prometheus,omitempty"`
}

// PrometheusGuard escalates agent changes while a PromQL query returns
// results, for example while an SLO is burning its error budget.
type PrometheusGuard struct {
	URL     string            `json:"url"`
	Timeout metav1.Duration   `json:"timeout,omitempty"`
	OnError Verdict           `json:"onError,omitempty"`
	Checks  []PrometheusCheck `json:"checks"`
}

type PrometheusCheck struct {
	Name       string   `json:"name"`
	Query      string   `json:"query"`
	Namespaces []string `json:"namespaces,omitempty"`
	Action     Verdict  `json:"action,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// Load reads and validates a policy file.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Parse decodes, defaults and validates a policy document. Unknown fields are
// rejected so that typos fail loudly instead of silently weakening the policy.
func Parse(data []byte) (*Policy, error) {
	var p Policy
	if err := yaml.UnmarshalStrict(data, &p); err != nil {
		return nil, err
	}
	p.Checksum = fmt.Sprintf("%x", sha256.Sum256(data))
	p.applyDefaults()
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Policy) applyDefaults() {
	s := &p.Spec
	if s.Thresholds.Hold == 0 {
		s.Thresholds.Hold = 30
	}
	if s.Thresholds.Deny == 0 {
		s.Thresholds.Deny = 80
	}
	if s.Approvals.TTL.Duration == 0 {
		s.Approvals.TTL.Duration = 15 * time.Minute
	}
	if s.Approvals.PendingTTL.Duration == 0 {
		s.Approvals.PendingTTL.Duration = time.Hour
	}
	for i := range s.FreezeWindows {
		if s.FreezeWindows[i].Action == "" {
			s.FreezeWindows[i].Action = Hold
		}
	}
	if g := s.Guards.Prometheus; g != nil {
		if g.Timeout.Duration == 0 {
			g.Timeout.Duration = 2 * time.Second
		}
		if g.OnError == "" {
			g.OnError = Hold
		}
		for i := range g.Checks {
			if g.Checks[i].Action == "" {
				g.Checks[i].Action = Hold
			}
		}
	}
}

var operations = map[string]bool{"CREATE": true, "UPDATE": true, "DELETE": true, "CONNECT": true}

func (p *Policy) validate() error {
	if p.APIVersion != APIVersion || p.Kind != Kind {
		return fmt.Errorf("expected apiVersion %q and kind %q, got %q %q", APIVersion, Kind, p.APIVersion, p.Kind)
	}
	s := &p.Spec
	if len(s.Agents) == 0 {
		return fmt.Errorf("spec.agents: at least one agent is required")
	}
	seen := map[string]bool{}
	for i, a := range s.Agents {
		if a.Name == "" {
			return fmt.Errorf("spec.agents[%d]: name is required", i)
		}
		if seen[a.Name] {
			return fmt.Errorf("spec.agents[%d]: duplicate agent name %q", i, a.Name)
		}
		seen[a.Name] = true
		if len(a.Users)+len(a.Groups)+len(a.ServiceAccounts) == 0 {
			return fmt.Errorf("spec.agents[%d] (%s): needs at least one of users, groups or serviceAccounts", i, a.Name)
		}
		for _, sa := range a.ServiceAccounts {
			if ns, name, ok := strings.Cut(sa, "/"); !ok || ns == "" || name == "" {
				return fmt.Errorf("spec.agents[%d] (%s): service account %q must be namespace/name", i, a.Name, sa)
			}
		}
	}
	t := s.Thresholds
	if t.Hold < 1 || t.Hold > 100 || t.Deny < t.Hold || t.Deny > 101 {
		return fmt.Errorf("spec.thresholds: need 1 <= hold (%d) <= deny (%d) <= 101", t.Hold, t.Deny)
	}
	for i, repo := range s.Images.Trusted {
		last := repo[strings.LastIndex(repo, "/")+1:]
		if repo == "" || strings.Contains(repo, "@") || strings.Contains(last, ":") {
			return fmt.Errorf("spec.images.trusted[%d]: %q must be an image repository without tag or digest, e.g. cgr.dev/chainguard/*", i, repo)
		}
	}
	for i, r := range s.Rules {
		if r.Name == "" {
			return fmt.Errorf("spec.rules[%d]: name is required", i)
		}
		if !r.Action.valid() {
			return fmt.Errorf("spec.rules[%d] (%s): action must be allow, hold or deny", i, r.Name)
		}
		for _, op := range r.Match.Operations {
			if !operations[op] {
				return fmt.Errorf("spec.rules[%d] (%s): unknown operation %q", i, r.Name, op)
			}
		}
		for _, a := range r.Match.Agents {
			if !seen[a] {
				return fmt.Errorf("spec.rules[%d] (%s): unknown agent %q", i, r.Name, a)
			}
		}
	}
	for i := range s.FreezeWindows {
		if err := s.FreezeWindows[i].init(); err != nil {
			return fmt.Errorf("spec.freezeWindows[%d] (%s): %w", i, s.FreezeWindows[i].Name, err)
		}
	}
	if s.Approvals.TTL.Duration < 0 || s.Approvals.PendingTTL.Duration < 0 {
		return fmt.Errorf("spec.approvals: durations must be positive")
	}
	if g := s.Guards.Prometheus; g != nil {
		if g.URL == "" {
			return fmt.Errorf("spec.guards.prometheus.url is required")
		}
		if g.OnError != Hold && g.OnError != Deny && g.OnError != Allow {
			return fmt.Errorf("spec.guards.prometheus.onError must be allow, hold or deny")
		}
		if len(g.Checks) == 0 {
			return fmt.Errorf("spec.guards.prometheus.checks: at least one check is required")
		}
		for i, c := range g.Checks {
			if c.Name == "" || c.Query == "" {
				return fmt.Errorf("spec.guards.prometheus.checks[%d]: name and query are required", i)
			}
			if c.Action != Hold && c.Action != Deny {
				return fmt.Errorf("spec.guards.prometheus.checks[%d] (%s): action must be hold or deny", i, c.Name)
			}
		}
	}
	return nil
}

// MatchAgent returns the agent profile that a Kubernetes identity belongs to,
// or nil if the identity is not an agent.
func (p *Policy) MatchAgent(username string, groups []string) *Agent {
	for i := range p.Spec.Agents {
		if p.Spec.Agents[i].matches(username, groups) {
			return &p.Spec.Agents[i]
		}
	}
	return nil
}

// Agent returns the named agent profile, or nil.
func (p *Policy) Agent(name string) *Agent {
	for i := range p.Spec.Agents {
		if p.Spec.Agents[i].Name == name {
			return &p.Spec.Agents[i]
		}
	}
	return nil
}

func (a *Agent) matches(username string, groups []string) bool {
	for _, u := range a.Users {
		if Glob(u, username) {
			return true
		}
	}
	for _, sa := range a.ServiceAccounts {
		if Glob(ServiceAccountUsername(sa), username) {
			return true
		}
	}
	for _, want := range a.Groups {
		for _, g := range groups {
			if Glob(want, g) {
				return true
			}
		}
	}
	return false
}

// ServiceAccountUsername converts "namespace/name" to the username the API
// server assigns to that service account.
func ServiceAccountUsername(sa string) string {
	ns, name, _ := strings.Cut(sa, "/")
	return "system:serviceaccount:" + ns + ":" + name
}

// IsProtectedNamespace reports whether ns matches spec.protectedNamespaces.
func (p *Policy) IsProtectedNamespace(ns string) bool {
	return ns != "" && GlobAny(p.Spec.ProtectedNamespaces, ns)
}

// TrustedImage reports whether an image repository matches spec.images.trusted.
func (p *Policy) TrustedImage(repository string) bool {
	return GlobAny(p.Spec.Images.Trusted, repository)
}

// Matches reports whether the rule selects a change.
func (m Match) Matches(agent, operation, kind, namespace, name string) bool {
	return listMatches(m.Agents, agent) &&
		listMatches(m.Operations, operation) &&
		listMatches(m.Kinds, kind) &&
		listMatches(m.Namespaces, namespace) &&
		listMatches(m.Names, name)
}

func listMatches(patterns []string, s string) bool {
	return len(patterns) == 0 || GlobAny(patterns, s)
}
