package webhook

import (
	"context"
	"errors"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kubeamber/kubeamber/internal/changerequest"
)

// manualMetrics installs a meter provider whose metrics the test collects.
// Instruments are created by NewHandler and ObserveChangeRequests, so call it
// before either.
func manualMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	r := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	return r
}

func collect(t *testing.T, r *sdkmetric.ManualReader, name string) metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data
			}
		}
	}
	t.Fatalf("no metric %s", name)
	return nil
}

func attrs(kv ...string) attribute.Set {
	var a []attribute.KeyValue
	for i := 0; i < len(kv); i += 2 {
		a = append(a, attribute.String(kv[i], kv[i+1]))
	}
	return attribute.NewSet(a...)
}

func TestReviewDurationByOutcome(t *testing.T) {
	r := manualMetrics(t)
	f := newFixture(t)
	if resp := f.h.Review(context.Background(), scaleToZero(agentUser)); resp.Allowed {
		t.Fatal("scale to zero in prod was not held")
	}
	f.h.Review(context.Background(), scaleToZero("alice")) // people are not reviewed

	h := collect(t, r, "kubeamber.review.duration").(metricdata.Histogram[float64])
	if len(h.DataPoints) != 1 {
		t.Fatalf("got %d data points, want 1: %+v", len(h.DataPoints), h.DataPoints)
	}
	dp := h.DataPoints[0]
	if want := attrs("kubeamber.outcome", "hold"); !dp.Attributes.Equals(&want) {
		t.Errorf("attributes = %v, want %v", dp.Attributes.ToSlice(), want.ToSlice())
	}
	if dp.Count != 1 {
		t.Errorf("count = %d, want 1", dp.Count)
	}
}

func TestApprovalWaitByDecisionAndSource(t *testing.T) {
	r := manualMetrics(t)
	f := newFixture(t)
	now := f.now
	created := func(cr map[string]any) map[string]any {
		cr["metadata"].(map[string]any)["creationTimestamp"] = now.Add(-10 * time.Minute).Format(time.RFC3339)
		return cr
	}
	old := created(pendingCR(now.Add(time.Hour)))
	linked := withTicket(old, "CHG0030001")
	dryRun := crUpdate("alice", old, decided(old, "Approved", "alice", now, "Approved"))
	dryRun.DryRun = new(true)

	for _, req := range []*admissionv1.AdmissionRequest{
		crUpdate("alice", old, decided(old, "Approved", "alice", now, "Approved")),
		crUpdate("alice", old, decided(old, "Rejected", "alice", now, "Rejected")),
		crUpdate(bridge, linked, externallyDecided(linked, "Approved", "servicenow:jane.cab", "CHG0030001", now, "Approved")),
	} {
		if resp := f.h.Review(context.Background(), req); !resp.Allowed {
			t.Fatalf("decision denied: %s", resp.Result.Message)
		}
	}
	// None of these decide anything.
	for _, req := range []*admissionv1.AdmissionRequest{
		crUpdate(agentUser, old, decided(old, "Approved", agentUser, now, "Approved")), // denied
		crUpdate("alice", old, pendingCR(now.Add(time.Hour))),                          // a label edit
		dryRun,
	} {
		f.h.Review(context.Background(), req)
	}

	h := collect(t, r, "kubeamber.approval.wait").(metricdata.Histogram[float64])
	want := map[attribute.Set]bool{
		attrs("kubeamber.decision", "approved", "kubeamber.decision.source", "cluster"):    true,
		attrs("kubeamber.decision", "rejected", "kubeamber.decision.source", "cluster"):    true,
		attrs("kubeamber.decision", "approved", "kubeamber.decision.source", "servicenow"): true,
	}
	if len(h.DataPoints) != len(want) {
		t.Fatalf("got %d data points, want %d: %+v", len(h.DataPoints), len(want), h.DataPoints)
	}
	for _, dp := range h.DataPoints {
		if !want[dp.Attributes] {
			t.Errorf("unexpected attributes %v", dp.Attributes.ToSlice())
		}
		if dp.Count != 1 || dp.Sum != 600 {
			t.Errorf("%v: count %d, sum %v; want one wait of 600s", dp.Attributes.ToSlice(), dp.Count, dp.Sum)
		}
	}
}

type listerFunc func(context.Context) ([]*changerequest.ChangeRequest, error)

func (f listerFunc) List(ctx context.Context) ([]*changerequest.ChangeRequest, error) { return f(ctx) }

func TestChangeRequestsByEffectivePhase(t *testing.T) {
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	ttl := time.Hour
	cr := func(phase changerequest.Phase, expires time.Time, approvedAt *time.Time) *changerequest.ChangeRequest {
		c := &changerequest.ChangeRequest{}
		c.Status.Phase = phase
		c.Spec.ExpiresAt = metav1.NewTime(expires)
		if approvedAt != nil {
			c.Spec.Approval = &changerequest.Approval{Decision: changerequest.DecisionApproved, At: metav1.NewTime(*approvedAt)}
		}
		return c
	}
	recently, longAgo := now.Add(-time.Minute), now.Add(-2*ttl)
	crs := []*changerequest.ChangeRequest{
		cr(changerequest.Pending, now.Add(time.Hour), nil),
		cr(changerequest.Pending, now.Add(time.Hour), nil),
		cr(changerequest.Pending, now.Add(-time.Minute), nil), // past its deadline
		cr(changerequest.Approved, now.Add(time.Hour), &recently),
		cr(changerequest.Approved, now.Add(time.Hour), &longAgo), // approval older than the TTL
		cr(changerequest.Consumed, now.Add(time.Hour), &recently),
	}

	r := manualMetrics(t)
	fail := false
	l := listerFunc(func(context.Context) ([]*changerequest.ChangeRequest, error) {
		if fail {
			return nil, errors.New("apiserver unavailable")
		}
		return crs, nil
	})
	if err := ObserveChangeRequests(l, func() time.Duration { return ttl }, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}

	g := collect(t, r, "kubeamber.change_requests").(metricdata.Gauge[int64])
	got := map[string]int64{}
	for _, dp := range g.DataPoints {
		phase, _ := dp.Attributes.Value("kubeamber.phase")
		got[phase.AsString()] = dp.Value
	}
	want := map[string]int64{"Pending": 2, "Approved": 1, "Rejected": 0, "Consumed": 1, "Expired": 2}
	if len(got) != len(want) {
		t.Errorf("phases = %v, want %v", got, want)
	}
	for p, n := range want {
		if got[p] != n {
			t.Errorf("%s = %d, want %d", p, got[p], n)
		}
	}

	// A failed list reports nothing rather than zeros.
	fail = true
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err == nil {
		t.Error("a failed list was not reported")
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "kubeamber.change_requests" && len(m.Data.(metricdata.Gauge[int64]).DataPoints) > 0 {
				t.Errorf("reported %d data points after a failed list", len(m.Data.(metricdata.Gauge[int64]).DataPoints))
			}
		}
	}
}
