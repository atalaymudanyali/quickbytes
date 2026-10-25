-- +goose Up

-- Where an order is in its life. Only the moves in internal/orders are allowed
-- between them; picked_up and delivered wait for drivers (phase 3).
CREATE TYPE order_status AS ENUM (
    'placed', 'paid', 'accepted', 'ready', 'picked_up', 'delivered', 'cancelled'
);

-- Why a cancelled order was cancelled.
CREATE TYPE cancel_reason AS ENUM ('payment_failed', 'rejected', 'by_customer', 'expired');

-- An order is a receipt: everything the customer was shown at checkout is
-- copied in (restaurant name, address, prices), so later edits to the menu, the
-- restaurant or the address book never change it. The IDs are kept alongside
-- for "who" and "where", not for reading the details back.
--
-- ON DELETE CASCADE: nothing in the API deletes a user or a restaurant; only
-- the demo reset does, and it must be able to take their orders with it. A real
-- shop would keep the orders (see UPGRADES).
CREATE TABLE orders (
    id                  uuid PRIMARY KEY,
    customer_profile_id uuid NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
    restaurant_id       uuid NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    restaurant_name     text NOT NULL,
    status              order_status NOT NULL DEFAULT 'placed',
    cancel_reason       cancel_reason,
    -- The delivery address, copied (addresses can be edited or deleted).
    address_label       text NOT NULL,
    address_line        text NOT NULL,
    address_details     text NOT NULL,
    address_location    geography(Point, 4326) NOT NULL,
    subtotal_kurus      integer NOT NULL CHECK (subtotal_kurus > 0),
    delivery_fee_kurus  integer NOT NULL CHECK (delivery_fee_kurus >= 0),
    total_kurus         integer NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- A reason exactly when cancelled: never a cancelled order without one, and
    -- never a leftover reason on an order that's still going.
    CONSTRAINT orders_cancel_reason_check CHECK ((status = 'cancelled') = (cancel_reason IS NOT NULL)),
    CONSTRAINT orders_total_check CHECK (total_kurus = subtotal_kurus + delivery_fee_kurus)
);

-- "My orders, newest first" for a customer, and "incoming orders" for a
-- restaurant.
CREATE INDEX orders_customer_profile_id_idx ON orders (customer_profile_id, created_at);
CREATE INDEX orders_restaurant_id_idx ON orders (restaurant_id, created_at);

-- The expiry loop asks "which placed orders are older than 15 minutes?". A
-- partial index holds only the rows that match its WHERE, so it stays tiny: an
-- order leaves it as soon as it's paid or cancelled.
CREATE INDEX orders_placed_created_at_idx ON orders (created_at) WHERE status = 'placed';

-- One dish on the order, as it was at checkout. unit_price_kurus is the dish's
-- price plus its extras; menu_item_id is only a reference (no foreign key), since
-- the dish may later be deleted while the order stays.
CREATE TABLE order_items (
    id               uuid PRIMARY KEY,
    order_id         uuid NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    menu_item_id     uuid NOT NULL,
    name             text NOT NULL,
    quantity         integer NOT NULL CHECK (quantity BETWEEN 1 AND 20),
    unit_price_kurus integer NOT NULL CHECK (unit_price_kurus > 0),
    line_total_kurus integer NOT NULL,
    -- The order lines were in, in the cart.
    position         integer NOT NULL,
    CONSTRAINT order_items_line_total_check CHECK (line_total_kurus = unit_price_kurus * quantity)
);

CREATE INDEX order_items_order_id_idx ON order_items (order_id, position);

-- One picked choice on a dish, copied with its group's name and its extra price.
CREATE TABLE order_item_options (
    id                uuid PRIMARY KEY,
    order_item_id     uuid NOT NULL REFERENCES order_items (id) ON DELETE CASCADE,
    choice_id         uuid NOT NULL,
    group_name        text NOT NULL,
    choice_name       text NOT NULL,
    extra_price_kurus integer NOT NULL CHECK (extra_price_kurus >= 0),
    position          integer NOT NULL
);

CREATE INDEX order_item_options_order_item_id_idx ON order_item_options (order_item_id, position);

-- Every status change, written in the same transaction as the change itself. The
-- first row of each order has no from_status: it's the order being placed.
--
-- by_profile_id is who pressed the button; it's empty when the system did
-- (expiry, the payment flow). No foreign key: this is a record of what happened,
-- and it should read the same whatever happens to the profile later.
CREATE TABLE order_status_history (
    id            uuid PRIMARY KEY,
    order_id      uuid NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    from_status   order_status,
    to_status     order_status NOT NULL,
    cancel_reason cancel_reason,
    by_role       text NOT NULL CHECK (by_role IN ('customer', 'restaurant', 'driver', 'system')),
    by_profile_id uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT order_status_history_by_check CHECK ((by_role = 'system') = (by_profile_id IS NULL))
);

CREATE INDEX order_status_history_order_id_idx ON order_status_history (order_id, created_at);

-- +goose Down
DROP TABLE order_status_history;
DROP TABLE order_item_options;
DROP TABLE order_items;
DROP TABLE orders;
DROP TYPE cancel_reason;
DROP TYPE order_status;
