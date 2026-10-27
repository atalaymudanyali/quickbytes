-- +goose Up

-- The outbox: events waiting to be published to Redpanda. An event is written
-- in the SAME transaction as the change it describes (an order moving to
-- paid...), so it exists if and only if that change committed. A relay then
-- publishes the rows that aren't published yet and stamps published_at.
--
-- id is the event's own ID, also inside the payload: delivery is "at least
-- once", so a consumer may see an event twice and drops repeats by this ID.
--
-- seq is one shared counter that orders the events. Not created_at: two API
-- machines' clocks can be a few milliseconds apart, but a counter can't. Two
-- moves on the same order are already one after the other (the order row is
-- locked), so the later move always gets the bigger seq.
--
-- aggregate_id is the thing the event is about (the order). It's the Kafka
-- key: every event with the same key goes to the same partition, in order.
CREATE TABLE outbox (
    id           uuid PRIMARY KEY,
    seq          bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    topic        text NOT NULL,
    aggregate_id uuid NOT NULL,
    type         text NOT NULL,
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);

-- Both indexes hold only the rows still waiting (partial indexes), so they
-- stay small however many published rows pile up. The first finds the oldest
-- waiting events; the second answers "does this order have an earlier event
-- still waiting?" (the relay publishes one order's events strictly in order).
CREATE INDEX outbox_waiting_idx ON outbox (seq) WHERE published_at IS NULL;
CREATE INDEX outbox_waiting_aggregate_idx ON outbox (aggregate_id, seq) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE outbox;
