package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubeamber/kubeamber/internal/changerequest"
)

var t0 = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

type listStore struct {
	crs []*changerequest.ChangeRequest
}

func (s *listStore) List(context.Context) ([]*changerequest.ChangeRequest, error) { return s.crs, nil }

type memState struct {
	sent  Sent
	saves int
}

func (s *memState) Load(context.Context) (Sent, bool, error) {
	if s.sent == nil {
		return Sent{}, true, nil
	}
	out := Sent{}
	for k, v := range s.sent {
		out[k] = map[string]bool{}
		for kk := range v {
			out[k][kk] = true
		}
	}
	return out, false, nil
}

func (s *memState) Save(_ context.Context, sent Sent) error {
	s.sent, s.saves = sent, s.saves+1
	return nil
}

type fakeChannel struct {
	name string
	got  []Message
	fail bool
}

func (c *fakeChannel) Name() string { return c.name }
func (c *fakeChannel) Send(_ context.Context, m Message) error {
	if c.fail {
		return errors.New("down")
	}
	c.got = append(c.got, m)
	return nil
}

func (c *fakeChannel) events() []Event {
	var out []Event
	for _, m := range c.got {
		out = append(out, m.Event)
	}
	return out
}

func held(name string, created time.Time) *changerequest.ChangeRequest {
	return &changerequest.ChangeRequest{
		Name:              name,
		CreationTimestamp: metav1.NewTime(created),
		Spec: changerequest.Spec{
			Agent: "kagent", User: "system:serviceaccount:kagent:kagent-tools", Operation: "UPDATE",
			Target: changerequest.Target{Kind: "Deployment", Namespace: "prod", Name: "checkout"},
			Score:  50, Summary: "replicas 3 → 10", Factors: []string{"+25 namespace \"prod\" is protected"},
			Intent:    "traffic spike",
			ExpiresAt: metav1.NewTime(created.Add(time.Hour)),
		},
		Status: changerequest.Status{Phase: changerequest.Pending},
	}
}

func decide(cr *changerequest.ChangeRequest, d changerequest.Decision, at time.Time) {
	cr.Spec.Approval = &changerequest.Approval{Decision: d, By: "alice@example.com", Reason: "planned", At: metav1.NewTime(at)}
	cr.Status.Phase = changerequest.Phase(d)
}

func newNotifier(store *listStore, state State, now *time.Time, channels ...Channel) *Notifier {
	return &Notifier{
		Store: store, State: state, Channels: channels, RemindAfter: 15 * time.Minute,
		ApprovalTTL: func() time.Duration { return 15 * time.Minute },
		Now:         func() time.Time { return *now },
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestLifecycleIsToldOncePerEvent(t *testing.T) {
	now := t0
	cr := held("cr-1", t0)
	store, state := &listStore{crs: []*changerequest.ChangeRequest{cr}}, &memState{sent: Sent{}}
	slack := &fakeChannel{name: "slack"}
	n := newNotifier(store, state, &now, slack)
	ctx := context.Background()

	step := func(d time.Duration) {
		now = now.Add(d)
		if err := n.Notify(ctx); err != nil {
			t.Fatal(err)
		}
	}
	step(0)
	step(time.Minute)      // nothing new
	step(15 * time.Minute) // reminder
	decide(cr, changerequest.DecisionApproved, now)
	step(time.Minute)
	cr.Status.Phase = changerequest.Consumed
	step(time.Minute)
	step(time.Minute)

	want := []Event{Held, Reminder, Approved, Applied}
	if got := slack.events(); !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	m := slack.got[0]
	if !strings.Contains(m.Title, "Approval needed: UPDATE Deployment prod/checkout by agent kagent (risk 50/100)") {
		t.Errorf("held title %q", m.Title)
	}
	if !slices.ContainsFunc(m.Lines(), func(l string) bool { return strings.Contains(l, "kubeamber approve cr-1") }) {
		t.Errorf("held lines lack the approve command: %v", m.Lines())
	}
	if a := slack.got[2]; a.DecidedBy != "alice@example.com" || a.Reason != "planned" {
		t.Errorf("approved message: %+v", a)
	}
}

func TestRejectedAndExpired(t *testing.T) {
	now := t0
	rejected, unused, abandoned := held("cr-r", t0), held("cr-u", t0), held("cr-a", t0)
	decide(rejected, changerequest.DecisionRejected, t0)
	decide(unused, changerequest.DecisionApproved, t0)
	store := &listStore{crs: []*changerequest.ChangeRequest{rejected, unused, abandoned}}
	ch := &fakeChannel{name: "c"}
	n := newNotifier(store, &memState{sent: Sent{}}, &now, ch)
	n.RemindAfter = 0
	_ = n.Notify(context.Background())
	now = t0.Add(2 * time.Hour) // the approval TTL and the pending deadline have both passed
	_ = n.Notify(context.Background())

	got := map[string][]Event{}
	for _, m := range ch.got {
		got[m.ChangeRequest] = append(got[m.ChangeRequest], m.Event)
	}
	if !slices.Equal(got["cr-r"], []Event{Rejected}) || !slices.Equal(got["cr-u"], []Event{Approved, Expired}) ||
		!slices.Equal(got["cr-a"], []Event{Held, Expired}) {
		t.Fatalf("events: %v", got)
	}
}

func TestFirstStartDoesNotReplayHistory(t *testing.T) {
	now := t0.Add(time.Hour)
	old := held("cr-old", t0)
	decide(old, changerequest.DecisionApproved, t0)
	old.Status.Phase = changerequest.Consumed
	waiting := held("cr-waiting", t0.Add(50*time.Minute))
	store, state := &listStore{crs: []*changerequest.ChangeRequest{old, waiting}}, &memState{}
	ch := &fakeChannel{name: "c"}
	n := newNotifier(store, state, &now, ch)

	if err := n.Notify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ch.got) != 1 || ch.got[0].ChangeRequest != "cr-waiting" || ch.got[0].Event != Held {
		t.Fatalf("first start sent %+v", ch.got)
	}
	if !state.sent["cr-old"]["applied/c"] {
		t.Error("the decided request was not marked as told")
	}
}

func TestFailedChannelIsRetriedAlone(t *testing.T) {
	now := t0
	store := &listStore{crs: []*changerequest.ChangeRequest{held("cr-1", t0)}}
	ok, down := &fakeChannel{name: "ok"}, &fakeChannel{name: "down", fail: true}
	n := newNotifier(store, &memState{sent: Sent{}}, &now, ok, down)
	_ = n.Notify(context.Background())
	down.fail = false
	_ = n.Notify(context.Background())
	if len(ok.got) != 1 || len(down.got) != 1 {
		t.Fatalf("ok got %d, down got %d; want 1 each", len(ok.got), len(down.got))
	}
}

func TestNoReminderRightAfterTheFirstMessage(t *testing.T) {
	now := t0.Add(time.Hour) // first seen long after it was raised
	store := &listStore{crs: []*changerequest.ChangeRequest{held("cr-1", t0.Add(10*time.Minute))}}
	ch := &fakeChannel{name: "c"}
	n := newNotifier(store, &memState{sent: Sent{}}, &now, ch)
	n.RemindAfter = 5 * time.Minute
	_ = n.Notify(context.Background())
	now = now.Add(time.Minute)
	_ = n.Notify(context.Background())
	if got := ch.events(); !slices.Equal(got, []Event{Held}) {
		t.Fatalf("events %v, want only held", got)
	}
}

func TestRoutes(t *testing.T) {
	now := t0
	low := held("cr-low", t0)
	low.Spec.Score, low.Spec.Target.Namespace = 35, "shop"
	high := held("cr-high", t0)
	high.Spec.Score = 70
	store := &listStore{crs: []*changerequest.ChangeRequest{low, high}}
	team, oncall := &fakeChannel{name: "team"}, &fakeChannel{name: "oncall"}
	n := newNotifier(store, &memState{sent: Sent{}}, &now, team, oncall)
	n.Routes = []Route{
		{Channels: []string{"team"}},
		{Channels: []string{"oncall"}, MinRisk: 60, Namespaces: []string{"prod*"}, Events: []Event{Held}},
	}
	_ = n.Notify(context.Background())
	if len(team.got) != 2 || len(oncall.got) != 1 || oncall.got[0].ChangeRequest != "cr-high" {
		t.Fatalf("team %d, oncall %+v", len(team.got), oncall.got)
	}
	if err := ValidateRoutes(n.Routes, []string{"team", "oncall"}); err != nil {
		t.Error(err)
	}
	for _, bad := range [][]Route{{{Channels: []string{"nope"}}}, {{}}, {{Channels: []string{"team"}, Events: []Event{"typo"}}}, {{Channels: []string{"team"}, MinRisk: 101}}} {
		if ValidateRoutes(bad, []string{"team"}) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestStateIsPrunedAndPersisted(t *testing.T) {
	cs := fake.NewClientset()
	st := &ConfigMapState{Client: cs, Namespace: "kubeamber-system", Name: "kubeamber-notify-state"}
	now := t0
	store := &listStore{crs: []*changerequest.ChangeRequest{held("cr-1", t0)}}
	ch := &fakeChannel{name: "c"}
	n := newNotifier(store, st, &now, ch)
	ctx := context.Background()
	if err := n.Notify(ctx); err != nil {
		t.Fatal(err)
	}
	cm, err := cs.CoreV1().ConfigMaps("kubeamber-system").Get(ctx, "kubeamber-notify-state", metav1.GetOptions{})
	if err != nil || cm.Data["cr-1"] != "held/c" {
		t.Fatalf("state %v, %v", cm.Data, err)
	}

	// A restarted notifier reads the state back and repeats nothing.
	n2 := newNotifier(store, &ConfigMapState{Client: cs, Namespace: "kubeamber-system", Name: "kubeamber-notify-state"}, &now, ch)
	if err := n2.Notify(ctx); err != nil || len(ch.got) != 1 {
		t.Fatalf("after restart: %d messages, %v", len(ch.got), err)
	}

	// A deleted request is dropped from the state.
	store.crs = nil
	if err := n2.Notify(ctx); err != nil {
		t.Fatal(err)
	}
	cm, _ = cs.CoreV1().ConfigMaps("kubeamber-system").Get(ctx, "kubeamber-notify-state", metav1.GetOptions{})
	if len(cm.Data) != 0 {
		t.Errorf("state was not pruned: %v", cm.Data)
	}
}

func TestMessageCleansAgentText(t *testing.T) {
	cr := held("cr-1", t0)
	cr.Spec.Intent = "fix\r\x1b[2K the <!channel> crash\nloop" + strings.Repeat("x", 400)
	m := NewMessage(cr, Held, "prod-eu-1")
	if strings.ContainsAny(m.Intent, "\r\n\x1b") || len([]rune(m.Intent)) > maxIntent {
		t.Errorf("intent not cleaned: %q", m.Intent)
	}
	if m.Cluster != "prod-eu-1" || m.Target() != "Deployment prod/checkout" {
		t.Errorf("message: %+v", m)
	}
}
