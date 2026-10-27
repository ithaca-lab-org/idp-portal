package notify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ithaca-lab-org/idp-portal/notifier/internal/mail"
)

type rec struct {
	got []mail.Message
	err error
}

func (r *rec) Send(_ context.Context, m mail.Message) error {
	r.got = append(r.got, m)
	return r.err
}

const complete = `{"version":1,"type":"order.complete","orderId":"ORD-2026-000001","name":"team-a","businessUnit":"payment-bu","owner":"a@goliathshop.com","size":"small"}`

func TestCompleteSendsOneEmail(t *testing.T) {
	r := &rec{}
	if err := (&Handler{Sender: r}).Handle(context.Background(), []byte(complete)); err != nil {
		t.Fatal(err)
	}
	if len(r.got) != 1 || r.got[0].To != "a@goliathshop.com" || !strings.Contains(r.got[0].Subject, "team-a") ||
		!strings.Contains(r.got[0].Text, "payment-bu") {
		t.Fatalf("%+v", r.got)
	}
}

func TestFailedIncludesReasonOnOneLine(t *testing.T) {
	r := &rec{}
	ev := `{"version":1,"type":"order.failed","orderId":"ORD-2026-000002","name":"x","businessUnit":"b","owner":"a@x.com","message":"bad\nBcc: evil@x.com"}`
	if err := (&Handler{Sender: r}).Handle(context.Background(), []byte(ev)); err != nil {
		t.Fatal(err)
	}
	if len(r.got) != 1 || strings.Contains(r.got[0].Text, "\nBcc:") {
		t.Fatalf("%+v", r.got)
	}
}

func TestUnusableEventsAreDroppedWithoutRetry(t *testing.T) {
	for name, ev := range map[string]string{
		"garbage":      `not json`,
		"version":      `{"version":2,"type":"order.complete","owner":"a@x.com"}`,
		"unknown type": `{"version":1,"type":"order.other","owner":"a@x.com"}`,
		"no recipient": `{"version":1,"type":"order.complete","owner":"nobody"}`,
	} {
		r := &rec{}
		if err := (&Handler{Sender: r}).Handle(context.Background(), []byte(ev)); err != nil || len(r.got) != 0 {
			t.Fatalf("%s: err %v sent %d", name, err, len(r.got))
		}
	}
}

func TestTransientErrorsAreRetriedPermanentOnesAreNot(t *testing.T) {
	h := &Handler{Sender: &rec{err: errors.New("timeout")}}
	if err := h.Handle(context.Background(), []byte(complete)); err == nil {
		t.Fatal("transient error must be returned so the message is redelivered")
	}
	h = &Handler{Sender: &rec{err: mail.ErrPermanent}}
	if err := h.Handle(context.Background(), []byte(complete)); err != nil {
		t.Fatalf("permanent error must be dropped: %v", err)
	}
}
