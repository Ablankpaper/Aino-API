ALTER TABLE payment_orders
    ADD COLUMN IF NOT EXISTS client_order_id varchar(36),
    ADD COLUMN IF NOT EXISTS request_hash varchar(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS creation_state varchar(20) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS creation_lease_token varchar(36) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS creation_lease_until timestamptz,
    ADD COLUMN IF NOT EXISTS confirmation_required boolean NOT NULL DEFAULT false;

CREATE UNIQUE INDEX IF NOT EXISTS paymentorder_user_id_client_order_id
    ON payment_orders (user_id, client_order_id)
    WHERE client_order_id IS NOT NULL AND client_order_id <> '';
