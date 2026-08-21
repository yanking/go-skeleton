CREATE TABLE users (
    id         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '用户 ID',
    name       VARCHAR(32)  NOT NULL COMMENT '用户名',
    email      VARCHAR(255) NOT NULL COMMENT '邮箱',
    created_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (id),
    UNIQUE KEY uk_users_email (email)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COMMENT '用户表';
