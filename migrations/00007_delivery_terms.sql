-- +goose Up

-- What a restaurant charges to deliver, and the smallest basket it delivers.
-- Both in kuruş like every other amount; 0 means "free delivery" and "no minimum".
ALTER TABLE restaurants
    ADD COLUMN delivery_fee_kurus integer NOT NULL DEFAULT 0
        CHECK (delivery_fee_kurus BETWEEN 0 AND 50000),
    ADD COLUMN min_basket_kurus integer NOT NULL DEFAULT 0
        CHECK (min_basket_kurus BETWEEN 0 AND 500000);

-- +goose Down
ALTER TABLE restaurants DROP COLUMN min_basket_kurus, DROP COLUMN delivery_fee_kurus;
