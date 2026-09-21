-- One row per SKU. Every unit of initial_stock is in exactly one of
-- available, reserved, or sold.
CREATE TABLE stock (
    sku           TEXT PRIMARY KEY,
    initial_stock INTEGER NOT NULL,
    available     INTEGER NOT NULL,
    reserved      INTEGER NOT NULL,
    sold          INTEGER NOT NULL
);

-- One row per reservation ever made. A RESERVED row holds one unit of
-- stock.reserved; a CONFIRMED row accounts for one unit of stock.sold.
CREATE TABLE reservations (
    reservation_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sku            TEXT NOT NULL REFERENCES stock (sku),
    state          TEXT NOT NULL CHECK (state IN ('RESERVED', 'CONFIRMED', 'ABANDONED', 'EXPIRED')),
    deadline       TIMESTAMPTZ NOT NULL
);

-- One row per client operation identity, with the response that was returned for it.
CREATE TABLE operations (
    operation_id   TEXT PRIMARY KEY,
    operation_type TEXT NOT NULL CHECK (operation_type IN ('RESERVE', 'CONFIRM', 'ABANDON')),
    response       JSONB NOT NULL
);
