-- +goose Up
CREATE TABLE price_instruments (
    id BIGSERIAL PRIMARY KEY,
    exchange TEXT NOT NULL, market TEXT NOT NULL, native_symbol TEXT NOT NULL,
    symbol TEXT NOT NULL, base TEXT NOT NULL, quote TEXT NOT NULL,
    status INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_instrument UNIQUE (exchange, market, native_symbol)
);
CREATE TABLE price_subscriptions (
    id BIGSERIAL PRIMARY KEY,
    exchange TEXT NOT NULL, market TEXT NOT NULL, native_symbol TEXT NOT NULL,
    stream_type TEXT NOT NULL, interval TEXT NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_subscription UNIQUE (exchange, market, native_symbol, stream_type, interval)
);
CREATE TABLE price_klines (
    exchange TEXT NOT NULL, market TEXT NOT NULL, native_symbol TEXT NOT NULL,
    interval TEXT NOT NULL, open_time BIGINT NOT NULL,
    open NUMERIC NOT NULL, high NUMERIC NOT NULL, low NUMERIC NOT NULL, close NUMERIC NOT NULL,
    volume NUMERIC NOT NULL, quote_volume NUMERIC NOT NULL,
    source INT NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pk_kline PRIMARY KEY (exchange, market, native_symbol, interval, open_time)
);

-- +goose Down
DROP TABLE price_klines;
DROP TABLE price_subscriptions;
DROP TABLE price_instruments;
