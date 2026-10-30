package policy

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
apiVersion: kubeamber.dev/v1alpha1
kind: Policy
metadata: {name: test}
spec:
  agents:
    - name: bot
      serviceAccounts: ["agents/*"]
`

func TestLoadExample(t *testing.T) {
	p, err := Load("../../examples/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.MatchAgent("system:serviceaccount:kubeamber-agents:sre-agent", nil); got == nil || got.Name != "sre-agent" {
		t.Errorf("service account did not match sre-agent: %v", got)
	}
	if got := p.MatchAgent("alice", []string{"kubeamber:agents"}); got == nil || got.Name != "workstation-agents" {
		t.Errorf("group did not match workstation-agents: %v", got)
	}
	if p.MatchAgent("alice", []string{"system:authenticated"}) != nil {
		t.Error("a human matched an agent profile")
	}
}

func TestTrustedImage(t *testing.T) {
	p, err := Parse([]byte(minimal + "  images: {trusted: ['cgr.dev/chainguard/*', 'localhost:5000/*'], requireDigest: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for repo, want := range map[string]bool{
		"cgr.dev/chainguard/nginx": true,
		"localhost:5000/app":       true,
		"cgr.dev/someone-else/x":   false,
		"docker.io/library/nginx":  false,
	} {
		if got := p.TrustedImage(repo); got != want {
			t.Errorf("TrustedImage(%q) = %v, want %v", repo, got, want)
		}
	}
	if !p.Spec.Images.RequireDigest {
		t.Error("requireDigest not parsed")
	}
}

func TestDefaults(t *testing.T) {
	p, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	s := p.Spec
	if s.Thresholds.Hold != 30 || s.Thresholds.Deny != 80 {
		t.Errorf("thresholds = %+v", s.Thresholds)
	}
	if s.Approvals.TTL.Duration != 15*time.Minute || s.Approvals.PendingTTL.Duration != time.Hour {
		t.Errorf("approvals = %+v", s.Approvals)
	}
	if p.Checksum == "" {
		t.Error("checksum not set")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"unknown field":        minimal + "  thresholdz: {}\n",
		"wrong kind":           strings.Replace(minimal, "kind: Policy", "kind: Polcy", 1),
		"no agents":            "apiVersion: kubeamber.dev/v1alpha1\nkind: Policy\nspec: {agents: []}\n",
		"bad service account":  strings.Replace(minimal, "agents/*", "agents", 1),
		"bad thresholds":       minimal + "  thresholds: {hold: 90, deny: 50}\n",
		"bad rule action":      minimal + "  rules: [{name: r, action: maybe}]\n",
		"unknown rule agent":   minimal + "  rules: [{name: r, action: deny, match: {agents: [ghost]}}]\n",
		"bad operation":        minimal + "  rules: [{name: r, action: deny, match: {operations: [PATCH]}}]\n",
		"bad freeze day":       minimal + "  freezeWindows: [{name: f, days: [Funday], start: '10:00', end: '11:00'}]\n",
		"bad freeze time":      minimal + "  freezeWindows: [{name: f, start: '25:00', end: '11:00'}]\n",
		"mixed freeze":         minimal + "  freezeWindows: [{name: f, start: '10:00', end: '11:00', from: '2026-01-01T00:00:00Z', until: '2026-01-02T00:00:00Z'}]\n",
		"bad time zone":        minimal + "  freezeWindows: [{name: f, start: '10:00', end: '11:00', timeZone: Mars/Base}]\n",
		"guard without url":    minimal + "  guards: {prometheus: {checks: [{name: c, query: up}]}}\n",
		"trusted image tag":    minimal + "  images: {trusted: ['cgr.dev/chainguard/nginx:latest']}\n",
		"trusted image digest": minimal + "  images: {trusted: ['cgr.dev/chainguard/nginx@sha256:fc42af']}\n",
		"empty trusted image":  minimal + "  images: {trusted: ['']}\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"prod", "prod", true},
		{"prod", "prod-eu", false},
		{"prod-*", "prod-eu", true},
		{"prod-*", "prod", false},
		{"*", "", true},
		{"system:serviceaccount:agents:*", "system:serviceaccount:agents:holmes", true},
		{"system:serviceaccount:agents:*", "system:serviceaccount:agents-x:holmes", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "acb", false},
		{"ab*b", "ab", false},
		{"*/admin", "https://idp.example.com/admin", true},
	}
	for _, c := range cases {
		if got := Glob(c.pattern, c.s); got != c.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
	if got := GlobRegexp("prod.*-*"); got != `^prod\..*-.*$` {
		t.Errorf("GlobRegexp = %s", got)
	}
}

func TestFreezeWindows(t *testing.T) {
	doc := minimal + `  freezeWindows:
    - name: fri-evening
      days: [Fri]
      start: "17:00"
      end: "24:00"
      timeZone: Asia/Kolkata
      namespaces: ["prod*"]
    - name: overnight
      days: [Sat]
      start: "22:00"
      end: "06:00"
    - name: launch
      from: "2026-11-01T00:00:00Z"
      until: "2026-11-02T00:00:00Z"
      action: deny
`
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	fri, night, launch := &p.Spec.FreezeWindows[0], &p.Spec.FreezeWindows[1], &p.Spec.FreezeWindows[2]
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		w    *FreezeWindow
		at   string
		ns   string
		want bool
	}{
		{fri, "2026-10-23T12:00:00Z", "prod", true},  // Fri 17:30 IST
		{fri, "2026-10-23T11:00:00Z", "prod", false}, // Fri 16:30 IST
		{fri, "2026-10-23T12:00:00Z", "shop", false}, // wrong namespace
		{fri, "2026-10-23T18:29:00Z", "prod", true},  // Fri 23:59 IST
		{fri, "2026-10-23T18:31:00Z", "prod", false}, // Sat 00:01 IST
		{night, "2026-10-24T23:00:00Z", "", true},    // Sat 23:00
		{night, "2026-10-25T05:00:00Z", "", true},    // Sun 05:00, still Saturday night
		{night, "2026-10-25T07:00:00Z", "", false},   // Sun 07:00
		{night, "2026-10-24T05:00:00Z", "", false},   // Sat 05:00 belongs to Friday night
		{launch, "2026-11-01T10:00:00Z", "x", true},
		{launch, "2026-11-02T00:00:00Z", "x", false},
	}
	for _, c := range cases {
		if got := c.w.Active(at(c.at), c.ns); got != c.want {
			t.Errorf("%s at %s in %q: active = %v, want %v", c.w.Name, c.at, c.ns, got, c.want)
		}
	}
	if launch.Action != Deny || fri.Action != Hold {
		t.Errorf("actions: %s %s", launch.Action, fri.Action)
	}
}

func TestStricter(t *testing.T) {
	if Stricter(Allow, Hold) != Hold || Stricter(Deny, Hold) != Deny || Stricter(Allow, Allow) != Allow {
		t.Error("Stricter ordering is wrong")
	}
}
