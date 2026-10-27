// Package notify turns order events into emails.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/ithaca-lab-org/idp-portal/notifier/internal/mail"
)

// Event mirrors api/internal/orders.Event (version 1). Unknown fields are ignored.
type Event struct {
	Version      int    `json:"version"`
	Type         string `json:"type"`
	OrderID      string `json:"orderId"`
	Name         string `json:"name"`
	BusinessUnit string `json:"businessUnit"`
	Owner        string `json:"owner"`
	Size         string `json:"size"`
	Message      string `json:"message"`
}

var emailRe = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9-]+([.][a-zA-Z0-9-]+)+$`)

// Handler sends one email per event.
type Handler struct {
	Sender mail.Sender
}

// Handle returns nil when the message is done (sent, skipped or unusable) and an error only when
// a retry may help. Returning nil for bad input is deliberate: redelivering it cannot succeed.
func (h *Handler) Handle(ctx context.Context, data []byte) error {
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		slog.Warn("dropping unreadable event", "err", err)
		return nil
	}
	if e.Version != 1 {
		slog.Warn("dropping event with an unknown version", "version", e.Version, "order", e.OrderID)
		return nil
	}
	subject, text, ok := render(e)
	if !ok {
		slog.Info("ignoring event type", "type", e.Type)
		return nil
	}
	to := strings.TrimSpace(e.Owner)
	if len(to) > 254 || !emailRe.MatchString(to) {
		slog.Warn("skipping event without a valid recipient", "order", e.OrderID)
		return nil
	}
	err := h.Sender.Send(ctx, mail.Message{To: to, Subject: subject, Text: text})
	if errors.Is(err, mail.ErrPermanent) {
		slog.Error("email rejected, not retrying", "order", e.OrderID, "err", err)
		return nil
	}
	return err
}

// clean keeps user-controlled text on one line so it cannot add headers or fake paragraphs.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

func render(e Event) (subject, text string, ok bool) {
	name, id := clean(e.Name), clean(e.OrderID)
	switch e.Type {
	case "order.complete":
		return fmt.Sprintf("Namespace %s is ready (%s)", name, id),
			fmt.Sprintf("Your namespace order %s is complete.\n\nNamespace: %s\nBusiness unit: %s\nSize: %s\n",
				id, name, clean(e.BusinessUnit), clean(e.Size)), true
	case "order.failed":
		reason := clean(e.Message)
		if reason == "" {
			reason = "no reason given"
		}
		return fmt.Sprintf("Namespace %s could not be created (%s)", name, id),
			fmt.Sprintf("Your namespace order %s failed.\n\nNamespace: %s\nBusiness unit: %s\nReason: %s\n",
				id, name, clean(e.BusinessUnit), reason), true
	}
	return "", "", false
}
