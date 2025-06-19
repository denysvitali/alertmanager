// Copyright 2022 Prometheus Team
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

package webex

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

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
	// nolint:godot
	// maxMessageSize represents the maximum message length that Webex supports.
	maxMessageSize = 7439
)

type Notifier struct {
	conf    *config.WebexConfig
	tmpl    *template.Template
	logger  *slog.Logger
	client  *http.Client
	retrier *notify.Retrier
}

// New returns a new Webex notifier.
func New(c *config.WebexConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*c.HTTPConfig, "webex", httpOpts...)
	if err != nil {
		return nil, err
	}

	n := &Notifier{
		conf:    c,
		tmpl:    t,
		logger:  l,
		client:  client,
		retrier: &notify.Retrier{},
	}

	return n, nil
}

type webhook struct {
	Markdown string `json:"markdown"`
	RoomID   string `json:"roomId,omitempty"`
}

// Notify implements the Notifier interface.
func (n *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "webex.notify")
	defer span.End()

	key, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to extract group key")
		return false, err
	}

	span.SetAttributes(
		attribute.String("webex.group_key", key.String()),
		attribute.Int("webex.alerts_count", len(as)),
	)

	telemetry.AddEvent(ctx, "webex.notification_start")

	n.logger.Debug("extracted group key", "key", key)

	data := notify.GetTemplateData(ctx, n.tmpl, as, n.logger)
	tmpl := notify.TmplText(n.tmpl, data, &err)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "template execution failed")
		return false, err
	}

	message := tmpl(n.conf.Message)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "message template failed")
		return false, err
	}

	message, truncated := notify.TruncateInBytes(message, maxMessageSize)
	if truncated {
		telemetry.AddEvent(ctx, "webex.message_truncated",
			attribute.Int("max_size", maxMessageSize))
		n.logger.Debug("message truncated due to exceeding maximum allowed length by webex", "truncated_message", message)
	}

	span.SetAttributes(
		attribute.Int("webex.message_length", len(message)),
		attribute.Bool("webex.message_truncated", truncated),
		attribute.String("webex.room_id", tmpl(n.conf.RoomID)),
	)

	w := webhook{
		Markdown: message,
		RoomID:   tmpl(n.conf.RoomID),
	}

	telemetry.AddEvent(ctx, "webex.encoding_payload")
	var payload bytes.Buffer
	if err = json.NewEncoder(&payload).Encode(w); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "JSON encoding failed")
		return false, err
	}

	telemetry.AddEvent(ctx, "webex.sending_request")
	resp, err := notify.PostJSON(ctx, n.client, n.conf.APIURL.String(), &payload)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "HTTP request failed")
		return true, notify.RedactURL(err)
	}

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	telemetry.AddEvent(ctx, "webex.response_received", attribute.Int("status_code", resp.StatusCode))

	shouldRetry, err := n.retrier.Check(resp.StatusCode, resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "retry check failed")
		telemetry.AddEvent(ctx, "webex.notification_failed",
			attribute.String("error", err.Error()),
			attribute.Bool("retry", shouldRetry))
		return shouldRetry, err
	}

	span.SetStatus(codes.Ok, "notification sent")
	telemetry.AddEvent(ctx, "webex.notification_success")
	return false, nil
}
