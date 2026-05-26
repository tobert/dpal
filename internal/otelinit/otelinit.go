// Package otelinit configures OpenTelemetry tracing for dpal.
//
// The bootstrap is opt-in: if no endpoint is supplied (neither via the
// Config struct nor via standard OTEL_EXPORTER_OTLP_ENDPOINT-style env
// vars), no global TracerProvider is registered and otel.Tracer calls
// remain no-ops.
package otelinit

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Config controls OTel setup. An empty Endpoint disables OTel entirely.
type Config struct {
	Endpoint    string // OTLP gRPC endpoint, e.g. "localhost:4317"
	ServiceName string
	Version     string
	Insecure    bool // use plaintext gRPC; true is sensible for local collectors
}

// Bootstrap installs a global TracerProvider configured to export over
// OTLP gRPC, plus the W3C TraceContext + Baggage propagators. Returns a
// shutdown function that flushes pending spans on call.
//
// When cfg.Endpoint is empty, returns a no-op shutdown and leaves the
// otel global state untouched — callers can unconditionally defer the
// returned shutdown.
func Bootstrap(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	noopShutdown := func(context.Context) error { return nil }
	if cfg.Endpoint == "" {
		return noopShutdown, nil
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return noopShutdown, fmt.Errorf("otelinit: build OTLP exporter: %w", err)
	}

	res := resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", cfg.Version),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(shutdownCtx)
	}, nil
}
