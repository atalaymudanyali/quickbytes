package outbox

import (
	"context"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"
)

// KafkaPublisher publishes events to Redpanda (or any Kafka) with franz-go.
type KafkaPublisher struct {
	client *kgo.Client
}

// NewKafkaPublisher makes a publisher for these brokers ("host:port"). It
// doesn't connect yet: a broker that's down at startup only makes the first
// rounds fail, and the relay retries.
func NewKafkaPublisher(brokers []string) (*KafkaPublisher, error) {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		// The broker confirms a message only once every copy of it is
		// stored, so "published" really means saved.
		kgo.RequiredAcks(kgo.AllISRAcks()),
		// Let the relay's timeout win, even over a message already sent. By
		// default the idempotent client never gives up on a sent message
		// (it can't tell "never arrived" from "stored, but the answer was
		// lost") and waits for the broker forever, which would keep the
		// relay's transaction, and its row locks, open for as long as
		// Redpanda is down. Giving up may mean the message was stored after
		// all and is published again next round: a repeat, which consumers
		// already drop by event ID.
		kgo.AllowIdempotentProduceCancellation(),
		// Write the batch out at once instead of waiting a moment for more
		// messages: the relay already sends in batches.
		kgo.ProducerLinger(0),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return &KafkaPublisher{client: client}, nil
}

// Publish sends the events, each keyed by its aggregate ID, and waits until
// the broker has stored all of them.
//
// The key picks the partition (the same key always lands on the same one),
// and a partition keeps its messages in the order they were written. So one
// order's events are read back in order. The client is idempotent (franz-go's
// default): its own retries can't write a message twice or swap two.
func (p *KafkaPublisher) Publish(ctx context.Context, events []Event) error {
	records := make([]*kgo.Record, len(events))
	for i, e := range events {
		records[i] = &kgo.Record{
			Topic: e.Topic,
			Key:   []byte(e.AggregateID.String()),
			Value: e.Payload,
			// Also in the payload; as headers, a consumer can filter or
			// drop repeats without decoding the body.
			Headers: []kgo.RecordHeader{
				{Key: "event-id", Value: []byte(e.ID.String())},
				{Key: "event-type", Value: []byte(e.Type)},
			},
		}
	}
	return p.client.ProduceSync(ctx, records...).FirstErr()
}

// Close stops the client.
func (p *KafkaPublisher) Close() {
	p.client.Close()
}
