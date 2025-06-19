// Copyright 2024 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package telemetry

import (
	"context"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	ServiceName    = "alertmanager"
	ServiceVersion = "dev" // This should be set during build
)

var (
	tracer trace.Tracer
	logger *slog.Logger

	// Telemetry metrics
	spansStarted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "alertmanager_telemetry_spans_started_total",
			Help: "Total number of spans started by operation",
		},
		[]string{"operation"},
	)

	spansFinished = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "alertmanager_telemetry_spans_finished_total",
			Help: "Total number of spans finished by operation and status",
		},
		[]string{"operation", "status"},
	)

	exportErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "alertmanager_telemetry_export_errors_total",
			Help: "Total number of trace export errors",
		},
	)

	exportDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "alertmanager_telemetry_export_duration_seconds",
			Help:    "Time spent exporting traces",
			Buckets: prometheus.DefBuckets,
		},
	)
)

// Config holds telemetry configuration
type Config struct {
	Enabled bool
	Logger  *slog.Logger
}

// Initialize sets up OpenTelemetry with OTLP HTTP exporter
// This allows users to configure OTEL via environment variables
func Initialize(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	logger = cfg.Logger

	if !cfg.Enabled {
		logger.Info("OpenTelemetry tracing disabled")
		otel.SetTracerProvider(noop.NewTracerProvider())
		tracer = otel.Tracer(ServiceName)
		return func(context.Context) error { return nil }, nil
	}

	logger.Info("Initializing OpenTelemetry tracing")

	// Create resource with service information
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(ServiceName),
			semconv.ServiceVersion(getServiceVersion()),
		),
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithProcess(),
	)
	if err != nil {
		return nil, err
	}

	// Create OTLP HTTP trace exporter using autoexport
	// This supports configuration via environment variables like OTEL_EXPORTER_OTLP_ENDPOINT
	traceExporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		logger.Warn("Failed to create trace exporter, using no-op tracer", "error", err)
		otel.SetTracerProvider(noop.NewTracerProvider())
		tracer = otel.Tracer(ServiceName)
		return func(context.Context) error { return nil }, nil
	}

	// Create trace provider with instrumented processor
	batchProcessor := sdktrace.NewBatchSpanProcessor(traceExporter)
	instrumentedProcessor := &instrumentedSpanProcessor{next: batchProcessor}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(instrumentedProcessor),
		sdktrace.WithResource(res),
	)

	// Set global providers
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Get tracer for this service
	tracer = otel.Tracer(ServiceName)

	logger.Info("OpenTelemetry tracing initialized")

	// Return shutdown function
	return tracerProvider.Shutdown, nil
}

// GetTracer returns the global tracer instance
func GetTracer() trace.Tracer {
	return tracer
}

// getServiceVersion returns the service version from environment or default
func getServiceVersion() string {
	if version := os.Getenv("ALERTMANAGER_VERSION"); version != "" {
		return version
	}
	return ServiceVersion
}

// IsEnabled returns true if telemetry is enabled
func IsEnabled() bool {
	if tracer == nil {
		return false
	}

	// Test if tracer creates recording spans
	_, span := tracer.Start(context.Background(), "test")
	recording := span.IsRecording()
	span.End()
	return recording
}

// RegisterMetrics registers telemetry metrics with the given registerer
func RegisterMetrics(reg prometheus.Registerer) {
	reg.MustRegister(
		spansStarted,
		spansFinished,
		exportErrors,
		exportDuration,
	)
}

// instrumentedSpanProcessor wraps the span processor to add metrics
type instrumentedSpanProcessor struct {
	next sdktrace.SpanProcessor
}

func (p *instrumentedSpanProcessor) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	p.next.OnStart(parent, s)
	spansStarted.WithLabelValues(s.Name()).Inc()
}

func (p *instrumentedSpanProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	p.next.OnEnd(s)
	status := "ok"
	if s.Status().Code == codes.Error {
		status = "error"
	}
	spansFinished.WithLabelValues(s.Name(), status).Inc()
}

func (p *instrumentedSpanProcessor) Shutdown(ctx context.Context) error {
	return p.next.Shutdown(ctx)
}

func (p *instrumentedSpanProcessor) ForceFlush(ctx context.Context) error {
	return p.next.ForceFlush(ctx)
}
