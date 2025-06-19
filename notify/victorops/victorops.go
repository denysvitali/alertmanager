// Copyright 2019 Prometheus Team
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

package victorops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	commoncfg "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/telemetry"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
)

// https://help.victorops.com/knowledge-base/incident-fields-glossary/ - 20480 characters.
const maxMessageLenRunes = 20480

// Notifier implements a Notifier for VictorOps notifications.
type Notifier struct {
	conf    *config.VictorOpsConfig
	tmpl    *template.Template
	logger  *slog.Logger
	client  *http.Client
	retrier *notify.Retrier
}

// New returns a new VictorOps notifier.
func New(c *config.VictorOpsConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*c.HTTPConfig, "victorops", httpOpts...)
	if err != nil {
		return nil, err
	}
	return &Notifier{
		conf:   c,
		tmpl:   t,
		logger: l,
		client: client,
		// Missing documentation therefore assuming only 5xx response codes are
		// recoverable.
		retrier: &notify.Retrier{},
	}, nil
}

const (
	victorOpsEventTrigger = "CRITICAL"
	victorOpsEventResolve = "RECOVERY"
)

// Notify implements the Notifier interface.
func (n *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "victorops.notify")
	defer span.End()

	var err error
	var (
		data   = notify.GetTemplateData(ctx, n.tmpl, as, n.logger)
		tmpl   = notify.TmplText(n.tmpl, data, &err)
		apiURL = n.conf.APIURL.Copy()
	)

	span.SetAttributes(
		attribute.Int("victorops.alerts_count", len(as)),
		attribute.String("victorops.api_url", apiURL.String()),
	)

	telemetry.AddEvent(ctx, "victorops.notification_start")

	var apiKey string
	if n.conf.APIKey != "" {
		apiKey = string(n.conf.APIKey)
	} else {
		telemetry.AddEvent(ctx, "victorops.reading_api_key_file")
		content, fileErr := os.ReadFile(n.conf.APIKeyFile)
		if fileErr != nil {
			span.RecordError(fileErr)
			span.SetStatus(codes.Error, "failed to read API key file")
			return false, fmt.Errorf("failed to read API key from file: %w", fileErr)
		}
		apiKey = strings.TrimSpace(string(content))
	}

	apiURL.Path += fmt.Sprintf("%s/%s", apiKey, tmpl(n.conf.RoutingKey))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "template execution failed")
		return false, fmt.Errorf("templating error: %w", err)
	}

	span.SetAttributes(attribute.String("victorops.routing_key", tmpl(n.conf.RoutingKey)))

	telemetry.AddEvent(ctx, "victorops.creating_payload")
	buf, err := n.createVictorOpsPayload(ctx, as...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "payload creation failed")
		return true, err
	}

	telemetry.AddEvent(ctx, "victorops.sending_request")
	resp, err := notify.PostJSON(ctx, n.client, apiURL.String(), buf)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "HTTP request failed")
		return true, notify.RedactURL(err)
	}
	defer notify.Drain(resp)

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	telemetry.AddEvent(ctx, "victorops.response_received", attribute.Int("status_code", resp.StatusCode))

	shouldRetry, err := n.retrier.Check(resp.StatusCode, resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "retry check failed")
		telemetry.AddEvent(ctx, "victorops.notification_failed",
			attribute.String("error", err.Error()),
			attribute.Bool("retry", shouldRetry))
		return shouldRetry, notify.NewErrorWithReason(notify.GetFailureReasonFromStatusCode(resp.StatusCode), err)
	}

	span.SetStatus(codes.Ok, "notification sent")
	telemetry.AddEvent(ctx, "victorops.notification_success")
	return shouldRetry, err
}

// Create the JSON payload to be sent to the VictorOps API.
func (n *Notifier) createVictorOpsPayload(ctx context.Context, as ...*types.Alert) (*bytes.Buffer, error) {
	ctx, span := telemetry.StartSpan(ctx, "victorops.create_payload")
	defer span.End()

	victorOpsAllowedEvents := map[string]bool{
		"INFO":     true,
		"WARNING":  true,
		"CRITICAL": true,
	}

	key, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to extract group key")
		return nil, err
	}

	var (
		alerts = types.Alerts(as...)
		data   = notify.GetTemplateData(ctx, n.tmpl, as, n.logger)
		tmpl   = notify.TmplText(n.tmpl, data, &err)

		messageType  = tmpl(n.conf.MessageType)
		stateMessage = tmpl(n.conf.StateMessage)
	)

	if alerts.Status() == model.AlertFiring && !victorOpsAllowedEvents[messageType] {
		messageType = victorOpsEventTrigger
		telemetry.AddEvent(ctx, "victorops.message_type_overridden",
			attribute.String("original_type", tmpl(n.conf.MessageType)),
			attribute.String("new_type", messageType))
	}

	if alerts.Status() == model.AlertResolved {
		messageType = victorOpsEventResolve
		telemetry.AddEvent(ctx, "victorops.alert_resolved", attribute.String("message_type", messageType))
	}

	stateMessage, truncated := notify.TruncateInRunes(stateMessage, maxMessageLenRunes)
	if truncated {
		telemetry.AddEvent(ctx, "victorops.state_message_truncated",
			attribute.Int("max_runes", maxMessageLenRunes))
		n.logger.Warn("Truncated state_message", "incident", key, "max_runes", maxMessageLenRunes)
	}

	span.SetAttributes(
		attribute.String("victorops.entity_id", key.Hash()),
		attribute.String("victorops.message_type", messageType),
		attribute.String("victorops.alert_status", string(alerts.Status())),
		attribute.Int("victorops.state_message_length", len(stateMessage)),
		attribute.Bool("victorops.state_message_truncated", truncated),
		attribute.Int("victorops.custom_fields_count", len(n.conf.CustomFields)),
	)

	msg := map[string]string{
		"message_type":        messageType,
		"entity_id":           key.Hash(),
		"entity_display_name": tmpl(n.conf.EntityDisplayName),
		"state_message":       stateMessage,
		"monitoring_tool":     tmpl(n.conf.MonitoringTool),
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "template execution failed")
		return nil, fmt.Errorf("templating error: %w", err)
	}

	// Add custom fields to the payload.
	for k, v := range n.conf.CustomFields {
		msg[k] = tmpl(v)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "custom field template execution failed")
			return nil, fmt.Errorf("templating error: %w", err)
		}
	}

	telemetry.AddEvent(ctx, "victorops.encoding_payload")
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "JSON encoding failed")
		return nil, err
	}

	span.SetStatus(codes.Ok, "payload created")
	return &buf, nil
}
