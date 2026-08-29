-- +goose Up
CREATE TABLE merchants (
    id BIGSERIAL PRIMARY KEY,
    app_id TEXT NOT NULL, app_secret TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
    status INT NOT NULL DEFAULT 1,
    ip_whitelist TEXT NOT NULL DEFAULT '[]',
    limit_min BIGINT NOT NULL DEFAULT 0, limit_max BIGINT NOT NULL DEFAULT 0,
    fee_rate INT NOT NULL DEFAULT 0, fee_extra INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_merchant_app UNIQUE (app_id)
);
CREATE TABLE channel_instances (
    id BIGSERIAL PRIMARY KEY,
    channel_name TEXT NOT NULL, merchant_no TEXT NOT NULL, currency TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    limit_payment_min BIGINT NOT NULL DEFAULT 0, limit_payment_max BIGINT NOT NULL DEFAULT 0,
    callback_headers TEXT NOT NULL DEFAULT '[]', callback_data_source INT NOT NULL DEFAULT 1,
    callback_return TEXT NOT NULL DEFAULT '', callback_ip_whitelist TEXT NOT NULL DEFAULT '',
    synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_instance_route UNIQUE (channel_name, merchant_no, currency)
);
CREATE TABLE merchant_channels (
    id BIGSERIAL PRIMARY KEY,
    merchant_id BIGINT NOT NULL, channel_instance_id BIGINT NOT NULL,
    priority INT NOT NULL DEFAULT 100, enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_merchant_channel UNIQUE (merchant_id, channel_instance_id)
);
CREATE TABLE payment_orders (
    id BIGSERIAL PRIMARY KEY,
    order_no TEXT NOT NULL, merchant_id BIGINT NOT NULL, mch_order_no TEXT NOT NULL,
    amount BIGINT NOT NULL, fee BIGINT NOT NULL DEFAULT 0, currency TEXT NOT NULL,
    status INT NOT NULL DEFAULT 1,
    channel_instance_id BIGINT NOT NULL DEFAULT 0,
    out_order_no TEXT NOT NULL DEFAULT '', reference_no TEXT NOT NULL DEFAULT '',
    pay_url TEXT NOT NULL DEFAULT '', notify_url TEXT NOT NULL DEFAULT '',
    response TEXT NOT NULL DEFAULT '',
    completed_at TIMESTAMPTZ, notify_status INT NOT NULL DEFAULT 0, notified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_order_no UNIQUE (order_no),
    CONSTRAINT uniq_merchant_order UNIQUE (merchant_id, mch_order_no)
);
CREATE INDEX idx_order_reconcile ON payment_orders (channel_instance_id, status, created_at);
CREATE INDEX idx_order_notify ON payment_orders (notify_status, completed_at);
CREATE TABLE callbacks (
    id BIGSERIAL PRIMARY KEY,
    channel_instance_id BIGINT NOT NULL, source INT NOT NULL DEFAULT 1,
    headers TEXT NOT NULL DEFAULT '{}', query TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '',
    ip TEXT NOT NULL DEFAULT '', status INT NOT NULL DEFAULT 1,
    order_no TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE order_notifications (
    id BIGSERIAL PRIMARY KEY,
    order_no TEXT NOT NULL, attempt INT NOT NULL,
    response_code INT NOT NULL DEFAULT 0, response_body TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_notification_order ON order_notifications (order_no, created_at);

-- +goose Down
DROP TABLE order_notifications;
DROP TABLE callbacks;
DROP TABLE payment_orders;
DROP TABLE merchant_channels;
DROP TABLE channel_instances;
DROP TABLE merchants;
