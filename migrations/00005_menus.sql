-- +goose Up

-- Goes up by one on every menu change. A cache (a later issue) can compare it with
-- the version it stored to know whether its copy of the menu is out of date.
ALTER TABLE restaurants ADD COLUMN menu_version bigint NOT NULL DEFAULT 1;

-- Sections of a menu: "Çorbalar", "Kebaplar", "İçecekler".
CREATE TABLE menu_categories (
    id            uuid PRIMARY KEY,
    restaurant_id uuid NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    -- Display order: 1 first. Not unique: two equal positions are shown in ID order.
    position      integer NOT NULL,
    -- Set when the owner removes the category (a "soft delete"): hidden from the
    -- menu, but kept so it can be restored and old orders stay readable.
    archived_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- Lets menu_items point at (category, restaurant) together, below.
    UNIQUE (id, restaurant_id)
);

CREATE INDEX menu_categories_restaurant_id_idx ON menu_categories (restaurant_id);

CREATE TABLE menu_items (
    id            uuid PRIMARY KEY,
    -- Stored on the item too, so "this restaurant's items" needs no join, and the
    -- ownership check works the same way for items as for categories.
    restaurant_id uuid NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    category_id   uuid NOT NULL,
    name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    description   text NOT NULL DEFAULT '' CHECK (length(description) <= 500),
    -- Kuruş as a whole number: 12550 = 125.50 TL. At most 100,000 TL.
    price_kurus   integer NOT NULL CHECK (price_kurus > 0 AND price_kurus <= 10000000),
    -- false = sold out for now: still shown, but can't be ordered.
    available     boolean NOT NULL DEFAULT true,
    position      integer NOT NULL,
    archived_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- The item's category must belong to the same restaurant as the item. Pointing
    -- at the pair, not just the category, makes the database enforce that.
    FOREIGN KEY (category_id, restaurant_id)
        REFERENCES menu_categories (id, restaurant_id) ON DELETE CASCADE
);

CREATE INDEX menu_items_restaurant_id_idx ON menu_items (restaurant_id);
CREATE INDEX menu_items_category_id_idx ON menu_items (category_id, restaurant_id);

-- +goose Down
DROP TABLE menu_items;
DROP TABLE menu_categories;
ALTER TABLE restaurants DROP COLUMN menu_version;
