-- 种子:两个渠道的占位配置,给本地跑通与阅读示例用。
--
-- 这里的 platform JSON 全部是**假值**:域名用 RFC 2606 保留的 .invalid(保证解析不到),
-- IP 用 RFC 5737 文档段,密钥与口令是显式的 REPLACE_ME_ 占位。结构与各 adapter 的
-- Platform 结构体一一对应(见 internal/channel/adapter/<name>/config.go),照着填真值即可。
--
-- 真实渠道凭证一律不入仓库:由运维在部署环境直接写库,或经密钥管理下发。
-- +goose Up
INSERT INTO channels (channel_name, merchant_no, currency, channel_level, callback_headers,
                      callback_data_source, callback_return, callback_ip_whitelist, payout_supports,
                      limit_payment_min, limit_payment_max, limit_payout_min, limit_payout_max,
                      payment_commission_rate, payment_commission_extra,
                      payout_commission_rate, payout_commission_extra,
                      platform, reconcile_enabled)
VALUES ('payapay', 'example-merchant', 'INR', 4, '[]',
        1, 'success',
        '192.0.2.10,192.0.2.11,198.51.100.20',
        '[1]',
        20000, 5000000, 30000, 5000000,
        0, 0, 0, 0,
        '{"base_url":"https://payapay.example.invalid","apis":{"payment":"/api/v1/payApi/CreatePayInOrder","payment_query":"/api/v1/payApi/QueryOrder","payout":"/api/v1/payApi/CreatePayOutOrder","payout_query":"/api/v1/payApi/QueryOrder","balance_query":"/api/v1/payApi/QueryBalance"},"mer_id":10001,"app_id":10002,"app_secret":"REPLACE_ME_payapay_app_secret","pay_in_code":3,"pay_out_code":4}',
        FALSE),
       ('neokred', 'example-001', 'INR', 4, '[]',
        1, 'SUCCESS', '',
        '[1]',
        100, 10000000, 10000, 10000000,
        42, 0, 0, 0,
        '{"email":"ops@example.invalid","password":"REPLACE_ME_dashboard_password","dashboard_apis":{"login":"https://neokred.example.invalid/user-svc/api/v1/user/login/single-signin","query":"https://neokred.example.invalid/core-svc/api/v1/transaction/list","balance":"https://neokred.example.invalid/core-svc/api/v1/service/client/payout/balance"},"payment":{"client_secret":"REPLACE_ME_payin_client_secret","program_id":"REPLACE_ME_payin_program_id","apis":{"order":"https://neokred.example.invalid/payin/fn/api/v1/external/upi/qr/generate","query":"https://neokred.example.invalid/payin/fn/api/v1/external/upi/qr/status"}},"payout":{"client_secret":"REPLACE_ME_payout_client_secret","program_id":"REPLACE_ME_payout_program_id","apis":{"order":"https://neokred.example.invalid/ax-svc/api/v1/external/va/direct-transfer","query":"https://neokred.example.invalid/ax-svc/api/v1/external/va/transaction-status"}}}',
        FALSE);

-- +goose Down
DELETE FROM channels WHERE (channel_name, merchant_no) IN (('payapay', 'example-merchant'), ('neokred', 'example-001'));
