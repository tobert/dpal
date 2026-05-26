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
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Supported OTLP transport protocols.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http/protobuf"
)

// Config controls OTel setup. An empty Endpoint disables OTel entirely.
type Config struct {
	Endpoint    string // OTLP endpoint, e.g. "localhost:4317" (gRPC) or "localhost:4318" (HTTP)
	Protocol    string // "grpc" (default) or "http/protobuf"; matches OTEL_EXPORTER_OTLP_PROTOCOL conventions
	ServiceName string
	Version     string
	Insecure    bool // use plaintext transport; true is sensible for local collectors
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

	exporter, err := newExporter(ctx, cfg)
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

func newExporter(ctx context.Context, cfg Config) (*otlptrace.Exporter, error) {
	proto := cfg.Protocol
	if proto == "" {
		proto = ProtocolGRPC
	}
	switch proto {
	case ProtocolGRPC, "grpc/protobuf":
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	case ProtocolHTTP, "http":
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		return otlptracehttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown OTLP protocol %q (want %q or %q)", proto, ProtocolGRPC, ProtocolHTTP)
	}
}
