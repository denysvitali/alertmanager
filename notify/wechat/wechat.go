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

package wechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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

// Notifier implements a Notifier for wechat notifications.
type Notifier struct {
	conf   *config.WechatConfig
	tmpl   *template.Template
	logger *slog.Logger
	client *http.Client

	accessToken   string
	accessTokenAt time.Time
}

// token is the AccessToken with corpid and corpsecret.
type token struct {
	AccessToken string `json:"access_token"`
}

type weChatMessage struct {
	Text     weChatMessageContent `yaml:"text,omitempty" json:"text,omitempty"`
	ToUser   string               `yaml:"touser,omitempty" json:"touser,omitempty"`
	ToParty  string               `yaml:"toparty,omitempty" json:"toparty,omitempty"`
	Totag    string               `yaml:"totag,omitempty" json:"totag,omitempty"`
	AgentID  string               `yaml:"agentid,omitempty" json:"agentid,omitempty"`
	Safe     string               `yaml:"safe,omitempty" json:"safe,omitempty"`
	Type     string               `yaml:"msgtype,omitempty" json:"msgtype,omitempty"`
	Markdown weChatMessageContent `yaml:"markdown,omitempty" json:"markdown,omitempty"`
}

type weChatMessageContent struct {
	Content string `json:"content"`
}

type weChatResponse struct {
	Code  int    `json:"errcode"`
	Error string `json:"errmsg"`
}

// New returns a new Wechat notifier.
func New(c *config.WechatConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*c.HTTPConfig, "wechat", httpOpts...)
	if err != nil {
		return nil, err
	}

	return &Notifier{conf: c, tmpl: t, logger: l, client: client}, nil
}

// Notify implements the Notifier interface.
func (n *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "notification.wechat.send",
		telemetry.WithNotificationAlertAttributes("wechat", "wechat", as)...)
	defer span.End()

	key, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to extract group key")
		return false, err
	}

	span.SetAttributes(
		attribute.String("wechat.group_key", key.String()),
		attribute.Int("wechat.alerts_count", len(as)),
		attribute.String("wechat.message_type", n.conf.MessageType),
	)

	telemetry.AddEvent(ctx, "wechat.notification_start")

	n.logger.Debug("extracted group key", "key", key)
	telemetry.AddEvent(ctx, "wechat.template_data_preparation")
	data := notify.GetTemplateData(ctx, n.tmpl, as, n.logger)

	telemetry.AddEvent(ctx, "wechat.template_processing")
	tmpl := notify.TmplText(n.tmpl, data, &err)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "template execution failed")
		return false, err
	}

	// Refresh AccessToken over 2 hours
	tokenExpired := n.accessToken == "" || time.Since(n.accessTokenAt) > 2*time.Hour
	if tokenExpired {
		telemetry.AddEvent(ctx, "wechat.refreshing_access_token")
		span.SetAttributes(attribute.Bool("wechat.token_refresh_needed", true))

		parameters := url.Values{}
		parameters.Add("corpsecret", tmpl(string(n.conf.APISecret)))
		parameters.Add("corpid", tmpl(string(n.conf.CorpID)))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "template execution failed")
			return false, fmt.Errorf("templating error: %w", err)
		}

		u := n.conf.APIURL.Copy()
		u.Path += "gettoken"
		u.RawQuery = parameters.Encode()

		telemetry.AddEvent(ctx, "wechat.requesting_token")
		resp, err := notify.Get(ctx, n.client, u.String())
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "token request failed")
			return true, notify.RedactURL(err)
		}
		defer notify.Drain(resp)

		var wechatToken token
		if err := json.NewDecoder(resp.Body).Decode(&wechatToken); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "token response decode failed")
			return false, err
		}

		if wechatToken.AccessToken == "" {
			err := fmt.Errorf("invalid APISecret for CorpID: %s", n.conf.CorpID)
			span.RecordError(err)
			span.SetStatus(codes.Error, "invalid API credentials")
			return false, err
		}

		// Cache accessToken
		n.accessToken = wechatToken.AccessToken
		n.accessTokenAt = time.Now()
		telemetry.AddEvent(ctx, "wechat.token_refreshed")
	} else {
		span.SetAttributes(attribute.Bool("wechat.token_refresh_needed", false))
	}

	telemetry.AddEvent(ctx, "wechat.creating_message")
	msg := &weChatMessage{
		ToUser:  tmpl(n.conf.ToUser),
		ToParty: tmpl(n.conf.ToParty),
		Totag:   tmpl(n.conf.ToTag),
		AgentID: tmpl(n.conf.AgentID),
		Type:    n.conf.MessageType,
		Safe:    "0",
	}

	if msg.Type == "markdown" {
		msg.Markdown = weChatMessageContent{
			Content: tmpl(n.conf.Message),
		}
		span.SetAttributes(attribute.Bool("wechat.is_markdown", true))
	} else {
		msg.Text = weChatMessageContent{
			Content: tmpl(n.conf.Message),
		}
		span.SetAttributes(attribute.Bool("wechat.is_markdown", false))
	}

	span.SetAttributes(
		attribute.String("wechat.to_user", msg.ToUser),
		attribute.String("wechat.to_party", msg.ToParty),
		attribute.String("wechat.agent_id", msg.AgentID),
	)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "message template execution failed")
		return false, fmt.Errorf("templating error: %w", err)
	}

	telemetry.AddEvent(ctx, "wechat.encoding_message")
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "JSON encoding failed")
		return false, err
	}

	postMessageURL := n.conf.APIURL.Copy()
	postMessageURL.Path += "message/send"
	q := postMessageURL.Query()
	q.Set("access_token", n.accessToken)
	postMessageURL.RawQuery = q.Encode()

	telemetry.AddEvent(ctx, "wechat.sending_message")
	resp, err := notify.PostJSON(ctx, n.client, postMessageURL.String(), &buf)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "HTTP request failed")
		return true, notify.RedactURL(err)
	}
	defer notify.Drain(resp)

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	telemetry.AddEvent(ctx, "wechat.response_received", attribute.Int("status_code", resp.StatusCode))

	if resp.StatusCode != 200 {
		err := fmt.Errorf("unexpected status code %v", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(codes.Error, "unexpected status code")
		return true, notify.NewErrorWithReason(notify.GetFailureReasonFromStatusCode(resp.StatusCode), err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "response body read failed")
		return true, err
	}
	n.logger.Debug(string(body), "incident", key)

	var weResp weChatResponse
	if err := json.Unmarshal(body, &weResp); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "response decode failed")
		return true, err
	}

	span.SetAttributes(
		attribute.Int("wechat.response_code", weResp.Code),
		attribute.String("wechat.response_error", weResp.Error),
	)

	// https://work.weixin.qq.com/api/doc#10649
	if weResp.Code == 0 {
		span.SetStatus(codes.Ok, "notification sent")
		telemetry.AddEvent(ctx, "wechat.notification_success")
		return false, nil
	}

	// AccessToken is expired
	if weResp.Code == 42001 {
		n.accessToken = ""
		err := errors.New(weResp.Error)
		span.RecordError(err)
		span.SetStatus(codes.Error, "access token expired")
		telemetry.AddEvent(ctx, "wechat.token_expired", attribute.String("error", weResp.Error))
		return true, err
	}

	err = errors.New(weResp.Error)
	span.RecordError(err)
	span.SetStatus(codes.Error, "WeChat API error")
	telemetry.AddEvent(ctx, "wechat.notification_failed",
		attribute.String("error", weResp.Error),
		attribute.Int("error_code", weResp.Code))
	return false, err
}
