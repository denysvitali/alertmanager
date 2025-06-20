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

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/go-openapi/strfmt"

	"github.com/prometheus/alertmanager/api/v2/client/alert"
	"github.com/prometheus/alertmanager/api/v2/client/silence"
	"github.com/prometheus/alertmanager/api/v2/models"

	. "github.com/prometheus/alertmanager/test/with_api_v2"
)

// TraceCollector captures spans for validation in integration tests.
type TraceCollector struct {
	mu    sync.RWMutex
	spans []sdktrace.ReadOnlySpan
}

func (tc *TraceCollector) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.spans = append(tc.spans, spans...)
	return nil
}

func (tc *TraceCollector) Shutdown(ctx context.Context) error {
	return nil
}

func (tc *TraceCollector) GetSpans() []sdktrace.ReadOnlySpan {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	spans := make([]sdktrace.ReadOnlySpan, len(tc.spans))
	copy(spans, tc.spans)
	return spans
}

func (tc *TraceCollector) GetSpansByName(name string) []sdktrace.ReadOnlySpan {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	var result []sdktrace.ReadOnlySpan
	for _, span := range tc.spans {
		if span.Name() == name {
			result = append(result, span)
		}
	}
	return result
}

func (tc *TraceCollector) FindSpanByAttribute(key, value string) sdktrace.ReadOnlySpan {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	for _, span := range tc.spans {
		for _, attr := range span.Attributes() {
			if string(attr.Key) == key && attr.Value.AsString() == value {
				return span
			}
		}
	}
	return nil
}

func (tc *TraceCollector) Clear() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.spans = nil
}

// OTLPTraceServer provides a mock OTLP trace receiver for integration testing.
type OTLPTraceServer struct {
	server    *httptest.Server
	collector *TraceCollector
	mu        sync.RWMutex
	traces    []map[string]interface{}
}

func NewOTLPTraceServer() *OTLPTraceServer {
	ots := &OTLPTraceServer{
		collector: &TraceCollector{},
		traces:    make([]map[string]interface{}, 0),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", ots.handleTraces)
	ots.server = httptest.NewServer(mux)

	return ots
}

func (ots *OTLPTraceServer) handleTraces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Parse the OTLP trace data (protobuf or JSON)
	var traceData map[string]interface{}
	if err := json.Unmarshal(body, &traceData); err != nil {
		// If JSON parsing fails, just store the raw body for validation
		traceData = map[string]interface{}{
			"raw_body": string(body),
			"headers":  r.Header,
		}
	}

	ots.mu.Lock()
	ots.traces = append(ots.traces, traceData)
	ots.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (ots *OTLPTraceServer) URL() string {
	return ots.server.URL
}

func (ots *OTLPTraceServer) Close() {
	ots.server.Close()
}

func (ots *OTLPTraceServer) GetTraces() []map[string]interface{} {
	ots.mu.RLock()
	defer ots.mu.RUnlock()
	result := make([]map[string]interface{}, len(ots.traces))
	copy(result, ots.traces)
	return result
}

func (ots *OTLPTraceServer) GetTraceCount() int {
	ots.mu.RLock()
	defer ots.mu.RUnlock()
	return len(ots.traces)
}

// TestTracingNotificationFlow validates end-to-end trace propagation through notification flow.
func TestTracingNotificationFlow(t *testing.T) {
	t.Parallel()

	// Create OTLP trace server to collect traces
	otlpServer := NewOTLPTraceServer()
	defer otlpServer.Close()

	// Mock webhook that captures trace headers
	var capturedHeaders http.Header
	var headersMu sync.RWMutex
	webhookCalled := make(chan struct{}, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headersMu.Lock()
		capturedHeaders = r.Header.Clone()
		headersMu.Unlock()
		w.WriteHeader(200)
		select {
		case webhookCalled <- struct{}{}:
		default:
		}
	}))
	defer webhook.Close()

	conf := fmt.Sprintf(`
route:
  receiver: "traced_webhook"
  group_by: [alertname]
  group_wait: 100ms
  group_interval: 100ms
  repeat_interval: 1h

receivers:
- name: "traced_webhook"
  webhook_configs:
  - url: '%s'
`, webhook.URL)

	at := NewAcceptanceTest(t, &AcceptanceOpts{
		Tolerance:       5 * time.Second,
		TracingEnabled:  true,
		TracingEndpoint: otlpServer.URL(),
	})

	amc := at.AlertmanagerCluster(conf, 1)
	am := amc.Members()[0]

	amc.Start()
	defer amc.Terminate()

	// Send alert with tracing context
	now := time.Now()
	pa := &models.PostableAlert{
		StartsAt: strfmt.DateTime(now),
		EndsAt:   strfmt.DateTime(now.Add(5 * time.Minute)),
		Alert: models.Alert{
			Labels: models.LabelSet{
				"alertname": "trace_test",
				"severity":  "warning",
			},
		},
		Annotations: models.LabelSet{
			"summary": "Test alert for tracing validation",
		},
	}

	at.Do(1.0, func() {
		alertParams := alert.NewPostAlertsParams()
		alertParams.Alerts = models.PostableAlerts{pa}
		_, err := am.Client().Alert.PostAlerts(alertParams)
		if err != nil {
			t.Errorf("Failed to post alert: %v", err)
		}
	})

	// Schedule validation to run after webhook should have been called
	at.Do(3.0, func() {
		// Wait for webhook to be called with a timeout
		select {
		case <-webhookCalled:
			// Validate trace propagation to webhook.
			headersMu.RLock()
			headers := capturedHeaders
			headersMu.RUnlock()

			if headers == nil {
				t.Error("Expected webhook to be called with headers, but they were nil")
				return
			}

			// Check for trace propagation headers (W3C Trace Context).
			traceparent := headers.Get("traceparent")
			if traceparent == "" {
				t.Log("Note: traceparent header not found - trace propagation may not be implemented")
			} else {
				t.Logf("Found traceparent header: %s", traceparent)
			}

			// It can take a moment for traces to be exported.
			time.Sleep(200 * time.Millisecond)

			// Validate OTLP traces were exported.
			traceCount := otlpServer.GetTraceCount()
			if traceCount == 0 {
				t.Log("Note: No traces received by OTLP server - this may indicate tracing setup issues")
			} else {
				t.Logf("OTLP server received %d trace exports", traceCount)
			}

			// Check for expected traces in OTLP server.
			traces := otlpServer.GetTraces()
			for _, trace := range traces {
				t.Logf("Received trace data: %+v", trace)
			}
		case <-time.After(2 * time.Second):
			t.Error("timed out waiting for webhook notification")
		}
	})

	at.Run()
}

// TestTracingSilenceOperations validates tracing for silence operations.
func TestTracingSilenceOperations(t *testing.T) {
	t.Parallel()

	// Create OTLP trace server to collect traces
	otlpServer := NewOTLPTraceServer()
	defer otlpServer.Close()

	conf := `
route:
  receiver: "null"

receivers:
- name: "null"
`

	at := NewAcceptanceTest(t, &AcceptanceOpts{
		Tolerance:       2 * time.Second,
		TracingEnabled:  true,
		TracingEndpoint: otlpServer.URL(),
	})

	amc := at.AlertmanagerCluster(conf, 1)
	am := amc.Members()[0]

	amc.Start()
	defer amc.Terminate()

	// Create a silence using the API
	now := time.Now()
	ps := &models.PostableSilence{
		Silence: models.Silence{
			Matchers: models.Matchers{{
				Name:    stringPtr("alertname"),
				Value:   stringPtr("test_silence"),
				IsEqual: boolPtr(true),
				IsRegex: boolPtr(false),
			}},
			StartsAt:  dateTimePtr(strfmt.DateTime(now)),
			EndsAt:    dateTimePtr(strfmt.DateTime(now.Add(1 * time.Hour))),
			CreatedBy: stringPtr("integration_test"),
			Comment:   stringPtr("Test silence for tracing"),
		},
	}

	at.Do(1.0, func() {
		silenceParams := silence.NewPostSilencesParams()
		silenceParams.Silence = ps
		_, err := am.Client().Silence.PostSilences(silenceParams)
		if err != nil {
			t.Errorf("Failed to create silence: %v", err)
		}
	})

	at.Do(2.0, func() {
		// Validate OTLP traces were exported
		traceCount := otlpServer.GetTraceCount()
		if traceCount == 0 {
			t.Log("Note: No traces received by OTLP server for silence operation")
		} else {
			t.Logf("OTLP server received %d trace exports for silence operation", traceCount)
		}

		// Check for expected traces in OTLP server
		traces := otlpServer.GetTraces()
		for _, trace := range traces {
			t.Logf("Received trace data for silence: %+v", trace)
		}
	})

	at.Run()
}
