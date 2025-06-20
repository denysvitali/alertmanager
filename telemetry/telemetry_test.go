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
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestInitialize_Disabled(t *testing.T) {
	// Set OTEL_SDK_DISABLED to ensure no automatic initialization
	os.Setenv("OTEL_SDK_DISABLED", "true")
	defer os.Unsetenv("OTEL_SDK_DISABLED")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := Config{
		Enabled: false,
		Logger:  logger,
	}

	shutdown, err := Initialize(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer shutdown(context.Background())

	if IsEnabled() {
		t.Error("Expected tracing to be disabled")
	}

	// Verify no-op tracer is set
	tracer := otel.Tracer("test")
	_, span := tracer.Start(context.Background(), "test")
	if span.IsRecording() {
		t.Error("Expected no-op span that doesn't record")
	}
	span.End()
}

func TestInitialize_Enabled_NoExporter(t *testing.T) {
	// Ensure OTEL_SDK_DISABLED is not set
	os.Unsetenv("OTEL_SDK_DISABLED")

	// Clear any OTLP endpoint that might cause autoexport to work
	os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	os.Unsetenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := Config{
		Enabled: true,
		Logger:  logger,
	}

	shutdown, err := Initialize(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer shutdown(context.Background())

	// When no exporter is configured, it should fall back to no-op
	// but the function might still succeed
	tracer := otel.Tracer("test")
	_, span := tracer.Start(context.Background(), "test")
	defer span.End()

	// The behavior here depends on autoexport fallback
	// Just verify it doesn't crash
}

func TestInitialize_WithInMemoryExporter(t *testing.T) {
	// Save original tracer
	originalTracer := tracer
	defer func() { tracer = originalTracer }()

	// Create a custom tracer provider with in-memory exporter for testing
	exporter := &InMemoryExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)
	tracer = otel.Tracer(ServiceName)

	defer func() {
		tp.Shutdown(context.Background())
	}()

	// Test span creation
	ctx, span := StartSpan(context.Background(), "test-span")
	span.End()

	// Verify span was recorded
	if len(exporter.spans) != 1 {
		t.Errorf("Expected 1 span, got %d", len(exporter.spans))
	}

	if len(exporter.spans) > 0 && exporter.spans[0].Name() != "test-span" {
		t.Errorf("Expected span name 'test-span', got '%s'", exporter.spans[0].Name())
	}

	// Test trace ID extraction
	traceID := GetTraceID(ctx)
	if traceID == "" {
		t.Error("Expected non-empty trace ID")
	}
}

func TestGetServiceVersion(t *testing.T) {
	// Test default version
	version := getServiceVersion()
	if version != ServiceVersion {
		t.Errorf("Expected default version %s, got %s", ServiceVersion, version)
	}

	// Test environment variable
	os.Setenv("ALERTMANAGER_VERSION", "v0.27.0")
	defer os.Unsetenv("ALERTMANAGER_VERSION")

	version = getServiceVersion()
	if version != "v0.27.0" {
		t.Errorf("Expected version 'v0.27.0', got %s", version)
	}
}

// InMemoryExporter is a test exporter that stores spans in memory.
type InMemoryExporter struct {
	spans []sdktrace.ReadOnlySpan
}

func (e *InMemoryExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *InMemoryExporter) Shutdown(ctx context.Context) error {
	return nil
}
