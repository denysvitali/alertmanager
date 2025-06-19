// Copyright 2021 Prometheus Team
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

package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	netUrl "net/url"
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
	// https://discord.com/developers/docs/resources/channel#embed-object-embed-limits - 256 characters or runes.
	maxTitleLenRunes = 256
	// https://discord.com/developers/docs/resources/channel#embed-object-embed-limits - 4096 characters or runes.
	maxDescriptionLenRunes = 4096

	maxContentLenRunes = 2000
)

const (
	colorRed   = 0x992D22
	colorGreen = 0x2ECC71
	colorGrey  = 0x95A5A6
)

// Notifier implements a Notifier for Discord notifications.
type Notifier struct {
	conf       *config.DiscordConfig
	tmpl       *template.Template
	logger     *slog.Logger
	client     *http.Client
	retrier    *notify.Retrier
	webhookURL *config.SecretURL
}

// New returns a new Discord notifier.
func New(c *config.DiscordConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*c.HTTPConfig, "discord", httpOpts...)
	if err != nil {
		return nil, err
	}
	n := &Notifier{
		conf:       c,
		tmpl:       t,
		logger:     l,
		client:     client,
		retrier:    &notify.Retrier{},
		webhookURL: c.WebhookURL,
	}
	return n, nil
}

type webhook struct {
	Content   string         `json:"content"`
	Embeds    []webhookEmbed `json:"embeds"`
	Username  string         `json:"username,omitempty"`
	AvatarURL string         `json:"avatar_url,omitempty"`
}

type webhookEmbed struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Color       int    `json:"color"`
}

// Notify implements the Notifier interface.
func (n *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "discord.notify")
	defer span.End()

	key, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to extract group key")
		return false, err
	}

	span.SetAttributes(
		attribute.String("discord.group_key", key.String()),
		attribute.Int("discord.alerts_count", len(as)),
	)

	telemetry.AddEvent(ctx, "discord.notification_start")

	n.logger.Debug("extracted group key", "key", key)

	alerts := types.Alerts(as...)
	telemetry.AddEvent(ctx, "discord.template_data_preparation")
	data := notify.GetTemplateData(ctx, n.tmpl, as, n.logger)
	tmpl := notify.TmplText(n.tmpl, data, &err)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "template execution failed")
		return false, err
	}

	telemetry.AddEvent(ctx, "discord.processing_content")

	telemetry.AddEvent(ctx, "discord.template_execution")
	title, truncated := notify.TruncateInRunes(tmpl(n.conf.Title), maxTitleLenRunes)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "title template failed")
		return false, err
	}
	if truncated {
		telemetry.AddEvent(ctx, "discord.title_truncated",
			attribute.Int("max_runes", maxTitleLenRunes))
		n.logger.Warn("Truncated title", "key", key, "max_runes", maxTitleLenRunes)
	}

	description, truncated := notify.TruncateInRunes(tmpl(n.conf.Message), maxDescriptionLenRunes)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "description template failed")
		return false, err
	}
	if truncated {
		telemetry.AddEvent(ctx, "discord.description_truncated",
			attribute.Int("max_runes", maxDescriptionLenRunes))
		n.logger.Warn("Truncated message", "key", key, "max_runes", maxDescriptionLenRunes)
	}

	content, truncated := notify.TruncateInRunes(tmpl(n.conf.Content), maxContentLenRunes)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "content template failed")
		return false, err
	}
	if truncated {
		telemetry.AddEvent(ctx, "discord.content_truncated",
			attribute.Int("max_runes", maxContentLenRunes))
		n.logger.Warn("Truncated message", "key", key, "max_runes", maxContentLenRunes)
	}

	color := colorGrey
	if alerts.Status() == model.AlertFiring {
		color = colorRed
	}
	if alerts.Status() == model.AlertResolved {
		color = colorGreen
	}

	span.SetAttributes(
		attribute.String("discord.alert_status", string(alerts.Status())),
		attribute.Int("discord.color", color),
		attribute.Int("discord.title_length", len(title)),
		attribute.Int("discord.description_length", len(description)),
		attribute.Int("discord.content_length", len(content)),
		attribute.String("discord.username", n.conf.Username),
	)

	var url string
	if n.conf.WebhookURL != nil {
		url = n.conf.WebhookURL.String()
	} else {
		telemetry.AddEvent(ctx, "discord.reading_webhook_url_file")
		b, err := os.ReadFile(n.conf.WebhookURLFile)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "failed to read webhook URL file")
			return false, fmt.Errorf("read webhook_url_file: %w", err)
		}
		url = strings.TrimSpace(string(b))
	}

	w := webhook{
		Content:  content,
		Username: n.conf.Username,
		Embeds: []webhookEmbed{{
			Title:       title,
			Description: description,
			Color:       color,
		}},
	}

	if len(n.conf.AvatarURL) != 0 {
		if _, err := netUrl.Parse(n.conf.AvatarURL); err == nil {
			w.AvatarURL = n.conf.AvatarURL
			span.SetAttributes(attribute.String("discord.avatar_url", n.conf.AvatarURL))
		} else {
			telemetry.AddEvent(ctx, "discord.invalid_avatar_url",
				attribute.String("url", n.conf.AvatarURL))
			n.logger.Warn("Bad avatar url", "key", key)
		}
	}

	telemetry.AddEvent(ctx, "discord.encoding_payload")
	var payload bytes.Buffer
	if err = json.NewEncoder(&payload).Encode(w); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "JSON encoding failed")
		return false, err
	}

	telemetry.AddEvent(ctx, "discord.sending_request")
	resp, err := notify.PostJSON(ctx, n.client, url, &payload)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "HTTP request failed")
		return true, notify.RedactURL(err)
	}

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	telemetry.AddEvent(ctx, "discord.response_received", attribute.Int("status_code", resp.StatusCode))

	shouldRetry, err := n.retrier.Check(resp.StatusCode, resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "retry check failed")
		telemetry.AddEvent(ctx, "discord.notification_failed",
			attribute.String("error", err.Error()),
			attribute.Bool("retry", shouldRetry))
		return shouldRetry, err
	}

	span.SetStatus(codes.Ok, "notification sent")
	telemetry.AddEvent(ctx, "discord.notification_success")
	return false, nil
}
