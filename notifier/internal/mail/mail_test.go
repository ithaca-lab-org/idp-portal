package mail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func resend(status int, got *map[string]any, auth *string) (*Resend, func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(got)
		w.WriteHeader(status)
	}))
	r := NewResend("key123", "IDP <idp@example.com>")
	r.URL = srv.URL
	return r, srv.Close
}

func TestResendSendsExpectedRequest(t *testing.T) {
	var body map[string]any
	var auth string
	r, done := resend(200, &body, &auth)
	defer done()
	if err := r.Send(context.Background(), Message{To: "a@x.com", Subject: "s", Text: "t"}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer key123" || body["from"] != "IDP <idp@example.com>" || body["subject"] != "s" {
		t.Fatalf("auth %q body %v", auth, body)
	}
}

func TestResendErrorClasses(t *testing.T) {
	for status, permanent := range map[int]bool{422: true, 401: true, 429: false, 503: false} {
		var body map[string]any
		var auth string
		r, done := resend(status, &body, &auth)
		err := r.Send(context.Background(), Message{To: "a@x.com"})
		done()
		if err == nil || errors.Is(err, ErrPermanent) != permanent {
			t.Fatalf("status %d: err %v", status, err)
		}
	}
}

func TestLogSenderSendsNothing(t *testing.T) {
	if err := (LogSender{}).Send(context.Background(), Message{To: "a@x.com"}); err != nil {
		t.Fatal(err)
	}
}
