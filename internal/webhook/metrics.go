package webhook

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/kubeamber/kubeamber/internal/changerequest"
)

// Lister lists change requests; *changerequest.Store implements it.
type Lister interface {
	List(ctx context.Context) ([]*changerequest.ChangeRequest, error)
}

// phases are the effective phases kubeamber.change_requests reports, each
// even when zero, so a dashboard never shows "no data" for an empty phase.
var phases = []changerequest.Phase{
	changerequest.Pending, changerequest.Approved, changerequest.Rejected,
	changerequest.Consumed, changerequest.Expired,
}

// ObserveChangeRequests reports the cluster's change requests by effective
// phase as the gauge kubeamber.change_requests, listing them each time
// metrics are collected. Every replica reports the same cluster-wide counts,
// so aggregate across replicas with max, not sum.
func ObserveChangeRequests(l Lister, approvalTTL func() time.Duration, now func() time.Time) error {
	meter := otel.Meter(instrumentation)
	_, err := meter.Int64ObservableGauge("kubeamber.change_requests",
		metric.WithDescription("Change requests in the cluster by effective phase. Every replica reports the cluster-wide count."),
		metric.WithUnit("{change_request}"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			crs, err := l.List(ctx)
			if err != nil {
				return err
			}
			counts := countByPhase(crs, now(), approvalTTL())
			for _, p := range phases {
				o.Observe(counts[p], metric.WithAttributes(attribute.String("kubeamber.phase", string(p))))
			}
			return nil
		}))
	return err
}

func countByPhase(crs []*changerequest.ChangeRequest, now time.Time, approvalTTL time.Duration) map[changerequest.Phase]int64 {
	counts := map[changerequest.Phase]int64{}
	for _, cr := range crs {
		counts[cr.Effective(now, approvalTTL)]++
	}
	return counts
}
