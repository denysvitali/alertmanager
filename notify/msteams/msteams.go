// Copyright 2023 Prometheus Team
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

package msteams

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

const (
	colorRed   = "8C1A1A"
	colorGreen = "2DC72D"
	colorGrey  = "808080"
)

type Notifier struct {
	conf         *config.MSTeamsConfig
	tmpl         *template.Template
	logger       *slog.Logger
	client       *http.Client
	retrier      *notify.Retrier
	webhookURL   *config.SecretURL
	postJSONFunc func(ctx context.Context, client *http.Client, url string, body io.Reader) (*http.Response, error)
}

// Message card reference can be found at https://learn.microsoft.com/en-us/outlook/actionable-messages/message-card-reference.
type teamsMessage struct {
	Context    string `json:"@context"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	Text       string `json:"text"`
	ThemeColor string `json:"themeColor"`
}

// New returns a new notifier that uses the Microsoft Teams Webhook API.
func New(c *config.MSTeamsConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*c.HTTPConfig, "msteams", httpOpts...)
	if err != nil {
		return nil, err
	}

	n := &Notifier{
		conf:         c,
		tmpl:         t,
		logger:       l,
		client:       client,
		retrier:      &notify.Retrier{},
		webhookURL:   c.WebhookURL,
		postJSONFunc: notify.PostJSON,
	}

	return n, nil
}

func (n *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "msteams.notify")
	defer span.End()

	key, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to extract group key")
		return false, err
	}

	span.SetAttributes(
		attribute.String("msteams.group_key", key.String()),
		attribute.Int("msteams.alerts_count", len(as)),
	)

	telemetry.AddEvent(ctx, "msteams.notification_start")

	n.logger.Debug("extracted group key", "key", key)

	data := notify.GetTemplateData(ctx, n.tmpl, as, n.logger)
	tmpl := notify.TmplText(n.tmpl, data, &err)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "template execution failed")
		return false, err
	}

	telemetry.AddEvent(ctx, "msteams.processing_content")

	title := tmpl(n.conf.Title)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "title template failed")
		return false, err
	}
	text := tmpl(n.conf.Text)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "text template failed")
		return false, err
	}
	summary := tmpl(n.conf.Summary)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "summary template failed")
		return false, err
	}

	alerts := types.Alerts(as...)
	color := colorGrey
	switch alerts.Status() {
	case model.AlertFiring:
		color = colorRed
	case model.AlertResolved:
		color = colorGreen
	}

	span.SetAttributes(
		attribute.String("msteams.alert_status", string(alerts.Status())),
		attribute.String("msteams.color", color),
		attribute.Int("msteams.title_length", len(title)),
		attribute.Int("msteams.text_length", len(text)),
		attribute.Int("msteams.summary_length", len(summary)),
	)

	var url string
	if n.conf.WebhookURL != nil {
		url = n.conf.WebhookURL.String()
	} else {
		telemetry.AddEvent(ctx, "msteams.reading_webhook_url_file")
		content, err := os.ReadFile(n.conf.WebhookURLFile)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "failed to read webhook URL file")
			return false, fmt.Errorf("read webhook_url_file: %w", err)
		}
		url = strings.TrimSpace(string(content))
	}

	t := teamsMessage{
		Context:    "http://schema.org/extensions",
		Type:       "MessageCard",
		Title:      title,
		Summary:    summary,
		Text:       text,
		ThemeColor: color,
	}

	telemetry.AddEvent(ctx, "msteams.encoding_payload")
	var payload bytes.Buffer
	if err = json.NewEncoder(&payload).Encode(t); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "JSON encoding failed")
		return false, err
	}

	telemetry.AddEvent(ctx, "msteams.sending_request")
	resp, err := n.postJSONFunc(ctx, n.client, url, &payload)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "HTTP request failed")
		return true, notify.RedactURL(err)
	}
	defer notify.Drain(resp)

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	telemetry.AddEvent(ctx, "msteams.response_received", attribute.Int("status_code", resp.StatusCode))

	// https://learn.microsoft.com/en-us/microsoftteams/platform/webhooks-and-connectors/how-to/connectors-using?tabs=cURL#rate-limiting-for-connectors
	shouldRetry, err := n.retrier.Check(resp.StatusCode, resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "retry check failed")
		telemetry.AddEvent(ctx, "msteams.notification_failed",
			attribute.String("error", err.Error()),
			attribute.Bool("retry", shouldRetry))
		return shouldRetry, notify.NewErrorWithReason(notify.GetFailureReasonFromStatusCode(resp.StatusCode), err)
	}

	span.SetStatus(codes.Ok, "notification sent")
	telemetry.AddEvent(ctx, "msteams.notification_success")
	return shouldRetry, err
}
