package webhook

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kubeamber/kubeamber/pkg/risk"
)

// recordedEvent is one Event as the handler emitted it.
type recordedEvent struct {
	regarding, related   *corev1.ObjectReference
	kind, reason, action string
	note                 string
}

type recorder struct{ events []recordedEvent }

func (r *recorder) Eventf(regarding, related runtime.Object, eventtype, reason, action, note string, args ...any) {
	e := recordedEvent{kind: eventtype, reason: reason, action: action, note: fmt.Sprintf(note, args...)}
	e.regarding, _ = regarding.(*corev1.ObjectReference)
	e.related, _ = related.(*corev1.ObjectReference)
	r.events = append(r.events, e)
}

func withEvents(t *testing.T, f *fixture, outcomes string) *recorder {
	t.Helper()
	set, err := ParseEventOutcomes(outcomes)
	if err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	f.h.Events, f.h.EventOutcomes = r, set
	return r
}

func TestEventsFollowTheDecision(t *testing.T) {
	f := newFixture(t)
	r := withEvents(t, f, strings.Join(EventOutcomes, ","))
	ctx := context.Background()

	f.h.Review(ctx, scaleToZero(agentUser))
	if len(r.events) != 1 {
		t.Fatalf("hold: want 1 event, got %d", len(r.events))
	}
	held := r.events[0]
	if held.kind != corev1.EventTypeWarning || held.reason != "AgentChangeHeld" || held.action != "UPDATE" {
		t.Errorf("hold event: %+v", held)
	}
	if got := held.regarding; got == nil || got.APIVersion != "apps/v1" || got.Kind != "Deployment" || got.Namespace != "prod" || got.Name != "checkout" {
		t.Errorf("regarding: %+v", got)
	}
	if got := held.related; got == nil || got.Kind != "ChangeRequest" || got.Name != "cr-1" {
		t.Errorf("related: %+v", got)
	}
	for _, want := range []string{"Agent bot", "cr-1", "kubeamber approve cr-1", "kubeamber reject cr-1", "risk 75/100"} {
		if !strings.Contains(held.note, want) {
			t.Errorf("hold note %q lacks %q", held.note, want)
		}
	}

	f.store.approve("cr-1", "alice", f.now)
	f.h.Review(ctx, scaleToZero(agentUser))
	if len(r.events) != 2 {
		t.Fatalf("approved retry: want 2 events, got %d", len(r.events))
	}
	if e := r.events[1]; e.kind != corev1.EventTypeNormal || e.reason != "AgentChangeApproved" || !strings.Contains(e.note, "cr-1 was approved") {
		t.Errorf("approved event: %+v", e)
	}
}

func TestEventsForDenialsAndRejections(t *testing.T) {
	f := newFixture(t)
	r := withEvents(t, f, "deny,rejected")
	ctx := context.Background()

	req := scaleToZero(agentUser)
	req.Kind.Group, req.Kind.Kind = "", "Namespace"
	req.Resource.Group, req.Resource.Version, req.Resource.Resource = "", "v1", "namespaces"
	req.Namespace, req.Name, req.Operation = "", "prod", "DELETE"
	req.Object, req.OldObject = runtime.RawExtension{}, runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"prod","uid":"ns-uid"}}`)}
	f.h.Review(ctx, req)
	if len(r.events) != 1 {
		t.Fatalf("deny: want 1 event, got %d", len(r.events))
	}
	denied := r.events[0]
	if denied.reason != "AgentChangeDenied" || denied.kind != corev1.EventTypeWarning || !strings.Contains(denied.note, "DENY") {
		t.Errorf("deny event: %+v", denied)
	}
	if denied.regarding.UID != "ns-uid" || denied.regarding.APIVersion != "v1" || denied.related != nil {
		t.Errorf("deny regarding/related: %+v %+v", denied.regarding, denied.related)
	}

	// Holds are not in this set; the rejection that follows is.
	f.h.Review(ctx, scaleToZero(agentUser))
	cr := f.store.crs["cr-1"]
	f.store.approve("cr-1", "alice", f.now)
	cr.Spec.Approval.Decision, cr.Status.Phase = "Rejected", "Rejected"
	f.h.Review(ctx, scaleToZero(agentUser))
	if len(r.events) != 2 || r.events[1].reason != "AgentChangeRejected" || !strings.Contains(r.events[1].note, "rejected change request cr-1") {
		t.Fatalf("rejection events: %+v", r.events)
	}
}

func TestNoEventsForAllowsDryRunsOrPeople(t *testing.T) {
	f := newFixture(t)
	r := withEvents(t, f, strings.Join(EventOutcomes, ","))
	ctx := context.Background()

	allow := scaleToZero(agentUser)
	allow.Namespace, allow.Object = "shop", deploymentJSON(4)
	f.h.Review(ctx, allow)

	dry := scaleToZero(agentUser)
	yes := true
	dry.DryRun = &yes
	f.h.Review(ctx, dry)

	f.h.Review(ctx, scaleToZero("alice"))

	if len(r.events) != 0 {
		t.Fatalf("want no events, got %+v", r.events)
	}

	// Allows are recorded when asked for.
	r = withEvents(t, f, "all")
	f.h.Review(ctx, allow)
	if len(r.events) != 1 || r.events[0].reason != "AgentChangeAllowed" || r.events[0].kind != corev1.EventTypeNormal {
		t.Fatalf("allow event: %+v", r.events)
	}
}

func TestParseEventOutcomes(t *testing.T) {
	for in, want := range map[string]int{"": 0, "none": 0, "hold": 1, "hold, deny": 2, "all": 5} {
		got, err := ParseEventOutcomes(in)
		if err != nil || len(got) != want {
			t.Errorf("ParseEventOutcomes(%q) = %v, %v; want %d outcomes", in, got, err, want)
		}
	}
	if _, err := ParseEventOutcomes("hold,typo"); err == nil {
		t.Error("an unknown outcome was accepted")
	}
}

func TestEventNoteFitsTheAPILimit(t *testing.T) {
	long := strings.Repeat("é", 2000)
	got := truncate(long, maxEventNote)
	if len(got) > maxEventNote || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate: %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
	if truncate("short", maxEventNote) != "short" {
		t.Error("a short note was changed")
	}
}

func TestObjectUID(t *testing.T) {
	c := risk.Change{Object: map[string]any{"metadata": map[string]any{"uid": "new"}}}
	if objectUID(c) != "new" {
		t.Error("UID from the object")
	}
	c.OldObject = map[string]any{"metadata": map[string]any{"uid": "old"}}
	if objectUID(c) != "old" {
		t.Error("the old object's UID comes first")
	}
	if objectUID(risk.Change{}) != "" {
		t.Error("no UID without objects")
	}
}
