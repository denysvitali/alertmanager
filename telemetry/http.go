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
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

// HTTPMiddleware returns an HTTP middleware that adds tracing to requests
func HTTPMiddleware(handlerName string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if tracer == nil {
				next(w, r)
				return
			}

			// Extract trace context from headers
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			// Start a new span
			spanName := handlerName
			if spanName == "" {
				spanName = r.Method + " " + r.URL.Path
			}

			attrs := []attribute.KeyValue{
				semconv.HTTPMethod(r.Method),
				semconv.HTTPRoute(r.URL.Path),
				semconv.HTTPScheme(r.URL.Scheme),
				semconv.HTTPTarget(r.URL.RequestURI()),
				semconv.UserAgentOriginal(r.UserAgent()),
				attribute.String("http.handler", handlerName),
			}

			if r.Host != "" {
				attrs = append(attrs, attribute.String("http.host", r.Host))
			}
			if r.URL.RawQuery != "" {
				attrs = append(attrs, attribute.Int("http.request.query_length", len(r.URL.RawQuery)))
			}

			ctx, span := tracer.Start(ctx, spanName, trace.WithAttributes(attrs...))
			defer span.End()

			// Create a response writer wrapper to capture status code
			wrapper := &responseWriterWrapper{
				ResponseWriter: w,
				statusCode:     200, // Default to 200 if WriteHeader is not called
			}

			// Call the next handler with the trace context
			next(wrapper, r.WithContext(ctx))

			// Add response attributes
			span.SetAttributes(
				semconv.HTTPStatusCode(wrapper.statusCode),
				attribute.Int("http.response.body_size", wrapper.bytesWritten),
			)

			// Set span status based on HTTP status code
			if wrapper.statusCode >= 400 {
				span.SetStatus(codes.Error, http.StatusText(wrapper.statusCode))
			} else {
				span.SetStatus(codes.Ok, "")
			}
		}
	}
}

// responseWriterWrapper wraps http.ResponseWriter to capture status code and response size
type responseWriterWrapper struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
}

func (w *responseWriterWrapper) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *responseWriterWrapper) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.bytesWritten += n
	return n, err
}

// InstrumentHandler creates a traced version of an HTTP handler
func InstrumentHandler(handlerName string, handler http.HandlerFunc) http.HandlerFunc {
	// Apply tracing middleware
	return HTTPMiddleware(handlerName)(handler)
}

// GetTracingHeaders extracts tracing headers for outgoing HTTP requests
func GetTracingHeaders(ctx context.Context) map[string]string {
	if tracer == nil {
		return nil
	}

	headers := make(map[string]string)
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(headers))
	return headers
}
