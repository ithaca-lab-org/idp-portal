// notifier consumes order events from Pub/Sub and sends the owner an email. Sending is OFF unless
// NOTIFIER_SEND_EMAIL=true; when off it only logs what it would send and acknowledges the event.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"cloud.google.com/go/pubsub/v2"

	"github.com/ithaca-lab-org/idp-portal/notifier/internal/mail"
	"github.com/ithaca-lab-org/idp-portal/notifier/internal/notify"
)

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		slog.Error("missing required environment variable", "name", name)
		os.Exit(1)
	}
	return v
}

// sender returns the real sender only when the toggle is exactly "true". Turning it on without
// an API key or sender address is a startup error, not a silent fallback to logging.
func sender() mail.Sender {
	if os.Getenv("NOTIFIER_SEND_EMAIL") != "true" {
		slog.Info("email sending is OFF (set NOTIFIER_SEND_EMAIL=true to enable)")
		return mail.LogSender{}
	}
	key := strings.TrimSpace(mustEnv("RESEND_API_KEY"))
	slog.Info("email sending is ON")
	return mail.NewResend(key, mustEnv("EMAIL_FROM"))
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	h := &notify.Handler{Sender: sender()}
	client, err := pubsub.NewClient(ctx, mustEnv("PUBSUB_PROJECT"))
	if err != nil {
		slog.Error("pubsub client", "err", err)
		os.Exit(1)
	}
	defer func() { _ = client.Close() }()

	sub := client.Subscriber(mustEnv("PUBSUB_SUBSCRIPTION"))
	slog.Info("notifier started")
	err = sub.Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
		if err := h.Handle(ctx, m.Data); err != nil {
			slog.Warn("handling failed, will be redelivered", "err", err)
			m.Nack()
			return
		}
		m.Ack()
	})
	if err != nil {
		slog.Error("receive", "err", err)
		os.Exit(1)
	}
}
