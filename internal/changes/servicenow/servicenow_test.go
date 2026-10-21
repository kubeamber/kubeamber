package servicenow

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/internal/changes"
	"github.com/kubeamber/kubeamber/internal/changes/servicenow/fake"
)

func secretFile(t *testing.T, name, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func setup(t *testing.T, auth Auth) (*Client, *fake.Server) {
	t.Helper()
	snow := fake.New("kubeamber.integration", "s3cret", "kubeamber", "client-s3cret")
	srv := httptest.NewServer(snow.Handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{
		InstanceURL: srv.URL,
		Auth:        auth,
		Window:      metav1.Duration{Duration: 4 * time.Hour},
		Fields:      map[string]string{"assignment_group": "platform-oncall"},
		ConfigurationItems: map[string]string{
			"prod*": "ci-prod-cluster",
			"*":     "ci-default",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC) }
	return c, snow
}

func basicAuth(t *testing.T) Auth {
	return Auth{Type: "basic", UsernameFile: secretFile(t, "user", "kubeamber.integration"), PasswordFile: secretFile(t, "pass", "s3cret")}
}

func heldCR() *changerequest.ChangeRequest {
	return &changerequest.ChangeRequest{Name: "cr-1920026c-r7kch", Spec: changerequest.Spec{
		Agent: "sre-agent", User: "system:serviceaccount:kubeamber-agents:sre-agent", Operation: "UPDATE",
		Target:  changerequest.Target{Resource: "deployments", Kind: "Deployment", Namespace: "prod", Name: "checkout"},
		Score:   75,
		Summary: "replicas 3 → 0",
		Factors: []string{"+35 scales checkout from 3 to 0 replicas (takes it offline)", "+25 namespace \"prod\" is protected"},
		Reasons: []string{"risk 75/100 is at or above the hold threshold (30)"},
		Intent:  "checkout pods are crash-looping; stop them while I investigate", Fingerprint: "abc123",
	}}
}

func TestOpenCreatesOneTicket(t *testing.T) {
	c, snow := setup(t, basicAuth(t))
	ctx := context.Background()
	ext, err := c.Open(ctx, heldCR())
	if err != nil {
		t.Fatal(err)
	}
	if ext.System != "servicenow" || ext.Number != "CHG0030001" || ext.State != "New" || !strings.Contains(ext.URL, "/change_request.do?sys_id=") {
		t.Fatalf("external = %+v", ext)
	}
	if !ext.PlannedStart.Time.Equal(c.now()) || !ext.PlannedEnd.Time.Equal(c.now().Add(4*time.Hour)) {
		t.Errorf("window %v - %v", ext.PlannedStart, ext.PlannedEnd)
	}
	f := snow.Get("CHG0030001").Fields
	checks := map[string]string{
		"type": "normal", "correlation_id": "cr-1920026c-r7kch", "correlation_display": "KubeAmber",
		"cmdb_ci": "ci-prod-cluster", "assignment_group": "platform-oncall",
		"start_date": "2026-10-21 10:00:00", "end_date": "2026-10-21 14:00:00",
		"short_description": "AI agent sre-agent: UPDATE Deployment prod/checkout (replicas 3 → 0)",
	}
	for k, want := range checks {
		if f[k] != want {
			t.Errorf("%s = %q, want %q", k, f[k], want)
		}
	}
	for _, want := range []string{"Risk score: 75/100", "+35 scales checkout", "crash-looping", "abc123", "exactly this change, once"} {
		if !strings.Contains(f["description"], want) {
			t.Errorf("description lacks %q:\n%s", want, f["description"])
		}
	}

	// A restart re-opening the same change request finds the existing ticket.
	again, err := c.Open(ctx, heldCR())
	if err != nil {
		t.Fatal(err)
	}
	if again.Number != ext.Number || snow.Count() != 1 {
		t.Errorf("second Open made %d tickets (got %s)", snow.Count(), again.Number)
	}
}

func TestSyncDecisions(t *testing.T) {
	c, snow := setup(t, basicAuth(t))
	ctx := context.Background()
	ext, err := c.Open(ctx, heldCR())
	if err != nil {
		t.Fatal(err)
	}

	tk, err := c.Sync(ctx, *ext)
	if err != nil || tk.Decision != "" || tk.State != "New" {
		t.Fatalf("undecided: %+v, %v", tk, err)
	}

	if err := snow.Decide(ext.Number, "jane.cab", "approved"); err != nil {
		t.Fatal(err)
	}
	tk, err = c.Sync(ctx, *ext)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Decision != changerequest.DecisionApproved || tk.State != "Scheduled" || len(tk.DecidedBy) != 1 || tk.DecidedBy[0] != "jane.cab" {
		t.Errorf("approved: %+v", tk)
	}
	if tk.PlannedEnd == nil || !tk.PlannedEnd.Equal(c.now().Add(4*time.Hour)) {
		t.Errorf("window end %v", tk.PlannedEnd)
	}

	ext2, _ := c.Open(ctx, &changerequest.ChangeRequest{Name: "cr-other", Spec: heldCR().Spec})
	if err := snow.Decide(ext2.Number, "bob.cab", "rejected"); err != nil {
		t.Fatal(err)
	}
	tk, _ = c.Sync(ctx, *ext2)
	if tk.Decision != changerequest.DecisionRejected || tk.DecidedBy[0] != "bob.cab" || !strings.Contains(tk.Reason, "rejected") {
		t.Errorf("rejected: %+v", tk)
	}
}

func TestClose(t *testing.T) {
	c, snow := setup(t, basicAuth(t))
	ctx := context.Background()
	ext, _ := c.Open(ctx, heldCR())
	_ = snow.Decide(ext.Number, "jane.cab", "approved")
	if err := c.Close(ctx, *ext, changes.Implemented, "applied by sre-agent"); err != nil {
		t.Fatal(err)
	}
	got := snow.Get(ext.Number)
	if got.Fields["state"] != "0" || got.Fields["close_code"] != "successful" || got.Fields["close_notes"] != "applied by sre-agent" || len(got.WorkNotes) != 1 {
		t.Errorf("implemented: %+v notes %q", got.Fields, got.WorkNotes)
	}

	ext2, _ := c.Open(ctx, &changerequest.ChangeRequest{Name: "cr-expired", Spec: heldCR().Spec})
	if err := c.Close(ctx, *ext2, changes.Abandoned, "expired"); err != nil {
		t.Fatal(err)
	}
	if got := snow.Get(ext2.Number); got.Fields["state"] != "4" || got.WorkNotes[0] != "expired" {
		t.Errorf("abandoned: %+v", got.Fields)
	}
}

func TestOAuth(t *testing.T) {
	auth := Auth{Type: "oauth", ClientIDFile: secretFile(t, "id", "kubeamber"), ClientSecretFile: secretFile(t, "secret", "client-s3cret")}
	c, snow := setup(t, auth)
	ctx := context.Background()
	if _, err := c.Open(ctx, heldCR()); err != nil {
		t.Fatal(err)
	}
	first := c.token
	if first == "" {
		t.Fatal("no token cached")
	}
	if _, err := c.Open(ctx, heldCR()); err != nil || c.token != first {
		t.Errorf("token was not reused (err %v)", err)
	}
	if len(snow.Requests) < 3 {
		t.Errorf("requests: %v", snow.Requests)
	}
}

func TestAuthErrors(t *testing.T) {
	bad := Auth{Type: "basic", UsernameFile: secretFile(t, "user", "kubeamber.integration"), PasswordFile: secretFile(t, "pass", "wrong")}
	c, _ := setup(t, bad)
	_, err := c.Open(context.Background(), heldCR())
	if err == nil || !strings.Contains(err.Error(), "User Not Authenticated") {
		t.Errorf("err = %v", err)
	}

	missing := Auth{Type: "basic", UsernameFile: "/nonexistent/user", PasswordFile: "/nonexistent/pass"}
	c, _ = setup(t, missing)
	if _, err := c.Open(context.Background(), heldCR()); err == nil || !strings.Contains(err.Error(), "reading credential") {
		t.Errorf("err = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	ok := Auth{Type: "basic", UsernameFile: "u", PasswordFile: "p"}
	cases := map[string]Config{
		"no url":         {Auth: ok},
		"bad scheme":     {InstanceURL: "ftp://x", Auth: ok},
		"bad type":       {InstanceURL: "https://x.service-now.com", Auth: ok, ChangeType: "standard"},
		"no auth":        {InstanceURL: "https://x.service-now.com"},
		"oauth no files": {InstanceURL: "https://x.service-now.com", Auth: Auth{Type: "oauth"}},
		"bad close":      {InstanceURL: "https://x.service-now.com", Auth: ok, CloseState: "done"},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestField(t *testing.T) {
	r := map[string]any{"a": "x", "b": map[string]any{"value": "y", "display_value": "Y"}, "c": float64(3)}
	if field(r, "a") != "x" || field(r, "b") != "y" || field(r, "c") != "3" || field(r, "missing") != "" {
		t.Error("field did not read both value shapes")
	}
}
