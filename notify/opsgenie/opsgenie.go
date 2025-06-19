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

package opsgenie

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

// https://docs.opsgenie.com/docs/alert-api - 130 characters meaning runes.
const maxMessageLenRunes = 130

// Notifier implements a Notifier for OpsGenie notifications.
type Notifier struct {
	conf    *config.OpsGenieConfig
	tmpl    *template.Template
	logger  *slog.Logger
	client  *http.Client
	retrier *notify.Retrier
}

// New returns a new OpsGenie notifier.
func New(c *config.OpsGenieConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*c.HTTPConfig, "opsgenie", httpOpts...)
	if err != nil {
		return nil, err
	}
	return &Notifier{
		conf:    c,
		tmpl:    t,
		logger:  l,
		client:  client,
		retrier: &notify.Retrier{RetryCodes: []int{http.StatusTooManyRequests}},
	}, nil
}

type opsGenieCreateMessage struct {
	Alias       string                           `json:"alias"`
	Message     string                           `json:"message"`
	Description string                           `json:"description,omitempty"`
	Details     map[string]string                `json:"details"`
	Source      string                           `json:"source"`
	Responders  []opsGenieCreateMessageResponder `json:"responders,omitempty"`
	Tags        []string                         `json:"tags,omitempty"`
	Note        string                           `json:"note,omitempty"`
	Priority    string                           `json:"priority,omitempty"`
	Entity      string                           `json:"entity,omitempty"`
	Actions     []string                         `json:"actions,omitempty"`
}

type opsGenieCreateMessageResponder struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Username string `json:"username,omitempty"`
	Type     string `json:"type"` // team, user, escalation, schedule etc.
}

type opsGenieCloseMessage struct {
	Source string `json:"source"`
}

type opsGenieUpdateMessageMessage struct {
	Message string `json:"message,omitempty"`
}

type opsGenieUpdateDescriptionMessage struct {
	Description string `json:"description,omitempty"`
}

// Notify implements the Notifier interface.
func (n *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "opsgenie.notify")
	defer span.End()

	span.SetAttributes(attribute.Int("opsgenie.alerts_count", len(as)))
	telemetry.AddEvent(ctx, "opsgenie.notification_start")

	requests, retry, err := n.createRequests(ctx, as...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to create requests")
		return retry, err
	}

	span.SetAttributes(attribute.Int("opsgenie.requests_count", len(requests)))
	telemetry.AddEvent(ctx, "opsgenie.requests_created", attribute.Int("count", len(requests)))

	for i, req := range requests {
		_, reqSpan := telemetry.StartSpan(ctx, "opsgenie.send_request")
		reqSpan.SetAttributes(
			attribute.Int("opsgenie.request_index", i),
			attribute.String("opsgenie.request_method", req.Method),
			attribute.String("opsgenie.request_url", req.URL.Path),
		)

		req.Header.Set("User-Agent", notify.UserAgentHeader)
		resp, err := n.client.Do(req)
		if err != nil {
			reqSpan.RecordError(err)
			reqSpan.SetStatus(codes.Error, "HTTP request failed")
			reqSpan.End()
			span.RecordError(err)
			span.SetStatus(codes.Error, "HTTP request failed")
			return true, err
		}

		reqSpan.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
		telemetry.AddEvent(ctx, "opsgenie.response_received",
			attribute.Int("request_index", i),
			attribute.Int("status_code", resp.StatusCode))

		shouldRetry, err := n.retrier.Check(resp.StatusCode, resp.Body)
		notify.Drain(resp)
		if err != nil {
			reqSpan.RecordError(err)
			reqSpan.SetStatus(codes.Error, "retry check failed")
			reqSpan.End()
			span.RecordError(err)
			span.SetStatus(codes.Error, "request failed")
			telemetry.AddEvent(ctx, "opsgenie.request_failed",
				attribute.Int("request_index", i),
				attribute.String("error", err.Error()),
				attribute.Bool("retry", shouldRetry))
			return shouldRetry, notify.NewErrorWithReason(notify.GetFailureReasonFromStatusCode(resp.StatusCode), err)
		}

		reqSpan.SetStatus(codes.Ok, "request successful")
		reqSpan.End()
	}

	span.SetStatus(codes.Ok, "all requests successful")
	telemetry.AddEvent(ctx, "opsgenie.notification_success")
	return true, nil
}

// Like Split but filter out empty strings.
func safeSplit(s, sep string) []string {
	a := strings.Split(strings.TrimSpace(s), sep)
	b := a[:0]
	for _, x := range a {
		if x != "" {
			b = append(b, x)
		}
	}
	return b
}

// Create requests for a list of alerts.
func (n *Notifier) createRequests(ctx context.Context, as ...*types.Alert) ([]*http.Request, bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "opsgenie.create_requests")
	defer span.End()

	key, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to extract group key")
		return nil, false, err
	}
	telemetry.AddEvent(ctx, "opsgenie.template_data_preparation")
	data := notify.GetTemplateData(ctx, n.tmpl, as, n.logger)

	n.logger.Debug("extracted group key", "key", key)

	telemetry.AddEvent(ctx, "opsgenie.template_processing")
	tmpl := notify.TmplText(n.tmpl, data, &err)

	telemetry.AddEvent(ctx, "opsgenie.template_execution")
	details := make(map[string]string)

	for k, v := range data.CommonLabels {
		details[k] = v
	}

	for k, v := range n.conf.Details {
		details[k] = tmpl(v)
	}

	requests := []*http.Request{}

	var (
		alias  = key.Hash()
		alerts = types.Alerts(as...)
	)

	span.SetAttributes(
		attribute.String("opsgenie.alias", alias),
		attribute.String("opsgenie.alert_status", string(alerts.Status())),
		attribute.Int("opsgenie.details_count", len(details)),
	)

	switch alerts.Status() {
	case model.AlertResolved:
		telemetry.AddEvent(ctx, "opsgenie.creating_close_request")
		resolvedEndpointURL := n.conf.APIURL.Copy()
		resolvedEndpointURL.Path += fmt.Sprintf("v2/alerts/%s/close", alias)
		q := resolvedEndpointURL.Query()
		q.Set("identifierType", "alias")
		resolvedEndpointURL.RawQuery = q.Encode()
		msg := &opsGenieCloseMessage{Source: tmpl(n.conf.Source)}
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(msg); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "JSON encoding failed")
			return nil, false, err
		}
		req, err := http.NewRequest("POST", resolvedEndpointURL.String(), &buf)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "request creation failed")
			return nil, true, err
		}
		requests = append(requests, req.WithContext(ctx))
	default:
		telemetry.AddEvent(ctx, "opsgenie.creating_alert_request")
		message, truncated := notify.TruncateInRunes(tmpl(n.conf.Message), maxMessageLenRunes)
		if truncated {
			telemetry.AddEvent(ctx, "opsgenie.message_truncated",
				attribute.Int("max_runes", maxMessageLenRunes))
			n.logger.Warn("Truncated message", "alert", key, "max_runes", maxMessageLenRunes)
		}

		createEndpointURL := n.conf.APIURL.Copy()
		createEndpointURL.Path += "v2/alerts"

		var responders []opsGenieCreateMessageResponder
		for _, r := range n.conf.Responders {
			responder := opsGenieCreateMessageResponder{
				ID:       tmpl(r.ID),
				Name:     tmpl(r.Name),
				Username: tmpl(r.Username),
				Type:     tmpl(r.Type),
			}

			if responder == (opsGenieCreateMessageResponder{}) {
				// Filter out empty responders. This is useful if you want to fill
				// responders dynamically from alert's common labels.
				continue
			}

			if responder.Type == "teams" {
				teams := safeSplit(responder.Name, ",")
				for _, team := range teams {
					newResponder := opsGenieCreateMessageResponder{
						Name: tmpl(team),
						Type: tmpl("team"),
					}
					responders = append(responders, newResponder)
				}
				continue
			}

			responders = append(responders, responder)
		}

		span.SetAttributes(
			attribute.Int("opsgenie.responders_count", len(responders)),
			attribute.Int("opsgenie.message_length", len(message)),
			attribute.Bool("opsgenie.message_truncated", truncated),
			attribute.String("opsgenie.priority", tmpl(n.conf.Priority)),
		)

		msg := &opsGenieCreateMessage{
			Alias:       alias,
			Message:     message,
			Description: tmpl(n.conf.Description),
			Details:     details,
			Source:      tmpl(n.conf.Source),
			Responders:  responders,
			Tags:        safeSplit(tmpl(n.conf.Tags), ","),
			Note:        tmpl(n.conf.Note),
			Priority:    tmpl(n.conf.Priority),
			Entity:      tmpl(n.conf.Entity),
			Actions:     safeSplit(tmpl(n.conf.Actions), ","),
		}
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(msg); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "JSON encoding failed")
			return nil, false, err
		}
		req, err := http.NewRequest("POST", createEndpointURL.String(), &buf)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "request creation failed")
			return nil, true, err
		}
		requests = append(requests, req.WithContext(ctx))

		if n.conf.UpdateAlerts {
			telemetry.AddEvent(ctx, "opsgenie.creating_update_requests")
			span.SetAttributes(attribute.Bool("opsgenie.update_alerts", true))

			updateMessageEndpointURL := n.conf.APIURL.Copy()
			updateMessageEndpointURL.Path += fmt.Sprintf("v2/alerts/%s/message", alias)
			q := updateMessageEndpointURL.Query()
			q.Set("identifierType", "alias")
			updateMessageEndpointURL.RawQuery = q.Encode()
			updateMsgMsg := &opsGenieUpdateMessageMessage{
				Message: msg.Message,
			}
			var updateMessageBuf bytes.Buffer
			if err := json.NewEncoder(&updateMessageBuf).Encode(updateMsgMsg); err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "JSON encoding failed")
				return nil, false, err
			}
			req, err := http.NewRequest("PUT", updateMessageEndpointURL.String(), &updateMessageBuf)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "request creation failed")
				return nil, true, err
			}
			requests = append(requests, req)

			updateDescriptionEndpointURL := n.conf.APIURL.Copy()
			updateDescriptionEndpointURL.Path += fmt.Sprintf("v2/alerts/%s/description", alias)
			q = updateDescriptionEndpointURL.Query()
			q.Set("identifierType", "alias")
			updateDescriptionEndpointURL.RawQuery = q.Encode()
			updateDescMsg := &opsGenieUpdateDescriptionMessage{
				Description: msg.Description,
			}

			var updateDescriptionBuf bytes.Buffer
			if err := json.NewEncoder(&updateDescriptionBuf).Encode(updateDescMsg); err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "JSON encoding failed")
				return nil, false, err
			}
			req, err = http.NewRequest("PUT", updateDescriptionEndpointURL.String(), &updateDescriptionBuf)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "request creation failed")
				return nil, true, err
			}
			requests = append(requests, req.WithContext(ctx))
		}
	}

	var apiKey string
	if n.conf.APIKey != "" {
		apiKey = tmpl(string(n.conf.APIKey))
	} else {
		telemetry.AddEvent(ctx, "opsgenie.reading_api_key_file")
		content, err := os.ReadFile(n.conf.APIKeyFile)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "failed to read API key file")
			return nil, false, fmt.Errorf("read key_file error: %w", err)
		}
		apiKey = tmpl(string(content))
		apiKey = strings.TrimSpace(string(apiKey))
	}

	telemetry.AddEvent(ctx, "opsgenie.template_execution_completed")
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "template execution failed")
		return nil, false, fmt.Errorf("templating error: %w", err)
	}

	telemetry.AddEvent(ctx, "opsgenie.setting_headers")
	for _, req := range requests {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("GenieKey %s", apiKey))
	}

	span.SetStatus(codes.Ok, "requests created")
	return requests, true, nil
}
