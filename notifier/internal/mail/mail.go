// Package mail sends notification emails. LogSender is the default: it writes what would be sent
// and sends nothing, so the whole pipeline can run before a real provider is switched on.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

type Message struct {
	To      string
	Subject string
	Text    string
}

type Sender interface {
	Send(ctx context.Context, m Message) error
}

// ErrPermanent marks a failure that retrying cannot fix (bad address, rejected request).
var ErrPermanent = errors.New("permanent send failure")

// LogSender logs the message metadata. The body is not logged: it holds user data.
type LogSender struct{}

func (LogSender) Send(_ context.Context, m Message) error {
	slog.Info("email sending is disabled, would send", "to", m.To, "subject", m.Subject)
	return nil
}

// Resend sends through the Resend HTTP API (https://resend.com/docs/api-reference/emails/send-email).
type Resend struct {
	APIKey string
	From   string
	URL    string // defaults to the public API; tests point it elsewhere
	Client *http.Client
}

func NewResend(apiKey, from string) *Resend {
	return &Resend{APIKey: apiKey, From: from, URL: "https://api.resend.com/emails",
		Client: &http.Client{Timeout: 15 * time.Second}}
}

func (r *Resend) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(map[string]any{"from": r.From, "to": []string{m.To}, "subject": m.Subject, "text": m.Text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return err // network error: transient
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5:
		return fmt.Errorf("resend returned %d", resp.StatusCode) // retry later
	default:
		return fmt.Errorf("%w: resend returned %d", ErrPermanent, resp.StatusCode)
	}
}
