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
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// BenchmarkTracingOverhead measures the performance impact of tracing operations.
func BenchmarkTracingOverhead(b *testing.B) {
	// Test different scenarios
	b.Run("NoOp", benchmarkNoOpTracing)
	b.Run("WithTracing", benchmarkWithTracing)
	b.Run("SpanCreation", benchmarkSpanCreation)
	b.Run("AttributeSetting", benchmarkAttributeSetting)
	b.Run("EventLogging", benchmarkEventLogging)
}

func benchmarkNoOpTracing(b *testing.B) {
	// Setup no-op tracer
	otel.SetTracerProvider(noop.NewTracerProvider())
	tracer := otel.Tracer("benchmark")

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ctx, span := tracer.Start(context.Background(), "benchmark-span")
			span.SetAttributes(
				attribute.String("test.key", "test.value"),
				attribute.Int("test.count", 42),
			)
			span.AddEvent("test.event")
			span.End()
			_ = ctx
		}
	})
}

func benchmarkWithTracing(b *testing.B) {
	// Setup real tracer with no-op exporter for benchmarking
	exporter := &NoOpExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)
	tracer := otel.Tracer("benchmark")

	defer tp.Shutdown(context.Background())

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ctx, span := tracer.Start(context.Background(), "benchmark-span")
			span.SetAttributes(
				attribute.String("test.key", "test.value"),
				attribute.Int("test.count", 42),
			)
			span.AddEvent("test.event")
			span.End()
			_ = ctx
		}
	})
}

func benchmarkSpanCreation(b *testing.B) {
	exporter := &NoOpExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)
	tracer := otel.Tracer("benchmark")

	defer tp.Shutdown(context.Background())

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, span := tracer.Start(context.Background(), "benchmark-span")
			span.End()
		}
	})
}

func benchmarkAttributeSetting(b *testing.B) {
	exporter := &NoOpExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)
	tracer := otel.Tracer("benchmark")

	defer tp.Shutdown(context.Background())

	_, span := tracer.Start(context.Background(), "benchmark-span")
	defer span.End()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			span.SetAttributes(
				attribute.String("test.key1", "value1"),
				attribute.String("test.key2", "value2"),
				attribute.Int("test.number", 123),
				attribute.Bool("test.flag", true),
			)
		}
	})
}

func benchmarkEventLogging(b *testing.B) {
	exporter := &NoOpExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)
	tracer := otel.Tracer("benchmark")

	defer tp.Shutdown(context.Background())

	_, span := tracer.Start(context.Background(), "benchmark-span")
	defer span.End()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			span.AddEvent("benchmark.event")
		}
	})
}

// BenchmarkAlertManagerOperations measures the overhead in realistic AlertManager scenarios.
func BenchmarkAlertManagerOperations(b *testing.B) {
	b.Run("AlertIngestion", benchmarkAlertIngestion)
	b.Run("NotificationSend", benchmarkNotificationSend)
	b.Run("SilenceCheck", benchmarkSilenceCheck)
}

func benchmarkAlertIngestion(b *testing.B) {
	// Setup tracer
	exporter := &NoOpExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)
	tracer := otel.Tracer("alertmanager")

	defer tp.Shutdown(context.Background())

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Simulate alert ingestion tracing
			ctx, span := tracer.Start(context.Background(), "alert.ingestion")

			span.SetAttributes(
				attribute.String("alert.name", "TestAlert"),
				attribute.String("alert.severity", "warning"),
				attribute.Int("alert.count", 1),
				attribute.String("alert.fingerprint", "abc123"),
			)

			// Simulate processing steps
			span.AddEvent("alert.validation")
			span.AddEvent("alert.routing")
			span.AddEvent("alert.storage")

			span.End()
			_ = ctx
		}
	})
}

func benchmarkNotificationSend(b *testing.B) {
	exporter := &NoOpExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)
	tracer := otel.Tracer("alertmanager")

	defer tp.Shutdown(context.Background())

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Simulate notification sending tracing
			ctx, span := tracer.Start(context.Background(), "notification.webhook.send")

			span.SetAttributes(
				attribute.String("notification.receiver", "webhook-receiver"),
				attribute.String("notification.type", "webhook"),
				attribute.String("notification.url", "<redacted>"),
				attribute.String("alert.name", "TestAlert"),
				attribute.Int("alert.count", 3),
			)

			// Simulate notification steps
			span.AddEvent("template.render")
			span.AddEvent("http.request")
			span.AddEvent("http.response")

			span.End()
			_ = ctx
		}
	})
}

func benchmarkSilenceCheck(b *testing.B) {
	exporter := &NoOpExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)
	tracer := otel.Tracer("alertmanager")

	defer tp.Shutdown(context.Background())

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Simulate silence checking tracing
			ctx, span := tracer.Start(context.Background(), "silence.check")

			span.SetAttributes(
				attribute.String("alert.name", "TestAlert"),
				attribute.String("alert.fingerprint", "abc123"),
				attribute.Int("silence.count", 10),
			)

			span.AddEvent("silence.query")
			span.AddEvent("silence.match")

			span.End()
			_ = ctx
		}
	})
}

// NoOpExporter is used for benchmarking to avoid I/O overhead.
type NoOpExporter struct{}

func (e *NoOpExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	// Do nothing - this is for performance testing
	return nil
}

func (e *NoOpExporter) Shutdown(ctx context.Context) error {
	return nil
}

// BenchmarkTracingUtilities benchmarks the utility functions.
func BenchmarkTracingUtilities(b *testing.B) {
	b.Run("WithAlertAttributes", benchmarkWithAlertAttributes)
	b.Run("WithNotificationAttributes", benchmarkWithNotificationAttributes)
	b.Run("TraceFunc", benchmarkTraceFunc)
}

func benchmarkWithAlertAttributes(b *testing.B) {
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			attrs := WithAlertAttributes("TestAlert", "test-group", 5)
			_ = attrs
		}
	})
}

func benchmarkWithNotificationAttributes(b *testing.B) {
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			attrs := WithNotificationAttributes("test-receiver", "webhook")
			_ = attrs
		}
	})
}

func benchmarkTraceFunc(b *testing.B) {
	exporter := &NoOpExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)
	tracer = otel.Tracer("alertmanager")

	defer tp.Shutdown(context.Background())

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := TraceFunc(context.Background(), "benchmark-func", func(ctx context.Context) error {
				// Simulate some work
				return nil
			})
			_ = err
		}
	})
}
