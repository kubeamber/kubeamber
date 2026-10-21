// Package telemetry configures OpenTelemetry from the standard OTEL_*
// environment variables, and can serve metrics for Prometheus to scrape.
// Traces and OTLP metrics stay off unless an OTLP endpoint is set.
package telemetry

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Enabled reports whether an OTLP endpoint is configured.
func Enabled() bool {
	return tracesEnabled() || metricsEnabled()
}

func tracesEnabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

func metricsEnabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}

// Setup installs global trace and metric providers. Traces and metrics are each
// exported over OTLP/HTTP when their endpoint, or the shared one, is set, so
// metrics can go straight to Prometheus's OTLP receiver. With scrape set,
// metrics are also kept for Prometheus, and the returned handler serves them in
// the Prometheus text format; otherwise the handler is nil. The returned
// function flushes and stops the providers.
func Setup(ctx context.Context, version string, scrape bool) (http.Handler, func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if !Enabled() && !scrape {
		return nil, func(context.Context) error { return nil }, nil
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "kubeamber"),
			attribute.String("service.version", version),
		),
		resource.WithFromEnv(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, nil, err
	}

	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	var stops []func(context.Context) error
	var handler http.Handler
	if scrape {
		registry := prometheus.NewRegistry()
		registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		// Every KubeAmber metric comes from one instrumentation scope, so the
		// otel_scope_* labels would only add noise.
		promExp, err := otelprometheus.New(otelprometheus.WithRegisterer(registry), otelprometheus.WithoutScopeInfo())
		if err != nil {
			return nil, nil, err
		}
		opts = append(opts, sdkmetric.WithReader(promExp))
		handler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	}
	if tracesEnabled() {
		traceExp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
		otel.SetTracerProvider(tp)
		stops = append(stops, tp.Shutdown)
	}
	if metricsEnabled() {
		metricExp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(mp)
	stops = append(stops, mp.Shutdown)

	return handler, func(ctx context.Context) error {
		var errs []error
		for _, stop := range stops {
			errs = append(errs, stop(ctx))
		}
		return errors.Join(errs...)
	}, nil
}
