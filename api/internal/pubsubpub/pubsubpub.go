// Package pubsubpub publishes order events to a Google Cloud Pub/Sub topic using Application
// Default Credentials (Workload Identity on GKE). The service account needs publish rights on
// the one topic and nothing else.
package pubsubpub

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cloud.google.com/go/pubsub/v2"

	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
)

const publishTimeout = 10 * time.Second

type Publisher struct {
	client *pubsub.Client
	topic  *pubsub.Publisher
}

// New connects to project and publishes to topicID (the short topic name).
func New(ctx context.Context, project, topicID string) (*Publisher, error) {
	c, err := pubsub.NewClient(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("pubsub client: %w", err)
	}
	return &Publisher{client: c, topic: c.Publisher(topicID)}, nil
}

// Publish sends the event and waits for the server to accept it, bounded by publishTimeout. The
// type is also an attribute so subscribers can filter without parsing the body.
func (p *Publisher) Publish(ctx context.Context, e orders.Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	_, err = p.topic.Publish(ctx, &pubsub.Message{
		Data:       data,
		Attributes: map[string]string{"type": e.Type, "orderId": e.OrderID},
	}).Get(ctx)
	return err
}

func (p *Publisher) Close() error {
	p.topic.Stop()
	return p.client.Close()
}
