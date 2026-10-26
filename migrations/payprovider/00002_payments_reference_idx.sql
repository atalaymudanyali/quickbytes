-- +goose Up

-- Serves "find my payments with this reference" (GET /v1/payments?reference=):
-- a merchant whose authorize call timed out never got the payment's ID, only
-- knows its own reference (QuickBytes: the order ID), and looks it up by that.
CREATE INDEX payments_merchant_reference_idx ON payments (merchant_id, reference, id);

-- +goose Down
DROP INDEX payments_merchant_reference_idx;
