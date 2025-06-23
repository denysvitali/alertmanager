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

package pagerduty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/alecthomas/units"
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

const (
	maxEventSize int = 512000
	// https://developer.pagerduty.com/docs/ZG9jOjExMDI5NTc4-send-a-v1-event - 1024 characters or runes.
	maxV1DescriptionLenRunes = 1024
	// https://developer.pagerduty.com/docs/ZG9jOjExMDI5NTgx-send-an-alert-event - 1024 characters or runes.
	maxV2SummaryLenRunes = 1024
)

// Notifier implements a Notifier for PagerDuty notifications.
type Notifier struct {
	conf    *config.PagerdutyConfig
	tmpl    *template.Template
	logger  *slog.Logger
	apiV1   string // for tests.
	client  *http.Client
	retrier *notify.Retrier
}

// New returns a new PagerDuty notifier.
func New(c *config.PagerdutyConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*c.HTTPConfig, "pagerduty", httpOpts...)
	if err != nil {
		return nil, err
	}

	n := &Notifier{conf: c, tmpl: t, logger: l, client: notify.InstrumentedClient(client, "pagerduty")}
	if c.ServiceKey != "" || c.ServiceKeyFile != "" {
		n.apiV1 = "https://events.pagerduty.com/generic/2010-04-15/create_event.json"
		// Retrying can solve the issue on 403 (rate limiting) and 5xx response codes.
		// https://v2.developer.pagerduty.com/docs/trigger-events
		n.retrier = &notify.Retrier{RetryCodes: []int{http.StatusForbidden}, CustomDetailsFunc: errDetails}
	} else {
		// Retrying can solve the issue on 429 (rate limiting) and 5xx response codes.
		// https://v2.developer.pagerduty.com/docs/events-api-v2#api-response-codes--retry-logic
		n.retrier = &notify.Retrier{RetryCodes: []int{http.StatusTooManyRequests}, CustomDetailsFunc: errDetails}
	}
	return n, nil
}

const (
	pagerDutyEventTrigger = "trigger"
	pagerDutyEventResolve = "resolve"
)

type pagerDutyMessage struct {
	RoutingKey  string            `json:"routing_key,omitempty"`
	ServiceKey  string            `json:"service_key,omitempty"`
	DedupKey    string            `json:"dedup_key,omitempty"`
	IncidentKey string            `json:"incident_key,omitempty"`
	EventType   string            `json:"event_type,omitempty"`
	Description string            `json:"description,omitempty"`
	EventAction string            `json:"event_action"`
	Payload     *pagerDutyPayload `json:"payload"`
	Client      string            `json:"client,omitempty"`
	ClientURL   string            `json:"client_url,omitempty"`
	Details     map[string]string `json:"details,omitempty"`
	Images      []pagerDutyImage  `json:"images,omitempty"`
	Links       []pagerDutyLink   `json:"links,omitempty"`
}

type pagerDutyLink struct {
	HRef string `json:"href"`
	Text string `json:"text"`
}

type pagerDutyImage struct {
	Src  string `json:"src"`
	Alt  string `json:"alt"`
	Href string `json:"href"`
}

type pagerDutyPayload struct {
	Summary       string            `json:"summary"`
	Source        string            `json:"source"`
	Severity      string            `json:"severity"`
	Timestamp     string            `json:"timestamp,omitempty"`
	Class         string            `json:"class,omitempty"`
	Component     string            `json:"component,omitempty"`
	Group         string            `json:"group,omitempty"`
	CustomDetails map[string]string `json:"custom_details,omitempty"`
}

func (n *Notifier) encodeMessage(msg *pagerDutyMessage) (bytes.Buffer, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(msg); err != nil {
		return buf, fmt.Errorf("failed to encode PagerDuty message: %w", err)
	}

	if buf.Len() > maxEventSize {
		truncatedMsg := fmt.Sprintf("Custom details have been removed because the original event exceeds the maximum size of %s", units.MetricBytes(maxEventSize).String())

		if n.apiV1 != "" {
			msg.Details = map[string]string{"error": truncatedMsg}
		} else {
			msg.Payload.CustomDetails = map[string]string{"error": truncatedMsg}
		}

		warningMsg := fmt.Sprintf("Truncated Details because message of size %s exceeds limit %s", units.MetricBytes(buf.Len()).String(), units.MetricBytes(maxEventSize).String())
		n.logger.Warn(warningMsg)

		buf.Reset()
		if err := json.NewEncoder(&buf).Encode(msg); err != nil {
			return buf, fmt.Errorf("failed to encode PagerDuty message: %w", err)
		}
	}

	return buf, nil
}

func (n *Notifier) notifyV1(
	ctx context.Context,
	eventType string,
	key notify.Key,
	data *template.Data,
	details map[string]string,
	as ...*types.Alert,
) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "pagerduty.notify_v1")
	defer span.End()

	var tmplErr error
	tmpl := notify.TmplText(n.tmpl, data, &tmplErr)

	description, truncated := notify.TruncateInRunes(tmpl(n.conf.Description), maxV1DescriptionLenRunes)
	if truncated {
		telemetry.AddEvent(ctx, "pagerduty.description_truncated",
			attribute.Int("max_runes", maxV1DescriptionLenRunes))
		n.logger.Warn("Truncated description", "key", key, "max_runes", maxV1DescriptionLenRunes)
	}

	serviceKey := string(n.conf.ServiceKey)
	if serviceKey == "" {
		telemetry.AddEvent(ctx, "pagerduty.reading_service_key_file")
		content, fileErr := os.ReadFile(n.conf.ServiceKeyFile)
		if fileErr != nil {
			span.RecordError(fileErr)
			span.SetStatus(codes.Error, "failed to read service key file")
			return false, fmt.Errorf("failed to read service key from file: %w", fileErr)
		}
		serviceKey = strings.TrimSpace(string(content))
	}

	msg := &pagerDutyMessage{
		ServiceKey:  tmpl(serviceKey),
		EventType:   eventType,
		IncidentKey: key.Hash(),
		Description: description,
		Details:     details,
	}

	if eventType == pagerDutyEventTrigger {
		msg.Client = tmpl(n.conf.Client)
		msg.ClientURL = tmpl(n.conf.ClientURL)
	}

	if tmplErr != nil {
		span.RecordError(tmplErr)
		span.SetStatus(codes.Error, "template execution failed")
		return false, fmt.Errorf("failed to template PagerDuty v1 message: %w", tmplErr)
	}

	// Ensure that the service key isn't empty after templating.
	if msg.ServiceKey == "" {
		err := errors.New("service key cannot be empty")
		span.RecordError(err)
		span.SetStatus(codes.Error, "empty service key")
		return false, err
	}

	span.SetAttributes(
		attribute.String("pagerduty.incident_key", msg.IncidentKey),
		attribute.Int("pagerduty.description_length", len(description)),
		attribute.Bool("pagerduty.description_truncated", truncated),
	)

	telemetry.AddEvent(ctx, "pagerduty.encoding_message")
	encodedMsg, err := n.encodeMessage(msg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "message encoding failed")
		return false, err
	}

	telemetry.AddEvent(ctx, "pagerduty.sending_request", attribute.String("url", n.apiV1))
	resp, err := notify.PostJSON(ctx, n.client, n.apiV1, &encodedMsg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "HTTP request failed")
		return true, fmt.Errorf("failed to post message to PagerDuty v1: %w", err)
	}
	defer notify.Drain(resp)

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	telemetry.AddEvent(ctx, "pagerduty.response_received", attribute.Int("status_code", resp.StatusCode))

	return n.retrier.Check(resp.StatusCode, resp.Body)
}

func (n *Notifier) notifyV2(
	ctx context.Context,
	eventType string,
	key notify.Key,
	data *template.Data,
	details map[string]string,
	as ...*types.Alert,
) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "pagerduty.notify_v2")
	defer span.End()

	var tmplErr error
	tmpl := notify.TmplText(n.tmpl, data, &tmplErr)

	if n.conf.Severity == "" {
		n.conf.Severity = "error"
	}

	summary, truncated := notify.TruncateInRunes(tmpl(n.conf.Description), maxV2SummaryLenRunes)
	if truncated {
		telemetry.AddEvent(ctx, "pagerduty.summary_truncated",
			attribute.Int("max_runes", maxV2SummaryLenRunes))
		n.logger.Warn("Truncated summary", "key", key, "max_runes", maxV2SummaryLenRunes)
	}

	routingKey := string(n.conf.RoutingKey)
	if routingKey == "" {
		telemetry.AddEvent(ctx, "pagerduty.reading_routing_key_file")
		content, fileErr := os.ReadFile(n.conf.RoutingKeyFile)
		if fileErr != nil {
			span.RecordError(fileErr)
			span.SetStatus(codes.Error, "failed to read routing key file")
			return false, fmt.Errorf("failed to read routing key from file: %w", fileErr)
		}
		routingKey = strings.TrimSpace(string(content))
	}

	msg := &pagerDutyMessage{
		Client:      tmpl(n.conf.Client),
		ClientURL:   tmpl(n.conf.ClientURL),
		RoutingKey:  tmpl(routingKey),
		EventAction: eventType,
		DedupKey:    key.Hash(),
		Images:      make([]pagerDutyImage, 0, len(n.conf.Images)),
		Links:       make([]pagerDutyLink, 0, len(n.conf.Links)),
		Payload: &pagerDutyPayload{
			Summary:       summary,
			Source:        tmpl(n.conf.Source),
			Severity:      tmpl(n.conf.Severity),
			CustomDetails: details,
			Class:         tmpl(n.conf.Class),
			Component:     tmpl(n.conf.Component),
			Group:         tmpl(n.conf.Group),
		},
	}

	for _, item := range n.conf.Images {
		image := pagerDutyImage{
			Src:  tmpl(item.Src),
			Alt:  tmpl(item.Alt),
			Href: tmpl(item.Href),
		}

		if image.Src != "" {
			msg.Images = append(msg.Images, image)
		}
	}

	for _, item := range n.conf.Links {
		link := pagerDutyLink{
			HRef: tmpl(item.Href),
			Text: tmpl(item.Text),
		}

		if link.HRef != "" {
			msg.Links = append(msg.Links, link)
		}
	}

	span.SetAttributes(
		attribute.String("pagerduty.dedup_key", msg.DedupKey),
		attribute.String("pagerduty.severity", msg.Payload.Severity),
		attribute.Int("pagerduty.summary_length", len(summary)),
		attribute.Bool("pagerduty.summary_truncated", truncated),
		attribute.Int("pagerduty.images_count", len(msg.Images)),
		attribute.Int("pagerduty.links_count", len(msg.Links)),
	)

	if tmplErr != nil {
		span.RecordError(tmplErr)
		span.SetStatus(codes.Error, "template execution failed")
		return false, fmt.Errorf("failed to template PagerDuty v2 message: %w", tmplErr)
	}

	// Ensure that the routing key isn't empty after templating.
	if msg.RoutingKey == "" {
		err := errors.New("routing key cannot be empty")
		span.RecordError(err)
		span.SetStatus(codes.Error, "empty routing key")
		return false, err
	}

	telemetry.AddEvent(ctx, "pagerduty.encoding_message")
	encodedMsg, err := n.encodeMessage(msg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "message encoding failed")
		return false, err
	}

	telemetry.AddEvent(ctx, "pagerduty.sending_request", attribute.String("url", n.conf.URL.String()))
	resp, err := notify.PostJSON(ctx, n.client, n.conf.URL.String(), &encodedMsg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "HTTP request failed")
		return true, fmt.Errorf("failed to post message to PagerDuty: %w", err)
	}
	defer notify.Drain(resp)

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	telemetry.AddEvent(ctx, "pagerduty.response_received", attribute.Int("status_code", resp.StatusCode))

	retry, err := n.retrier.Check(resp.StatusCode, resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "retry check failed")
		return retry, notify.NewErrorWithReason(notify.GetFailureReasonFromStatusCode(resp.StatusCode), err)
	}
	return retry, err
}

// Notify implements the Notifier interface.
func (n *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "pagerduty.notify",
		telemetry.WithNotificationAlertAttributes("pagerduty", "pagerduty", as)...)
	defer span.End()

	key, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to extract group key")
		return false, err
	}

	var (
		alerts    = types.Alerts(as...)
		data      = notify.GetTemplateData(ctx, n.tmpl, as, n.logger)
		eventType = pagerDutyEventTrigger
	)
	if alerts.Status() == model.AlertResolved {
		eventType = pagerDutyEventResolve
	}

	span.SetAttributes(
		attribute.String("pagerduty.event_type", eventType),
		attribute.String("pagerduty.group_key", key.String()),
		attribute.Int("pagerduty.alerts_count", len(as)),
		attribute.String("pagerduty.alert_status", string(alerts.Status())),
	)

	telemetry.AddEvent(ctx, "pagerduty.notification_start", attribute.String("event_type", eventType))

	n.logger.Debug("extracted group key", "key", key, "eventType", eventType)

	details := make(map[string]string, len(n.conf.Details))
	for k, v := range n.conf.Details {
		detail, err := n.tmpl.ExecuteTextString(v, data)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "template execution failed")
			return false, fmt.Errorf("%q: failed to template %q: %w", k, v, err)
		}
		details[k] = detail
	}

	telemetry.AddEvent(ctx, "pagerduty.template_processed", attribute.Int("details_count", len(details)))

	var retry bool
	if n.apiV1 != "" {
		span.SetAttributes(attribute.String("pagerduty.api_version", "v1"))
		retry, err = n.notifyV1(ctx, eventType, key, data, details, as...)
	} else {
		span.SetAttributes(attribute.String("pagerduty.api_version", "v2"))
		retry, err = n.notifyV2(ctx, eventType, key, data, details, as...)
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "notification failed")
		telemetry.AddEvent(ctx, "pagerduty.notification_failed",
			attribute.String("error", err.Error()),
			attribute.Bool("retry", retry))
	} else {
		span.SetStatus(codes.Ok, "notification sent")
		telemetry.AddEvent(ctx, "pagerduty.notification_success")
	}

	return retry, err
}

func errDetails(status int, body io.Reader) string {
	// See https://v2.developer.pagerduty.com/docs/trigger-events for the v1 events API.
	// See https://v2.developer.pagerduty.com/docs/send-an-event-events-api-v2 for the v2 events API.
	if status != http.StatusBadRequest || body == nil {
		return ""
	}
	var pgr struct {
		Status  string   `json:"status"`
		Message string   `json:"message"`
		Errors  []string `json:"errors"`
	}
	if err := json.NewDecoder(body).Decode(&pgr); err != nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", pgr.Message, strings.Join(pgr.Errors, ","))
}
