package changes

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeamber/kubeamber/internal/changerequest"
)

type fakeProvider struct {
	opened  int
	ticket  Ticket
	syncErr error
	closed  []Outcome
	notes   []string
}

func (f *fakeProvider) Name() string { return "servicenow" }

func (f *fakeProvider) Open(_ context.Context, cr *changerequest.ChangeRequest) (*changerequest.External, error) {
	f.opened++
	end := metav1.NewTime(time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC))
	return &changerequest.External{System: "servicenow", ID: "sys-1", Number: "CHG0030001", State: "New", PlannedEnd: &end}, nil
}

func (f *fakeProvider) Sync(context.Context, changerequest.External) (Ticket, error) {
	return f.ticket, f.syncErr
}

func (f *fakeProvider) Close(_ context.Context, _ changerequest.External, o Outcome, notes string) error {
	f.closed = append(f.closed, o)
	f.notes = append(f.notes, notes)
	return nil
}

type memStore struct {
	crs     []*changerequest.ChangeRequest
	updates int
}

func (m *memStore) List(context.Context) ([]*changerequest.ChangeRequest, error) { return m.crs, nil }
func (m *memStore) Update(context.Context, *changerequest.ChangeRequest) error {
	m.updates++
	return nil
}

var now = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)

func pending() *changerequest.ChangeRequest {
	return &changerequest.ChangeRequest{Name: "cr-1", Spec: changerequest.Spec{
		Agent: "sre-agent", User: "system:serviceaccount:kubeamber-agents:sre-agent", Operation: "UPDATE",
		Target: changerequest.Target{Kind: "Deployment", Namespace: "prod", Name: "checkout"}, Score: 75,
		Summary: "replicas 3 → 0", Intent: "stop the crash loop", Fingerprint: "fp",
		ExpiresAt: metav1.NewTime(now.Add(time.Hour)),
	}, Status: changerequest.Status{Phase: changerequest.Pending}}
}

func newBridge(p *fakeProvider, s *memStore) *Bridge {
	return &Bridge{Provider: p, Store: s, ApprovalTTL: 15 * time.Minute,
		Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestOpensOnceThenWaits(t *testing.T) {
	p, s := &fakeProvider{ticket: Ticket{State: "New"}}, &memStore{crs: []*changerequest.ChangeRequest{pending()}}
	b := newBridge(p, s)
	ctx := context.Background()
	for range 3 {
		if err := b.ReconcileAll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	cr := s.crs[0]
	if p.opened != 1 || cr.Status.External == nil || cr.Status.External.Number != "CHG0030001" {
		t.Fatalf("opened %d times, external %+v", p.opened, cr.Status.External)
	}
	// One write to link the ticket; an unchanged ticket is not rewritten.
	if s.updates != 1 {
		t.Errorf("%d updates; an unchanged ticket should not be rewritten every pass", s.updates)
	}
	// Linked to a ticket, the request now waits for the ticket's window, not pendingTTL.
	if got := cr.Effective(now.Add(2*time.Hour), time.Minute); got != changerequest.Pending {
		t.Errorf("phase two hours later = %s", got)
	}

	// A rescheduled window is mirrored.
	later := now.Add(48 * time.Hour)
	p.ticket = Ticket{State: "Assess", PlannedEnd: &later}
	_ = b.ReconcileAll(ctx)
	if !cr.Status.External.PlannedEnd.Time.Equal(later) || cr.Status.External.State != "Assess" || s.updates != 2 {
		t.Errorf("reschedule not mirrored: %+v (updates %d)", cr.Status.External, s.updates)
	}
}

func TestRecordsApproval(t *testing.T) {
	p, s := &fakeProvider{}, &memStore{crs: []*changerequest.ChangeRequest{pending()}}
	b := newBridge(p, s)
	ctx := context.Background()
	_ = b.ReconcileAll(ctx)

	p.ticket = Ticket{State: "Scheduled", Decision: changerequest.DecisionApproved, DecidedBy: []string{"jane.cab"}, Reason: "CHG0030001 approved in ServiceNow"}
	_ = b.ReconcileAll(ctx)
	cr := s.crs[0]
	a := cr.Spec.Approval
	if cr.Status.Phase != changerequest.Approved || a == nil || a.By != "servicenow:jane.cab" || a.Source != "servicenow" ||
		a.ExternalRef != "CHG0030001" || !a.At.Time.Equal(now) {
		t.Fatalf("approval %+v phase %s", a, cr.Status.Phase)
	}
}

func TestRecordsRejectionWithoutClosingTwice(t *testing.T) {
	p, s := &fakeProvider{}, &memStore{crs: []*changerequest.ChangeRequest{pending()}}
	b := newBridge(p, s)
	ctx := context.Background()
	_ = b.ReconcileAll(ctx)
	p.ticket = Ticket{State: "Canceled", Decision: changerequest.DecisionRejected, DecidedBy: []string{"bob.cab"}}
	_ = b.ReconcileAll(ctx)
	_ = b.ReconcileAll(ctx)
	cr := s.crs[0]
	if cr.Status.Phase != changerequest.Rejected || !cr.Status.External.Closed || len(p.closed) != 0 {
		t.Errorf("phase %s closed %v provider closes %v", cr.Status.Phase, cr.Status.External.Closed, p.closed)
	}
}

func TestClosesImplemented(t *testing.T) {
	cr := pending()
	consumed := metav1.NewTime(now)
	cr.Status = changerequest.Status{Phase: changerequest.Consumed, ConsumedAt: &consumed,
		External: &changerequest.External{System: "servicenow", ID: "sys-1", Number: "CHG0030001"}}
	cr.Spec.Approval = &changerequest.Approval{Decision: changerequest.DecisionApproved, By: "servicenow:jane.cab", Source: "servicenow", ExternalRef: "CHG0030001"}
	p, s := &fakeProvider{}, &memStore{crs: []*changerequest.ChangeRequest{cr}}
	b := newBridge(p, s)
	_ = b.ReconcileAll(context.Background())
	_ = b.ReconcileAll(context.Background())
	if len(p.closed) != 1 || p.closed[0] != Implemented || !cr.Status.External.Closed {
		t.Fatalf("closes %v", p.closed)
	}
	for _, want := range []string{"sre-agent", "UPDATE Deployment prod/checkout", "replicas 3 → 0", "servicenow:jane.cab", "stop the crash loop", "cr-1"} {
		if !strings.Contains(p.notes[0], want) {
			t.Errorf("notes lack %q: %s", want, p.notes[0])
		}
	}
}

func TestCancelsAbandoned(t *testing.T) {
	expired := pending()
	past := metav1.NewTime(now.Add(-time.Minute))
	expired.Status.External = &changerequest.External{System: "servicenow", ID: "sys-1", Number: "CHG0030001", PlannedEnd: &past}

	rejectedHere := pending()
	rejectedHere.Name = "cr-2"
	rejectedHere.Status = changerequest.Status{Phase: changerequest.Rejected,
		External: &changerequest.External{System: "servicenow", ID: "sys-2", Number: "CHG0030002"}}
	rejectedHere.Spec.Approval = &changerequest.Approval{Decision: changerequest.DecisionRejected, By: "alice", Reason: "use a rollback"}

	p, s := &fakeProvider{}, &memStore{crs: []*changerequest.ChangeRequest{expired, rejectedHere}}
	_ = newBridge(p, s).ReconcileAll(context.Background())
	if len(p.closed) != 2 || p.closed[0] != Abandoned || p.closed[1] != Abandoned {
		t.Fatalf("closes %v", p.closed)
	}
	if !strings.Contains(p.notes[0], "expired") || !strings.Contains(p.notes[1], "use a rollback") {
		t.Errorf("notes %q", p.notes)
	}
}

func TestRecordsSyncErrors(t *testing.T) {
	cr := pending()
	cr.Status.External = &changerequest.External{System: "servicenow", ID: "sys-1", Number: "CHG0030001"}
	p, s := &fakeProvider{syncErr: errors.New("servicenow: 503 Service Unavailable")}, &memStore{crs: []*changerequest.ChangeRequest{cr}}
	_ = newBridge(p, s).ReconcileAll(context.Background())
	if !strings.Contains(cr.Status.External.Error, "503") || cr.Status.Phase != changerequest.Pending {
		t.Errorf("error %q phase %s", cr.Status.External.Error, cr.Status.Phase)
	}
}

func TestIgnoresLocalOnlyRequests(t *testing.T) {
	decided := pending()
	decided.Status.Phase = changerequest.Approved
	decided.Spec.Approval = &changerequest.Approval{Decision: changerequest.DecisionApproved, By: "alice", At: metav1.NewTime(now)}
	p, s := &fakeProvider{}, &memStore{crs: []*changerequest.ChangeRequest{decided}}
	_ = newBridge(p, s).ReconcileAll(context.Background())
	if p.opened != 0 || s.updates != 0 {
		t.Errorf("a request decided in the cluster got a ticket (opened %d)", p.opened)
	}
}
