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

package pushover

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	commoncfg "github.com/prometheus/common/config"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/telemetry"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
)

const (
	// https://pushover.net/api#limits - 250 characters or runes.
	maxTitleLenRunes = 250
	// https://pushover.net/api#limits - 1024 characters or runes.
	maxMessageLenRunes = 1024
	// https://pushover.net/api#limits - 512 characters or runes.
	maxURLLenRunes = 512
)

// Notifier implements a Notifier for Pushover notifications.
type Notifier struct {
	conf    *config.PushoverConfig
	tmpl    *template.Template
	logger  *slog.Logger
	client  *http.Client
	retrier *notify.Retrier
	apiURL  string // for tests.
}

// New returns a new Pushover notifier.
func New(c *config.PushoverConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*c.HTTPConfig, "pushover", httpOpts...)
	if err != nil {
		return nil, err
	}
	return &Notifier{
		conf:    c,
		tmpl:    t,
		logger:  l,
		client:  client,
		retrier: &notify.Retrier{},
		apiURL:  "https://api.pushover.net/1/messages.json",
	}, nil
}

// Notify implements the Notifier interface.
func (n *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "notification.pushover.send",
		telemetry.WithNotificationAlertAttributes("pushover", "pushover", as)...)
	defer span.End()

	key, ok := notify.GroupKey(ctx)
	if !ok {
		err := fmt.Errorf("group key missing")
		span.RecordError(err)
		span.SetStatus(codes.Error, "group key missing")
		return false, err
	}
	telemetry.AddEvent(ctx, "pushover.template_data_preparation")
	data := notify.GetTemplateData(ctx, n.tmpl, as, n.logger)

	span.SetAttributes(
		attribute.String("pushover.group_key", key),
		attribute.Int("pushover.alerts_count", len(as)),
	)

	telemetry.AddEvent(ctx, "pushover.notification_start")

	// @tjhop: should this use `group` for the keyval like most other notify implementations?
	n.logger.Debug("extracted group key", "incident", key)

	var (
		err     error
		message string
	)
	telemetry.AddEvent(ctx, "pushover.template_processing")
	tmpl := notify.TmplText(n.tmpl, data, &err)
	tmplHTML := notify.TmplHTML(n.tmpl, data, &err)

	var (
		token   string
		userKey string
	)
	if n.conf.Token != "" {
		token = string(n.conf.Token)
	} else {
		telemetry.AddEvent(ctx, "pushover.reading_token_file")
		content, err := os.ReadFile(n.conf.TokenFile)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "failed to read token file")
			return false, fmt.Errorf("read token_file: %w", err)
		}
		token = string(content)
	}
	if n.conf.UserKey != "" {
		userKey = string(n.conf.UserKey)
	} else {
		telemetry.AddEvent(ctx, "pushover.reading_user_key_file")
		content, err := os.ReadFile(n.conf.UserKeyFile)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "failed to read user key file")
			return false, fmt.Errorf("read user_key_file: %w", err)
		}
		userKey = string(content)
	}

	telemetry.AddEvent(ctx, "pushover.preparing_parameters")
	parameters := url.Values{}
	parameters.Add("token", tmpl(token))
	parameters.Add("user", tmpl(userKey))

	title, truncated := notify.TruncateInRunes(tmpl(n.conf.Title), maxTitleLenRunes)
	if truncated {
		telemetry.AddEvent(ctx, "pushover.title_truncated",
			attribute.Int("max_runes", maxTitleLenRunes))
		n.logger.Warn("Truncated title", "incident", key, "max_runes", maxTitleLenRunes)
	}
	parameters.Add("title", title)

	if n.conf.HTML {
		parameters.Add("html", "1")
		message = tmplHTML(n.conf.Message)
		span.SetAttributes(attribute.Bool("pushover.html_enabled", true))
	} else {
		message = tmpl(n.conf.Message)
		span.SetAttributes(attribute.Bool("pushover.html_enabled", false))
	}

	if n.conf.Monospace {
		parameters.Add("monospace", "1")
		span.SetAttributes(attribute.Bool("pushover.monospace", true))
	}

	message, truncated = notify.TruncateInRunes(message, maxMessageLenRunes)
	if truncated {
		telemetry.AddEvent(ctx, "pushover.message_truncated",
			attribute.Int("max_runes", maxMessageLenRunes))
		n.logger.Warn("Truncated message", "incident", key, "max_runes", maxMessageLenRunes)
	}
	message = strings.TrimSpace(message)
	if message == "" {
		// Pushover rejects empty messages.
		message = "(no details)"
		telemetry.AddEvent(ctx, "pushover.empty_message_replaced")
	}
	parameters.Add("message", message)

	supplementaryURL, truncated := notify.TruncateInRunes(tmpl(n.conf.URL), maxURLLenRunes)
	if truncated {
		telemetry.AddEvent(ctx, "pushover.url_truncated",
			attribute.Int("max_runes", maxURLLenRunes))
		n.logger.Warn("Truncated URL", "incident", key, "max_runes", maxURLLenRunes)
	}
	parameters.Add("url", supplementaryURL)
	parameters.Add("url_title", tmpl(n.conf.URLTitle))

	parameters.Add("priority", tmpl(n.conf.Priority))
	parameters.Add("retry", fmt.Sprintf("%d", int64(time.Duration(n.conf.Retry).Seconds())))
	parameters.Add("expire", fmt.Sprintf("%d", int64(time.Duration(n.conf.Expire).Seconds())))
	parameters.Add("device", tmpl(n.conf.Device))
	parameters.Add("sound", tmpl(n.conf.Sound))

	telemetry.AddEvent(ctx, "pushover.template_execution_completed")
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "template execution failed")
		return false, err
	}

	newttl := int64(time.Duration(n.conf.TTL).Seconds())
	if newttl > 0 {
		parameters.Add("ttl", fmt.Sprintf("%d", newttl))
	}

	span.SetAttributes(
		attribute.Int("pushover.title_length", len(title)),
		attribute.Int("pushover.message_length", len(message)),
		attribute.String("pushover.priority", tmpl(n.conf.Priority)),
		attribute.Int64("pushover.ttl_seconds", newttl),
	)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "template execution failed")
		return false, err
	}

	u, err := url.Parse(n.apiURL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "URL parsing failed")
		return false, err
	}
	u.RawQuery = parameters.Encode()

	telemetry.AddEvent(ctx, "pushover.sending_request")
	// Don't log the URL as it contains secret data (see #1825).
	n.logger.Debug("Sending message", "incident", key)
	resp, err := notify.PostText(ctx, n.client, u.String(), nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "HTTP request failed")
		return true, notify.RedactURL(err)
	}
	defer notify.Drain(resp)

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	telemetry.AddEvent(ctx, "pushover.response_received", attribute.Int("status_code", resp.StatusCode))

	shouldRetry, err := n.retrier.Check(resp.StatusCode, resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "retry check failed")
		telemetry.AddEvent(ctx, "pushover.notification_failed",
			attribute.String("error", err.Error()),
			attribute.Bool("retry", shouldRetry))
		return shouldRetry, notify.NewErrorWithReason(notify.GetFailureReasonFromStatusCode(resp.StatusCode), err)
	}

	span.SetStatus(codes.Ok, "notification sent")
	telemetry.AddEvent(ctx, "pushover.notification_success")
	return shouldRetry, err
}
