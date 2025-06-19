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

package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	commoncfg "github.com/prometheus/common/config"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gopkg.in/telebot.v3"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/telemetry"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
)

// Telegram supports 4096 chars max - from https://limits.tginfo.me/en.
const maxMessageLenRunes = 4096

// Notifier implements a Notifier for telegram notifications.
type Notifier struct {
	conf    *config.TelegramConfig
	tmpl    *template.Template
	logger  *slog.Logger
	client  *telebot.Bot
	retrier *notify.Retrier
}

// New returns a new Telegram notification handler.
func New(conf *config.TelegramConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	httpclient, err := commoncfg.NewClientFromConfig(*conf.HTTPConfig, "telegram", httpOpts...)
	if err != nil {
		return nil, err
	}

	client, err := createTelegramClient(conf.APIUrl.String(), conf.ParseMode, httpclient)
	if err != nil {
		return nil, err
	}

	return &Notifier{
		conf:    conf,
		tmpl:    t,
		logger:  l,
		client:  client,
		retrier: &notify.Retrier{},
	}, nil
}

func (n *Notifier) Notify(ctx context.Context, alert ...*types.Alert) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "telegram.notify")
	defer span.End()

	var (
		err error
	)
	telemetry.AddEvent(ctx, "telegram.template_data_preparation")
	data := notify.GetTemplateData(ctx, n.tmpl, alert, n.logger)
	tmpl := notify.TmplText(n.tmpl, data, &err)

	if n.conf.ParseMode == "HTML" {
		telemetry.AddEvent(ctx, "telegram.html_template_processing")
		tmpl = notify.TmplHTML(n.tmpl, data, &err)
		span.SetAttributes(attribute.Bool("telegram.html_mode", true))
	} else {
		span.SetAttributes(attribute.Bool("telegram.html_mode", false))
	}

	key, ok := notify.GroupKey(ctx)
	if !ok {
		err := fmt.Errorf("group key missing")
		span.RecordError(err)
		span.SetStatus(codes.Error, "group key missing")
		return false, err
	}

	span.SetAttributes(
		attribute.String("telegram.group_key", key),
		attribute.Int("telegram.alerts_count", len(alert)),
		attribute.String("telegram.parse_mode", n.conf.ParseMode),
		attribute.Int64("telegram.chat_id", n.conf.ChatID),
		attribute.Bool("telegram.disable_notifications", n.conf.DisableNotifications),
		attribute.Int("telegram.message_thread_id", n.conf.MessageThreadID),
	)

	telemetry.AddEvent(ctx, "telegram.notification_start")

	telemetry.AddEvent(ctx, "telegram.template_execution")
	messageText, truncated := notify.TruncateInRunes(tmpl(n.conf.Message), maxMessageLenRunes)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "template execution failed")
		return false, err
	}
	if truncated {
		telemetry.AddEvent(ctx, "telegram.message_truncated",
			attribute.Int("max_runes", maxMessageLenRunes))
		n.logger.Warn("Truncated message", "alert", key, "max_runes", maxMessageLenRunes)
	}

	span.SetAttributes(
		attribute.Int("telegram.message_length", len(messageText)),
		attribute.Bool("telegram.message_truncated", truncated),
	)

	telemetry.AddEvent(ctx, "telegram.getting_bot_token")
	n.client.Token, err = n.getBotToken()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to get bot token")
		return true, err
	}

	telemetry.AddEvent(ctx, "telegram.sending_message")
	message, err := n.client.Send(telebot.ChatID(n.conf.ChatID), messageText, &telebot.SendOptions{
		DisableNotification:   n.conf.DisableNotifications,
		DisableWebPagePreview: true,
		ThreadID:              n.conf.MessageThreadID,
		ParseMode:             n.conf.ParseMode,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to send message")
		telemetry.AddEvent(ctx, "telegram.notification_failed", attribute.String("error", err.Error()))
		return true, err
	}

	span.SetAttributes(
		attribute.Int("telegram.message_id", message.ID),
		attribute.Int64("telegram.response_chat_id", message.Chat.ID),
	)
	span.SetStatus(codes.Ok, "message sent")
	telemetry.AddEvent(ctx, "telegram.notification_success",
		attribute.Int("message_id", message.ID))

	n.logger.Debug("Telegram message successfully published", "message_id", message.ID, "chat_id", message.Chat.ID)

	return false, nil
}

func createTelegramClient(apiURL, parseMode string, httpClient *http.Client) (*telebot.Bot, error) {
	bot, err := telebot.NewBot(telebot.Settings{
		URL:       apiURL,
		ParseMode: parseMode,
		Client:    httpClient,
		Offline:   true,
	})
	if err != nil {
		return nil, err
	}

	return bot, nil
}

func (n *Notifier) getBotToken() (string, error) {
	if len(n.conf.BotTokenFile) > 0 {
		content, err := os.ReadFile(n.conf.BotTokenFile)
		if err != nil {
			return "", fmt.Errorf("could not read %s: %w", n.conf.BotTokenFile, err)
		}
		return strings.TrimSpace(string(content)), nil
	}
	return string(n.conf.BotToken), nil
}
