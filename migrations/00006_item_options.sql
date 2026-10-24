-- +goose Up

-- Lets option_groups point at (item, restaurant) together, below.
ALTER TABLE menu_items ADD CONSTRAINT menu_items_id_restaurant_id_key UNIQUE (id, restaurant_id);

-- The questions a dish asks: "Size: pick exactly 1", "Extras: up to 3".
CREATE TABLE option_groups (
    id            uuid PRIMARY KEY,
    -- Stored here too, like on menu_items, so ownership checks and "this
    -- restaurant's options" need no join.
    restaurant_id uuid NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    item_id       uuid NOT NULL,
    name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    -- How many choices the customer must / may pick. min 0 = optional; min 1 =
    -- required; min = max = 1 = "pick exactly one".
    min_picks     integer NOT NULL,
    max_picks     integer NOT NULL,
    position      integer NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- Named, so the store can recognise it and answer with a clear error when an
    -- edit would break it (e.g. raising min_picks above the stored max_picks).
    CONSTRAINT option_groups_picks_check
        CHECK (min_picks >= 0 AND max_picks BETWEEN 1 AND 20 AND min_picks <= max_picks),
    -- The group's item must belong to the same restaurant as the group.
    FOREIGN KEY (item_id, restaurant_id)
        REFERENCES menu_items (id, restaurant_id) ON DELETE CASCADE,
    UNIQUE (id, restaurant_id)
);

CREATE INDEX option_groups_restaurant_id_idx ON option_groups (restaurant_id);
CREATE INDEX option_groups_item_id_idx ON option_groups (item_id, restaurant_id);

-- The answers to a group: "Large, +20 TL", "Extra cheese, +15 TL".
CREATE TABLE option_choices (
    id                uuid PRIMARY KEY,
    restaurant_id     uuid NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    group_id          uuid NOT NULL,
    name              text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    -- Added to the item's price. 0 is allowed ("Small, no extra charge").
    extra_price_kurus integer NOT NULL CHECK (extra_price_kurus BETWEEN 0 AND 10000000),
    -- false = out for now: still shown, but can't be picked.
    available         boolean NOT NULL DEFAULT true,
    position          integer NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    -- Deleting a group deletes its choices with it.
    FOREIGN KEY (group_id, restaurant_id)
        REFERENCES option_groups (id, restaurant_id) ON DELETE CASCADE
);

CREATE INDEX option_choices_restaurant_id_idx ON option_choices (restaurant_id);
CREATE INDEX option_choices_group_id_idx ON option_choices (group_id, restaurant_id);

-- +goose Down
DROP TABLE option_choices;
DROP TABLE option_groups;
ALTER TABLE menu_items DROP CONSTRAINT menu_items_id_restaurant_id_key;
