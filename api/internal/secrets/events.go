package secrets

import (
	"context"
	"log/slog"
	"time"
)

// Audit event types, published on the same topic as order events. Consumers ignore types they
// do not know (the notifier does).
const (
	EventChanged = "secret.changed"
	EventDeleted = "secret.deleted"
)

// Event is the audit record of a change. It has no field for a value.
type Event struct {
	Version       int       `json:"version"`
	Type          string    `json:"type"`
	Namespace     string    `json:"namespace"`
	Name          string    `json:"name"`
	Actor         string    `json:"actor"` // the caller's session email
	Keys          []string  `json:"keys"`
	SecretVersion int       `json:"secretVersion,omitempty"` // the KV version written (or the last one, on delete)
	At            time.Time `json:"at"`
}

// Publisher sends audit events; delivery is best effort.
type Publisher interface {
	PublishSecretEvent(ctx context.Context, e Event) error
}

func (s *Service) emit(ctx context.Context, typ string, m Metadata) {
	if s.Events == nil {
		return
	}
	keys := m.Keys
	if keys == nil {
		keys = []string{}
	}
	e := Event{Version: 1, Type: typ, Namespace: m.Namespace, Name: m.Name, Actor: m.UpdatedBy,
		Keys: keys, SecretVersion: m.Version, At: s.Now().UTC()}
	if err := s.Events.PublishSecretEvent(ctx, e); err != nil {
		slog.Error("publish secret event", "type", typ, "namespace", m.Namespace, "name", m.Name, "err", err)
	}
}
