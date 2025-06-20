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
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func setupTestTracer() (*InMemoryExporter, func()) {
	exporter := &InMemoryExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)
	tracer = otel.Tracer(ServiceName)

	cleanup := func() {
		tp.Shutdown(context.Background())
	}
	return exporter, cleanup
}

func TestStartSpan(t *testing.T) {
	exporter, cleanup := setupTestTracer()
	defer cleanup()

	ctx, span := StartSpan(context.Background(), "test-span",
		attribute.String("test-key", "test-value"))
	span.End()

	if len(exporter.spans) != 1 {
		t.Errorf("Expected 1 span, got %d", len(exporter.spans))
	}

	span0 := exporter.spans[0]
	if span0.Name() != "test-span" {
		t.Errorf("Expected span name 'test-span', got '%s'", span0.Name())
	}

	// Check attributes
	attrs := span0.Attributes()
	found := false
	for _, attr := range attrs {
		if attr.Key == "test-key" && attr.Value.AsString() == "test-value" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected to find test-key attribute")
	}

	// Verify context contains span
	traceID := GetTraceID(ctx)
	if traceID == "" {
		t.Error("Expected non-empty trace ID from context")
	}
}

func TestStartSpan_NoTracer(t *testing.T) {
	// Save original tracer
	originalTracer := tracer
	defer func() { tracer = originalTracer }()

	// Set tracer to nil
	tracer = nil

	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	// Should not panic and should return the input context
	if ctx == nil {
		t.Error("Expected non-nil context")
	}

	if span == nil {
		t.Error("Expected non-nil span")
	}
}

func TestSetError(t *testing.T) {
	exporter, cleanup := setupTestTracer()
	defer cleanup()

	ctx, span := StartSpan(context.Background(), "test-span")

	testErr := errors.New("test error")
	SetError(ctx, testErr)

	span.End()

	if len(exporter.spans) != 1 {
		t.Errorf("Expected 1 span, got %d", len(exporter.spans))
	}

	span0 := exporter.spans[0]
	status := span0.Status()
	if status.Code != codes.Error {
		t.Errorf("Expected error status, got %v", status.Code)
	}

	if status.Description != "test error" {
		t.Errorf("Expected error description 'test error', got '%s'", status.Description)
	}

	// Check if error event was recorded
	events := span0.Events()
	found := false
	for _, event := range events {
		if event.Name == "exception" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected to find exception event")
	}
}

func TestSetStatus(t *testing.T) {
	exporter, cleanup := setupTestTracer()
	defer cleanup()

	ctx, span := StartSpan(context.Background(), "test-span")

	SetStatus(ctx, codes.Ok, "success")

	span.End()

	if len(exporter.spans) != 1 {
		t.Errorf("Expected 1 span, got %d", len(exporter.spans))
	}

	span0 := exporter.spans[0]
	status := span0.Status()
	if status.Code != codes.Ok {
		t.Errorf("Expected ok status, got %v", status.Code)
	}

	// Note: OK status descriptions may be empty in some implementations
	// Let's test with Error status which should preserve description
	ctx2, span2 := StartSpan(context.Background(), "test-span-error")
	SetStatus(ctx2, codes.Error, "error occurred")
	span2.End()

	if len(exporter.spans) != 2 {
		t.Errorf("Expected 2 spans, got %d", len(exporter.spans))
	}

	span1 := exporter.spans[1]
	errorStatus := span1.Status()
	if errorStatus.Code != codes.Error {
		t.Errorf("Expected error status, got %v", errorStatus.Code)
	}

	if errorStatus.Description != "error occurred" {
		t.Errorf("Expected error description 'error occurred', got '%s'", errorStatus.Description)
	}
}

func TestAddEvent(t *testing.T) {
	exporter, cleanup := setupTestTracer()
	defer cleanup()

	ctx, span := StartSpan(context.Background(), "test-span")

	AddEvent(ctx, "test-event", attribute.String("event-key", "event-value"))

	span.End()

	if len(exporter.spans) != 1 {
		t.Errorf("Expected 1 span, got %d", len(exporter.spans))
	}

	span0 := exporter.spans[0]
	events := span0.Events()
	found := false
	for _, event := range events {
		if event.Name == "test-event" {
			found = true
			// Check event attributes
			for _, attr := range event.Attributes {
				if attr.Key == "event-key" && attr.Value.AsString() == "event-value" {
					break
				}
			}
			break
		}
	}
	if !found {
		t.Error("Expected to find test-event")
	}
}

func TestWithAlertAttributes(t *testing.T) {
	attrs := WithAlertAttributes("TestAlert", "test-group", 5)

	expectedAttrs := map[string]interface{}{
		AlertNameKey:  "TestAlert",
		AlertGroupKey: "test-group",
		AlertCountKey: 5,
	}

	if len(attrs) != 3 {
		t.Errorf("Expected 3 attributes, got %d", len(attrs))
	}

	for _, attr := range attrs {
		key := string(attr.Key)
		expected, exists := expectedAttrs[key]
		if !exists {
			t.Errorf("Unexpected attribute key: %s", key)
			continue
		}

		switch key {
		case AlertNameKey, AlertGroupKey:
			if attr.Value.AsString() != expected.(string) {
				t.Errorf("Expected %s=%s, got %s", key, expected, attr.Value.AsString())
			}
		case AlertCountKey:
			if attr.Value.AsInt64() != int64(expected.(int)) {
				t.Errorf("Expected %s=%d, got %d", key, expected, attr.Value.AsInt64())
			}
		}
	}
}

func TestWithNotificationAttributes(t *testing.T) {
	attrs := WithNotificationAttributes("test-receiver", "slack")

	if len(attrs) != 2 {
		t.Errorf("Expected 2 attributes, got %d", len(attrs))
	}

	var receiverFound, typeFound bool
	for _, attr := range attrs {
		switch string(attr.Key) {
		case NotificationReceiverKey:
			if attr.Value.AsString() != "test-receiver" {
				t.Errorf("Expected receiver 'test-receiver', got '%s'", attr.Value.AsString())
			}
			receiverFound = true
		case NotificationTypeKey:
			if attr.Value.AsString() != "slack" {
				t.Errorf("Expected type 'slack', got '%s'", attr.Value.AsString())
			}
			typeFound = true
		}
	}

	if !receiverFound {
		t.Error("Expected to find receiver attribute")
	}
	if !typeFound {
		t.Error("Expected to find type attribute")
	}
}

func TestTraceFunc(t *testing.T) {
	exporter, cleanup := setupTestTracer()
	defer cleanup()

	testErr := errors.New("test error")

	// Test successful function
	err := TraceFunc(context.Background(), "test-func", func(ctx context.Context) error {
		return nil
	})
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	// Test error function
	err = TraceFunc(context.Background(), "test-func-error", func(ctx context.Context) error {
		return testErr
	})

	if !errors.Is(err, testErr) {
		t.Errorf("Expected test error, got %v", err)
	}

	if len(exporter.spans) != 2 {
		t.Errorf("Expected 2 spans, got %d", len(exporter.spans))
	}

	// Check successful span
	successSpan := exporter.spans[0]
	if successSpan.Name() != "test-func" {
		t.Errorf("Expected span name 'test-func', got '%s'", successSpan.Name())
	}
	if successSpan.Status().Code != codes.Unset {
		t.Errorf("Expected unset status for success, got %v", successSpan.Status().Code)
	}

	// Check error span
	errorSpan := exporter.spans[1]
	if errorSpan.Name() != "test-func-error" {
		t.Errorf("Expected span name 'test-func-error', got '%s'", errorSpan.Name())
	}
	if errorSpan.Status().Code != codes.Error {
		t.Errorf("Expected error status, got %v", errorSpan.Status().Code)
	}
}

func TestTraceFuncWithResult(t *testing.T) {
	exporter, cleanup := setupTestTracer()
	defer cleanup()

	// Test successful function with result
	result, err := TraceFuncWithResult(context.Background(), "test-func-result", func(ctx context.Context) (string, error) {
		return "success", nil
	})
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result != "success" {
		t.Errorf("Expected result 'success', got '%s'", result)
	}

	// Test error function
	testErr := errors.New("test error")
	result, err = TraceFuncWithResult(context.Background(), "test-func-error", func(ctx context.Context) (string, error) {
		return "", testErr
	})

	if !errors.Is(err, testErr) {
		t.Errorf("Expected test error, got %v", err)
	}
	if result != "" {
		t.Errorf("Expected empty result, got '%s'", result)
	}

	if len(exporter.spans) != 2 {
		t.Errorf("Expected 2 spans, got %d", len(exporter.spans))
	}
}

func TestGetTraceID_NoSpan(t *testing.T) {
	traceID := GetTraceID(context.Background())
	if traceID != "" {
		t.Errorf("Expected empty trace ID, got '%s'", traceID)
	}
}

func TestGetSpanID_NoSpan(t *testing.T) {
	spanID := GetSpanID(context.Background())
	if spanID != "" {
		t.Errorf("Expected empty span ID, got '%s'", spanID)
	}
}

func TestLogFields(t *testing.T) {
	_, cleanup := setupTestTracer()
	defer cleanup()

	ctx, span := StartSpan(context.Background(), "test-span")

	fields := LogFields(ctx)

	// Should contain trace_id and span_id
	if len(fields) != 4 { // key-value pairs: trace_id, <value>, span_id, <value>
		t.Errorf("Expected 4 log fields, got %d", len(fields))
	}

	// Check that we have the expected keys
	foundTraceID := false
	foundSpanID := false
	for i := 0; i < len(fields); i += 2 {
		if i+1 >= len(fields) {
			break
		}
		key, ok := fields[i].(string)
		if !ok {
			continue
		}
		switch key {
		case "trace_id":
			foundTraceID = true
		case "span_id":
			foundSpanID = true
		}
	}

	if !foundTraceID {
		t.Error("Expected to find trace_id in log fields")
	}
	if !foundSpanID {
		t.Error("Expected to find span_id in log fields")
	}

	span.End()
}

func TestLogFields_NoSpan(t *testing.T) {
	fields := LogFields(context.Background())

	// Should be empty when no span is present
	if len(fields) != 0 {
		t.Errorf("Expected 0 log fields, got %d", len(fields))
	}
}

func TestSanitizeURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"http://example.com", "<redacted>"},
		{"https://api.example.com/webhook?token=secret", "<redacted>"},
	}

	for _, test := range tests {
		result := SanitizeURL(test.input)
		if result != test.expected {
			t.Errorf("SanitizeURL(%s) = %s, want %s", test.input, result, test.expected)
		}
	}
}
