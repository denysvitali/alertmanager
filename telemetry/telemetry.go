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

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	ServiceName    = "alertmanager"
	ServiceVersion = "dev" // This should be set during build
)

var (
	tracer trace.Tracer
	logger *slog.Logger
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
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
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
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		tracer = otel.Tracer(ServiceName)
		return func(context.Context) error { return nil }, nil
	}

	// Create trace provider
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
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
	return tracer != nil && tracer != trace.NewNoopTracerProvider().Tracer(ServiceName)
}
