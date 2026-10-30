package telemetry

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func noOTLP(t *testing.T) {
	t.Helper()
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		t.Setenv(k, "")
	}
}

func TestSetupServesPrometheusMetrics(t *testing.T) {
	noOTLP(t)
	ctx := context.Background()
	handler, shutdown, err := Setup(ctx, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(ctx) }()
	if handler == nil {
		t.Fatal("no metrics handler with scrape enabled")
	}

	decisions, err := otel.Meter("test").Int64Counter("kubeamber.decisions", metric.WithUnit("{decision}"))
	if err != nil {
		t.Fatal(err)
	}
	decisions.Add(ctx, 2, metric.WithAttributes(attribute.String("kubeamber.outcome", "hold")))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`kubeamber_decisions_total{`,
		`kubeamber_outcome="hold"`,
		`target_info{`,
		`service_name="kubeamber"`,
		`go_goroutines `,
		`kubeamber_build_info{goversion="go`,
		`version="test"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output lacks %q:\n%s", want, body)
		}
	}
}

func TestSetupMetricsEndpointStartsNoTraceExporter(t *testing.T) {
	noOTLP(t)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:1/api/v1/otlp/v1/metrics")
	before := otel.GetTracerProvider()
	_, shutdown, err := Setup(context.Background(), "test", false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	defer func() { _ = shutdown(ctx) }() // the endpoint is unreachable; only startup matters here
	if otel.GetTracerProvider() != before {
		t.Error("a trace exporter was started although only a metrics endpoint is set")
	}
}

func TestSetupSurvivesBadResourceAttributes(t *testing.T) {
	noOTLP(t)
	// A value without "=" is malformed; a platform may inject one like this.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team")
	handler, shutdown, err := Setup(context.Background(), "test", true)
	if err != nil {
		t.Fatalf("telemetry must never stop the webhook from starting: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if handler == nil {
		t.Fatal("no metrics handler")
	}
}

func TestSetupOffByDefault(t *testing.T) {
	noOTLP(t)
	handler, shutdown, err := Setup(context.Background(), "test", false)
	if err != nil {
		t.Fatal(err)
	}
	if handler != nil {
		t.Error("a metrics handler was returned with scrape disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Error(err)
	}
}
