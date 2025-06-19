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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Common attribute keys for AlertManager tracing
const (
	// Alert attributes
	AlertNameKey         = "alert.name"
	AlertGroupKey        = "alert.group"
	AlertCountKey        = "alert.count"
	AlertStatusKey       = "alert.status"
	AlertFingerprintKey  = "alert.fingerprint"
	AlertGeneratorURLKey = "alert.generator_url"

	// Notification attributes
	NotificationReceiverKey = "notification.receiver"
	NotificationTypeKey     = "notification.type"
	NotificationURLKey      = "notification.url"
	NotificationRetryKey    = "notification.retry"
	NotificationStatusKey   = "notification.status"

	// Routing attributes
	RouteMatcherKey  = "route.matcher"
	RouteReceiverKey = "route.receiver"
	RouteGroupByKey  = "route.group_by"

	// HTTP attributes
	HTTPMethodKey     = "http.method"
	HTTPURLKey        = "http.url"
	HTTPStatusCodeKey = "http.status_code"
	HTTPUserAgentKey  = "http.user_agent"

	// Silence attributes
	SilenceIDKey       = "silence.id"
	SilenceMatchersKey = "silence.matchers"

	// Template attributes
	TemplateNameKey = "template.name"
	TemplateTypeKey = "template.type"
)

// StartSpan starts a new span with the given name and attributes
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}

	return tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

// AddEvent adds an event to the current span
func AddEvent(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.AddEvent(name, trace.WithAttributes(attrs...))
	}
}

// SetAttributes sets attributes on the current span
func SetAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetAttributes(attrs...)
	}
}

// SetError records an error on the current span
func SetError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() && err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

// SetStatus sets the status of the current span
func SetStatus(ctx context.Context, code codes.Code, description string) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetStatus(code, description)
	}
}

// WithAlertAttributes returns alert-related attributes
func WithAlertAttributes(alertname, group string, count int) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(AlertNameKey, alertname),
		attribute.Int(AlertCountKey, count),
	}
	if group != "" {
		attrs = append(attrs, attribute.String(AlertGroupKey, group))
	}
	return attrs
}

// WithNotificationAttributes returns notification-related attributes
func WithNotificationAttributes(receiver, notificationType string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(NotificationReceiverKey, receiver),
		attribute.String(NotificationTypeKey, notificationType),
	}
}

// WithHTTPAttributes returns HTTP-related attributes
func WithHTTPAttributes(method, url string, statusCode int) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(HTTPMethodKey, method),
		attribute.Int(HTTPStatusCodeKey, statusCode),
	}
	if url != "" {
		attrs = append(attrs, attribute.String(HTTPURLKey, url))
	}
	return attrs
}

// WithRouteAttributes returns routing-related attributes
func WithRouteAttributes(receiver string, matchers []string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(RouteReceiverKey, receiver),
	}
	if len(matchers) > 0 {
		attrs = append(attrs, attribute.StringSlice(RouteMatcherKey, matchers))
	}
	return attrs
}

// FinishSpan finishes a span with optional error
func FinishSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// TraceFunc wraps a function with tracing
func TraceFunc(ctx context.Context, name string, fn func(ctx context.Context) error, attrs ...attribute.KeyValue) error {
	ctx, span := StartSpan(ctx, name, attrs...)
	defer span.End()

	err := fn(ctx)
	if err != nil {
		SetError(ctx, err)
	}
	return err
}

// TraceFuncWithResult wraps a function with tracing and returns a result
func TraceFuncWithResult[T any](ctx context.Context, name string, fn func(ctx context.Context) (T, error), attrs ...attribute.KeyValue) (T, error) {
	ctx, span := StartSpan(ctx, name, attrs...)
	defer span.End()

	result, err := fn(ctx)
	if err != nil {
		SetError(ctx, err)
	}
	return result, err
}

// GetTraceID returns the trace ID from the current span context
func GetTraceID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().IsValid() {
		return span.SpanContext().TraceID().String()
	}
	return ""
}

// GetSpanID returns the span ID from the current span context
func GetSpanID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().IsValid() {
		return span.SpanContext().SpanID().String()
	}
	return ""
}

// LogFields returns structured log fields with trace context
func LogFields(ctx context.Context) []any {
	traceID := GetTraceID(ctx)
	spanID := GetSpanID(ctx)

	fields := make([]any, 0, 4)
	if traceID != "" {
		fields = append(fields, "trace_id", traceID)
	}
	if spanID != "" {
		fields = append(fields, "span_id", spanID)
	}
	return fields
}

// SanitizeURL removes sensitive information from URLs for tracing
func SanitizeURL(url string) string {
	// This is a simple implementation - in production you might want
	// more sophisticated URL sanitization
	if url == "" {
		return ""
	}
	return "<redacted>"
}
