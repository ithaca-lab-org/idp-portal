package orders

import (
	"context"
	"time"
)

// Event types published when an order reaches a final outcome. The notifier turns them into
// emails; consumers must ignore types they do not know.
const (
	EventComplete = "order.complete"
	EventFailed   = "order.failed"
)

const maxEventMessage = 200

// Event is the message on the order-events topic. It never carries secrets or session data.
type Event struct {
	Version      int       `json:"version"`
	Type         string    `json:"type"`
	OrderID      string    `json:"orderId"`
	Kind         string    `json:"kind"`
	Name         string    `json:"name"`
	BusinessUnit string    `json:"businessUnit"`
	Owner        string    `json:"owner"` // the owner's email, the notification recipient
	Size         string    `json:"size"`
	Status       Status    `json:"status"`
	Message      string    `json:"message,omitempty"`
	At           time.Time `json:"at"`
}

// Publisher sends order events. Publish must not block the caller for long and must not fail the
// order: delivery is best effort (see Service.emit).
type Publisher interface {
	Publish(ctx context.Context, e Event) error
}

func newEvent(o *Order, to Status, msg string, at time.Time) (Event, bool) {
	var typ string
	switch to {
	case StatusComplete:
		typ = EventComplete
	case StatusFailed:
		typ = EventFailed
	default:
		return Event{}, false
	}
	if len(msg) > maxEventMessage {
		msg = msg[:maxEventMessage]
	}
	return Event{Version: 1, Type: typ, OrderID: o.ID, Kind: o.Kind, Name: o.Name, BusinessUnit: o.BusinessUnit,
		Owner: o.Owner, Size: o.Size, Status: to, Message: msg, At: at.UTC()}, true
}

// emit publishes the event for a status change this process just made. A failure is logged and
// dropped: the order is already stored, and an unreachable bus must not fail or stall it. (A
// transactional outbox would close the gap where the process dies between the update and the
// publish; it is not worth it until a consumer cannot tolerate a missed email.)
func (s *Service) emit(ctx context.Context, o *Order, to Status, msg string) {
	if s.Events == nil {
		return
	}
	e, ok := newEvent(o, to, msg, s.Now())
	if !ok {
		return
	}
	if err := s.Events.Publish(ctx, e); err != nil {
		logPublishFailure(o.ID, e.Type, err)
	}
}
