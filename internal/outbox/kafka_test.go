package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// kfake is an in-memory Kafka (from franz-go), so this runs without Redpanda,
// in CI too. The topic is made like `make topics` makes Redpanda's: 3
// partitions, and no topic is created by writing to it.
func fakeCluster(t *testing.T) *kfake.Cluster {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(3, "orders"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func publisher(t *testing.T, c *kfake.Cluster) *KafkaPublisher {
	t.Helper()
	p, err := NewKafkaPublisher(c.ListenAddrs())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestKafkaPublisherSendsKeyedEvents(t *testing.T) {
	c := fakeCluster(t)
	p := publisher(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, b := uuid.New(), uuid.New()
	var events []Event
	for _, agg := range []uuid.UUID{a, b, a, b, a} {
		id := uuid.New()
		events = append(events, Event{
			ID: id, Topic: "orders", AggregateID: agg, Type: "order.test",
			Payload: []byte(`{"id":"` + id.String() + `"}`),
		})
	}
	if err := p.Publish(ctx, events); err != nil {
		t.Fatal(err)
	}

	consumer, err := kgo.NewClient(kgo.SeedBrokers(c.ListenAddrs()...),
		kgo.ConsumeTopics("orders"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	var got []*kgo.Record
	for len(got) < len(events) {
		fetches := consumer.PollFetches(ctx)
		if err := fetches.Err(); err != nil {
			t.Fatal(err)
		}
		got = append(got, fetches.Records()...)
	}

	partitionOf := map[string]int32{}
	byKey := map[string][]string{}
	for _, r := range got {
		key := string(r.Key)
		if p, ok := partitionOf[key]; ok && p != r.Partition {
			t.Fatalf("key %s on partitions %d and %d, want one", key, p, r.Partition)
		}
		partitionOf[key] = r.Partition
		headers := map[string]string{}
		for _, h := range r.Headers {
			headers[h.Key] = string(h.Value)
		}
		if r.Topic != "orders" || headers["event-type"] != "order.test" ||
			string(r.Value) != `{"id":"`+headers["event-id"]+`"}` {
			t.Fatalf("record topic %s, headers %v, value %s: want the event as published", r.Topic, headers, r.Value)
		}
		byKey[key] = append(byKey[key], headers["event-id"])
	}
	for _, agg := range []uuid.UUID{a, b} {
		var want []string
		for _, e := range events {
			if e.AggregateID == agg {
				want = append(want, e.ID.String())
			}
		}
		got := byKey[agg.String()]
		if len(got) != len(want) {
			t.Fatalf("key %s: %v, want %v", agg, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("key %s read back as %v, want %v (the order published)", agg, got, want)
			}
		}
	}
}

// A topic that doesn't exist isn't created by publishing to it (a typo
// fails, like on Redpanda with make topics), and the error reaches the relay.
func TestKafkaPublisherRefusesAMissingTopic(t *testing.T) {
	p := publisher(t, fakeCluster(t))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := p.Publish(ctx, []Event{{ID: uuid.New(), Topic: "oders", AggregateID: uuid.New(), Type: "order.test", Payload: []byte(`{}`)}})
	if err == nil {
		t.Fatal("published to a topic that doesn't exist")
	}
}

// The broker takes the message but never answers (it stopped mid-request).
// Publish must give up when its context ends, not wait for the broker:
// otherwise the relay's transaction, and its row locks, stay open as long as
// Redpanda is down.
func TestKafkaPublisherGivesUpWhenTheBrokerStopsAnswering(t *testing.T) {
	c := fakeCluster(t)
	p := publisher(t, c)
	answer := make(chan struct{})
	held := false
	c.ControlKey(kmsg.Produce.Int16(), func(kmsg.Request) (kmsg.Response, error, bool) {
		c.KeepControl()
		if !held {
			held = true
			c.SleepControl(func() { <-answer })
		}
		return nil, nil, false // then handle it as usual
	})
	// So the test ends even if Publish waits for the answer.
	go func() {
		time.Sleep(5 * time.Second)
		close(answer)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	err := p.Publish(ctx, []Event{{ID: uuid.New(), Topic: "orders", AggregateID: uuid.New(), Type: "order.test", Payload: []byte(`{}`)}})
	took := time.Since(start)
	if err == nil {
		t.Fatal("want an error when the broker never answered in time")
	}
	if took > 3*time.Second {
		t.Fatalf("Publish took %v with a 1 s context: it waited for the broker", took.Round(time.Millisecond))
	}
}
