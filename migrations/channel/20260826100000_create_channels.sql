-- channels 表:渠道商户实例配置,一行对应原「一商户一分支一进程」。
-- platform 为渠道私有配置 JSON(结构因渠道而异,由 adapter 反序列化)。
-- 执行:make migrate-up SVC=channel
-- +goose Up
CREATE TABLE channels (
    id                      BIGSERIAL PRIMARY KEY,
    channel_name            TEXT        NOT NULL,
    merchant_no             TEXT        NOT NULL,
    currency                TEXT        NOT NULL,
    channel_level           INT         NOT NULL DEFAULT 2,
    callback_headers        TEXT        NOT NULL DEFAULT '[]',
    callback_data_source    INT         NOT NULL DEFAULT 1,
    callback_return         TEXT        NOT NULL DEFAULT '',
    callback_ip_whitelist   TEXT        NOT NULL DEFAULT '',
    payout_supports         TEXT        NOT NULL DEFAULT '[]',
    limit_payment_min       BIGINT      NOT NULL DEFAULT 0,
    limit_payment_max       BIGINT      NOT NULL DEFAULT 0,
    limit_payout_min        BIGINT      NOT NULL DEFAULT 0,
    limit_payout_max        BIGINT      NOT NULL DEFAULT 0,
    payment_commission_rate  INT        NOT NULL DEFAULT 0,
    payment_commission_extra INT        NOT NULL DEFAULT 0,
    payout_commission_rate   INT        NOT NULL DEFAULT 0,
    payout_commission_extra  INT        NOT NULL DEFAULT 0,
    platform                JSONB       NOT NULL DEFAULT '{}',
    reconcile_enabled       BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_channel_route UNIQUE (channel_name, merchant_no, currency)
);

-- +goose Down
DROP TABLE IF EXISTS channels;
