-- +goose Up

-- The shopping basket. One per customer (the UNIQUE), for one restaurant at a
-- time. It holds no prices: totals are worked out from the live menu on every
-- read, so a basket can never show an old price.
CREATE TABLE carts (
    id                  uuid PRIMARY KEY,
    customer_profile_id uuid NOT NULL UNIQUE REFERENCES profiles (id) ON DELETE CASCADE,
    restaurant_id       uuid NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- Lets cart_items point at (cart, restaurant) together, below.
    UNIQUE (id, restaurant_id)
);

-- One line in the basket: a dish, how many, and the picked option choices.
--
-- restaurant_id is repeated here so the database itself can enforce "every dish
-- in a cart comes from the cart's restaurant": both foreign keys below share it,
-- so a dish from another restaurant can't match both.
--
-- choice_ids is kept sorted, so the same picks always give the same array and
-- "same dish, same options" can be one UNIQUE constraint. The choices aren't
-- foreign keys on purpose: an owner can delete a choice, and the line should
-- then be flagged (the options changed), not silently changed or dropped.
CREATE TABLE cart_items (
    id            uuid PRIMARY KEY,
    cart_id       uuid NOT NULL,
    restaurant_id uuid NOT NULL,
    menu_item_id  uuid NOT NULL,
    quantity      integer NOT NULL CHECK (quantity BETWEEN 1 AND 20),
    choice_ids    uuid[] NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (cart_id, restaurant_id) REFERENCES carts (id, restaurant_id) ON DELETE CASCADE,
    FOREIGN KEY (menu_item_id, restaurant_id) REFERENCES menu_items (id, restaurant_id) ON DELETE CASCADE,
    UNIQUE (cart_id, menu_item_id, choice_ids)
);

-- The UNIQUE above starts with cart_id, so it also serves "this cart's lines".
-- These two serve the cascades when a restaurant or a dish is deleted.
CREATE INDEX cart_items_menu_item_id_idx ON cart_items (menu_item_id, restaurant_id);
CREATE INDEX carts_restaurant_id_idx ON carts (restaurant_id);

-- +goose Down
DROP TABLE cart_items;
DROP TABLE carts;
