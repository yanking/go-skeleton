-- 种子:本地演示商户一行,供 make run 后直接跑通冒烟。
-- 仅本地/演示用——生产商户与密钥由运维另行入库,不入仓库。
-- 不种商户-渠道绑定:channel_instances 由实例同步任务从 channel 服务生成,
-- 绑定需引用真实实例 id,手工 INSERT 示例见 docs/payment/README.md。

-- +goose Up
INSERT INTO merchants (app_id, app_secret, name, status, ip_whitelist,
                       limit_min, limit_max, fee_rate, fee_extra)
VALUES ('demo', 'demo-secret-0000', '演示商户', 1, '[]',
        100, 1000000, 30, 0);

-- +goose Down
DELETE FROM merchants WHERE app_id = 'demo';
