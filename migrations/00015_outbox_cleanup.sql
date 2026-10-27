-- +goose Up

-- Published outbox rows are kept for a week (to look into what was sent,
-- or publish again by hand), then deleted by the API's cleanup loop. This
-- index lets the cleanup find "published more than 7 days ago" without
-- reading the whole table. It holds only published rows; the waiting ones
-- have their own indexes (00014).
CREATE INDEX outbox_published_idx ON outbox (published_at) WHERE published_at IS NOT NULL;

-- +goose Down
DROP INDEX outbox_published_idx;
