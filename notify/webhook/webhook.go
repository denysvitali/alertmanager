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

package webhook

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

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/telemetry"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
)

// Notifier implements a Notifier for generic webhooks.
type Notifier struct {
	conf    *config.WebhookConfig
	tmpl    *template.Template
	logger  *slog.Logger
	client  *http.Client
	retrier *notify.Retrier
}

// New returns a new Webhook.
func New(conf *config.WebhookConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*conf.HTTPConfig, "webhook", httpOpts...)
	if err != nil {
		return nil, err
	}

	// Instrument the HTTP client for tracing
	instrumentedClient := notify.InstrumentedClient(client, "webhook")

	return &Notifier{
		conf:   conf,
		tmpl:   t,
		logger: l,
		client: instrumentedClient,
		// Webhooks are assumed to respond with 2xx response codes on a successful
		// request and 5xx response codes are assumed to be recoverable.
		retrier: &notify.Retrier{},
	}, nil
}

// Message defines the JSON object send to webhook endpoints.
type Message struct {
	*template.Data

	// The protocol version.
	Version         string `json:"version"`
	GroupKey        string `json:"groupKey"`
	TruncatedAlerts uint64 `json:"truncatedAlerts"`
}

func truncateAlerts(maxAlerts uint64, alerts []*types.Alert) ([]*types.Alert, uint64) {
	if maxAlerts != 0 && uint64(len(alerts)) > maxAlerts {
		return alerts[:maxAlerts], uint64(len(alerts)) - maxAlerts
	}

	return alerts, 0
}

// Notify implements the Notifier interface.
func (n *Notifier) Notify(ctx context.Context, alerts ...*types.Alert) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "notification.webhook.send",
		telemetry.WithNotificationAlertAttributes("webhook", "webhook", alerts)...)
	defer span.End()

	alerts, numTruncated := truncateAlerts(n.conf.MaxAlerts, alerts)
	telemetry.AddEvent(ctx, "notification.template.prepare")
	data := notify.GetTemplateData(ctx, n.tmpl, alerts, n.logger)

	groupKey, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		// @tjhop: should we `return false, err` here as we do in most
		// other Notify() implementations?
		telemetry.SetError(ctx, err)
		n.logger.Error("error extracting group key", "err", err)
	}

	// @tjhop: should we debug log the key here like most other Notify() implementations?

	msg := &Message{
		Version:         "4",
		Data:            data,
		GroupKey:        groupKey.String(),
		TruncatedAlerts: numTruncated,
	}

	var buf bytes.Buffer
	telemetry.AddEvent(ctx, "notification.marshal.start")
	if err := json.NewEncoder(&buf).Encode(msg); err != nil {
		telemetry.SetError(ctx, err)
		return false, err
	}

	var url string
	if n.conf.URL != nil {
		url = n.conf.URL.String()
	} else {
		content, err := os.ReadFile(n.conf.URLFile)
		if err != nil {
			telemetry.SetError(ctx, err)
			return false, fmt.Errorf("read url_file: %w", err)
		}
		url = strings.TrimSpace(string(content))
	}

	// Sanitize URL for tracing (remove sensitive info)
	telemetry.SetAttributes(ctx, telemetry.WithHTTPAttributes("POST", telemetry.SanitizeURL(url), 0)...)

	if n.conf.Timeout > 0 {
		postCtx, cancel := context.WithTimeoutCause(ctx, n.conf.Timeout, fmt.Errorf("configured webhook timeout reached (%s)", n.conf.Timeout))
		defer cancel()
		ctx = postCtx
	}

	telemetry.AddEvent(ctx, "notification.http.send")
	resp, err := notify.PostJSON(ctx, n.client, url, &buf)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", err, context.Cause(ctx))
		}
		telemetry.SetError(ctx, err)
		return true, notify.RedactURL(err)
	}
	defer notify.Drain(resp)

	telemetry.SetAttributes(ctx, telemetry.WithHTTPAttributes("POST", telemetry.SanitizeURL(url), resp.StatusCode)...)

	shouldRetry, err := n.retrier.Check(resp.StatusCode, resp.Body)
	if err != nil {
		telemetry.SetError(ctx, err)
		telemetry.AddEvent(ctx, "notification.retry_needed")
		return shouldRetry, notify.NewErrorWithReason(notify.GetFailureReasonFromStatusCode(resp.StatusCode), err)
	}

	telemetry.AddEvent(ctx, "notification.success")
	return shouldRetry, err
}
