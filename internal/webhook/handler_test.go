package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/pkg/decide"
	"github.com/kubeamber/kubeamber/pkg/policy"
)

const testPolicy = `
apiVersion: kubeamber.dev/v1alpha1
kind: Policy
metadata: {name: test}
spec:
  agents:
    - name: bot
      serviceAccounts: ["agents/*"]
  protectedNamespaces: [prod]
`

const (
	agentUser = "system:serviceaccount:agents:bot"
	selfUser  = "system:serviceaccount:kubeamber-system:kubeamber"
)

// memStore is an in-memory change request store.
type memStore struct {
	crs map[string]*changerequest.ChangeRequest
	n   int
}

func (m *memStore) FindByFingerprint(_ context.Context, fp string) ([]*changerequest.ChangeRequest, error) {
	var out []*changerequest.ChangeRequest
	for _, cr := range m.crs {
		if cr.Spec.Fingerprint == fp {
			c := *cr
			out = append(out, &c)
		}
	}
	return out, nil
}

func (m *memStore) Create(_ context.Context, cr *changerequest.ChangeRequest) (*changerequest.ChangeRequest, error) {
	m.n++
	c := *cr
	c.Name = fmt.Sprintf("cr-%d", m.n)
	c.Status.Phase = changerequest.Pending
	m.crs[c.Name] = &c
	out := c
	return &out, nil
}

func (m *memStore) SetPhase(_ context.Context, cr *changerequest.ChangeRequest, phase changerequest.Phase, now time.Time) error {
	stored := m.crs[cr.Name]
	if stored.Status.Phase != cr.Status.Phase {
		return changerequest.ErrAlreadyUsed
	}
	stored.Status.Phase = phase
	return nil
}

func (m *memStore) approve(name, by string, at time.Time) {
	cr := m.crs[name]
	cr.Spec.Approval = &changerequest.Approval{Decision: changerequest.DecisionApproved, By: by, Reason: "ok", At: metav1.NewTime(at)}
	cr.Status.Phase = changerequest.Approved
}

type fixture struct {
	h     *Handler
	store *memStore
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	p, err := policy.Parse([]byte(testPolicy))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: &memStore{crs: map[string]*changerequest.ChangeRequest{}}, now: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)}
	get := func() *policy.Policy { return p }
	f.h = NewHandler(Handler{
		Engine:   &decide.Engine{Policy: get, Self: decide.Self{Namespace: "kubeamber-system", WebhookName: "kubeamber"}},
		Store:    f.store,
		Policy:   get,
		SelfUser: selfUser,
		Now:      func() time.Time { return f.now },
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return f
}

func deploymentJSON(replicas int) runtime.RawExtension {
	obj := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "checkout", "namespace": "prod", "resourceVersion": fmt.Sprint(time.Now().UnixNano()),
			"annotations": map[string]any{IntentAnnotation: "stop the crash loop"}},
		"spec": map[string]any{"replicas": replicas},
	}
	data, _ := json.Marshal(obj)
	return runtime.RawExtension{Raw: data}
}

func scaleToZero(user string) *admissionv1.AdmissionRequest {
	return &admissionv1.AdmissionRequest{
		UID:       types.UID("uid-1"),
		Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
		Resource:  metav1.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Namespace: "prod", Name: "checkout", Operation: admissionv1.Update,
		UserInfo:  authnv1.UserInfo{Username: user, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:agents"}},
		Object:    deploymentJSON(0),
		OldObject: deploymentJSON(3),
	}
}

func TestHumansPassThrough(t *testing.T) {
	f := newFixture(t)
	resp := f.h.Review(context.Background(), scaleToZero("alice"))
	if !resp.Allowed || len(resp.Warnings) != 0 || len(f.store.crs) != 0 {
		t.Fatalf("human request was not passed through untouched: %+v", resp)
	}
}

func TestLowRiskAllowedWithWarning(t *testing.T) {
	f := newFixture(t)
	req := scaleToZero(agentUser)
	req.Namespace = "shop"
	req.Object = deploymentJSON(4)
	resp := f.h.Review(context.Background(), req)
	if !resp.Allowed || len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "ALLOW") {
		t.Fatalf("want allow with warning, got %+v", resp)
	}
	if resp.AuditAnnotations["verdict"] != "allow" || resp.AuditAnnotations["agent"] != "bot" {
		t.Errorf("audit annotations: %v", resp.AuditAnnotations)
	}
}

func TestHoldApproveRetry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 1. The agent's change is held and a change request is raised.
	resp := f.h.Review(ctx, scaleToZero(agentUser))
	if resp.Allowed || len(f.store.crs) != 1 {
		t.Fatalf("expected a hold with one change request, got allowed=%v crs=%d", resp.Allowed, len(f.store.crs))
	}
	cr := f.store.crs["cr-1"]
	if cr.Spec.Intent != "stop the crash loop" || cr.Spec.Agent != "bot" || cr.Spec.Score != 75 {
		t.Errorf("change request spec: %+v", cr.Spec)
	}
	for _, want := range []string{"HOLD", "cr-1", "kubeamber approve cr-1", "replicas 3 → 0"} {
		if !strings.Contains(resp.Result.Message, want) {
			t.Errorf("message %q does not contain %q", resp.Result.Message, want)
		}
	}
	if resp.Result.Code != http.StatusForbidden || resp.AuditAnnotations["change-request"] != "cr-1" {
		t.Errorf("code %d annotations %v", resp.Result.Code, resp.AuditAnnotations)
	}

	// 2. Retrying before approval does not raise a second request.
	resp = f.h.Review(ctx, scaleToZero(agentUser))
	if resp.Allowed || len(f.store.crs) != 1 || !strings.Contains(resp.Result.Message, "still waiting") {
		t.Fatalf("retry before approval: allowed=%v crs=%d msg=%q", resp.Allowed, len(f.store.crs), resp.Result.Message)
	}

	// 3. A human approves; the identical retry goes through exactly once.
	f.store.approve("cr-1", "alice", f.now)
	resp = f.h.Review(ctx, scaleToZero(agentUser))
	if !resp.Allowed || !strings.Contains(resp.Warnings[0], "approved by alice") {
		t.Fatalf("retry after approval: %+v", resp)
	}
	if f.store.crs["cr-1"].Status.Phase != changerequest.Consumed || resp.AuditAnnotations["verdict"] != "approved" {
		t.Errorf("phase %s, annotations %v", f.store.crs["cr-1"].Status.Phase, resp.AuditAnnotations)
	}

	// 4. The approval cannot be reused.
	resp = f.h.Review(ctx, scaleToZero(agentUser))
	if resp.Allowed || len(f.store.crs) != 2 {
		t.Fatalf("approval was reused: allowed=%v crs=%d", resp.Allowed, len(f.store.crs))
	}
}

func TestApprovalDoesNotCoverADifferentChange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.h.Review(ctx, scaleToZero(agentUser))
	f.store.approve("cr-1", "alice", f.now)

	other := scaleToZero(agentUser)
	other.Name = "payments"
	if resp := f.h.Review(ctx, other); resp.Allowed {
		t.Fatal("approval for checkout let payments through")
	}
}

func TestApprovalExpires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.h.Review(ctx, scaleToZero(agentUser))
	f.store.approve("cr-1", "alice", f.now)
	f.now = f.now.Add(16 * time.Minute) // approvals last 15m by default
	if resp := f.h.Review(ctx, scaleToZero(agentUser)); resp.Allowed {
		t.Fatal("an expired approval was honoured")
	}
}

func TestRejectionSticks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.h.Review(ctx, scaleToZero(agentUser))
	cr := f.store.crs["cr-1"]
	cr.Spec.Approval = &changerequest.Approval{Decision: changerequest.DecisionRejected, By: "alice", Reason: "scale it down instead", At: metav1.NewTime(f.now)}
	cr.Status.Phase = changerequest.Rejected

	resp := f.h.Review(ctx, scaleToZero(agentUser))
	if resp.Allowed || !strings.Contains(resp.Result.Message, "scale it down instead") || len(f.store.crs) != 1 {
		t.Fatalf("rejected change: allowed=%v crs=%d msg=%q", resp.Allowed, len(f.store.crs), resp.Result.Message)
	}
}

func TestDryRunRaisesNothing(t *testing.T) {
	f := newFixture(t)
	req := scaleToZero(agentUser)
	dry := true
	req.DryRun = &dry
	resp := f.h.Review(context.Background(), req)
	if resp.Allowed || len(f.store.crs) != 0 || !strings.Contains(resp.Result.Message, "dry run") {
		t.Fatalf("dry run: allowed=%v crs=%d", resp.Allowed, len(f.store.crs))
	}
}

func TestDenyIsFinal(t *testing.T) {
	f := newFixture(t)
	req := &admissionv1.AdmissionRequest{
		Kind:     metav1.GroupVersionKind{Version: "v1", Kind: "Namespace"},
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "namespaces"},
		Name:     "prod", Operation: admissionv1.Delete,
		UserInfo: authnv1.UserInfo{Username: agentUser},
	}
	resp := f.h.Review(context.Background(), req)
	if resp.Allowed || len(f.store.crs) != 0 || !strings.Contains(resp.Result.Message, "DENY") {
		t.Fatalf("namespace delete: %+v", resp)
	}
}

// crUpdate builds an admission request for a human editing a change request.
func crUpdate(user string, oldCR, newCR map[string]any) *admissionv1.AdmissionRequest {
	enc := func(m map[string]any) runtime.RawExtension {
		data, _ := json.Marshal(m)
		return runtime.RawExtension{Raw: data}
	}
	gvr := changerequest.GVR
	return &admissionv1.AdmissionRequest{
		Kind:     metav1.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "ChangeRequest"},
		Resource: metav1.GroupVersionResource{Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource},
		Name:     "cr-1", Operation: admissionv1.Update,
		UserInfo:  authnv1.UserInfo{Username: user},
		OldObject: enc(oldCR), Object: enc(newCR),
	}
}

func pendingCR(expires time.Time) map[string]any {
	return map[string]any{
		"apiVersion": "kubeamber.dev/v1alpha1", "kind": "ChangeRequest",
		"metadata": map[string]any{"name": "cr-1"},
		"spec": map[string]any{
			"agent": "bot", "user": agentUser, "operation": "UPDATE", "score": 75, "fingerprint": "abc",
			"target":    map[string]any{"resource": "deployments", "kind": "Deployment", "namespace": "prod", "name": "checkout"},
			"expiresAt": expires.Format(time.RFC3339),
		},
		"status": map[string]any{"phase": "Pending"},
	}
}

func decided(cr map[string]any, decision, by string, at time.Time, phase string) map[string]any {
	data, _ := json.Marshal(cr)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	out["spec"].(map[string]any)["approval"] = map[string]any{"decision": decision, "by": by, "reason": "r", "at": at.Format(time.RFC3339)}
	out["status"] = map[string]any{"phase": phase}
	return out
}

func TestChangeRequestValidation(t *testing.T) {
	f := newFixture(t)
	now := f.now
	old := pendingCR(now.Add(time.Hour))
	tampered := decided(old, "Approved", "alice", now, "Approved")
	tampered["spec"].(map[string]any)["score"] = 1
	cases := []struct {
		name      string
		req       *admissionv1.AdmissionRequest
		allowed   bool
		wantInMsg string
	}{
		{"human approves", crUpdate("alice", old, decided(old, "Approved", "alice", now, "Approved")), true, ""},
		{"human rejects", crUpdate("alice", old, decided(old, "Rejected", "alice", now, "Rejected")), true, ""},
		{"agent approves", crUpdate(agentUser, old, decided(old, "Approved", agentUser, now, "Approved")), false, "agents cannot"},
		{"self-approval", crUpdate("bob", pendingCRWithUser(now, "bob"), decided(pendingCRWithUser(now, "bob"), "Approved", "bob", now, "Approved")), false, "cannot approve it"},
		{"approve as someone else", crUpdate("alice", old, decided(old, "Approved", "carol", now, "Approved")), false, "your own identity"},
		{"tamper with spec", crUpdate("alice", old, tampered), false, "only spec.approval"},
		{"phase mismatch", crUpdate("alice", old, decided(old, "Approved", "alice", now, "Rejected")), false, "status.phase must be Approved"},
		{"bad decision", crUpdate("alice", old, decided(old, "Maybe", "alice", now, "Approved")), false, "Approved or Rejected"},
		{"post-dated approval", crUpdate("alice", old, decided(old, "Approved", "alice", now.Add(time.Hour), "Approved")), false, "current time"},
		{"expired", crUpdate("alice", pendingCR(now.Add(-time.Minute)), decided(pendingCR(now.Add(-time.Minute)), "Approved", "alice", now, "Approved")), false, "expired"},
		{"decide twice", crUpdate("carol", decided(old, "Approved", "alice", now, "Approved"), decided(old, "Rejected", "carol", now, "Rejected")), false, "already decided"},
		{"phase only", crUpdate("alice", old, func() map[string]any {
			m := pendingCR(now.Add(time.Hour))
			m["status"] = map[string]any{"phase": "Approved"}
			return m
		}()), false, "only when a decision"},
		{"label edit", crUpdate("alice", old, pendingCR(now.Add(time.Hour))), true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := f.h.Review(context.Background(), c.req)
			if resp.Allowed != c.allowed {
				msg := ""
				if resp.Result != nil {
					msg = resp.Result.Message
				}
				t.Fatalf("allowed = %v, want %v (%s)", resp.Allowed, c.allowed, msg)
			}
			if c.wantInMsg != "" && !strings.Contains(resp.Result.Message, c.wantInMsg) {
				t.Errorf("message %q does not contain %q", resp.Result.Message, c.wantInMsg)
			}
		})
	}
}

func pendingCRWithUser(now time.Time, user string) map[string]any {
	cr := pendingCR(now.Add(time.Hour))
	cr["spec"].(map[string]any)["user"] = user
	return cr
}

func TestChangeRequestCreateAndDelete(t *testing.T) {
	f := newFixture(t)
	gvr := metav1.GroupVersionResource{Group: "kubeamber.dev", Version: "v1alpha1", Resource: "changerequests"}
	create := &admissionv1.AdmissionRequest{Resource: gvr, Operation: admissionv1.Create, UserInfo: authnv1.UserInfo{Username: "alice"}}
	if f.h.Review(context.Background(), create).Allowed {
		t.Error("a human created a change request")
	}
	create.UserInfo.Username = selfUser
	if !f.h.Review(context.Background(), create).Allowed {
		t.Error("KubeAmber could not create its own change request")
	}
	del := &admissionv1.AdmissionRequest{Resource: gvr, Operation: admissionv1.Delete, UserInfo: authnv1.UserInfo{Username: "alice"}}
	if !f.h.Review(context.Background(), del).Allowed {
		t.Error("a human could not clean up a change request")
	}
}

func TestServeHTTP(t *testing.T) {
	f := newFixture(t)
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request:  scaleToZero(agentUser),
	}
	body, _ := json.Marshal(review)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(body)))
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Response == nil || out.Response.UID != "uid-1" || out.Response.Allowed || out.Kind != "AdmissionReview" {
		t.Fatalf("bad review response: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/validate", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET returned %d", rec.Code)
	}
}

func TestToChangeUsesParentKind(t *testing.T) {
	req := &admissionv1.AdmissionRequest{
		Kind:        metav1.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "Scale"},
		Resource:    metav1.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		SubResource: "scale", Operation: admissionv1.Update, Namespace: "shop", Name: "checkout",
	}
	c, err := toChange(req)
	if err != nil {
		t.Fatal(err)
	}
	if c.GroupKind() != "apps/Deployment" || c.SubResource != "scale" {
		t.Errorf("change = %+v", c)
	}
}

func TestLinkTo(t *testing.T) {
	if _, ok := linkTo("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"); !ok {
		t.Error("valid traceparent was not linked")
	}
	if _, ok := linkTo("garbage"); ok {
		t.Error("invalid traceparent was linked")
	}
}
