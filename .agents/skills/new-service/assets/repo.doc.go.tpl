// Package repo 是__scope__的数据访问层：实现 service 声明的接口——GORM 仓储，
// 以及 Redis、三方 HTTP 接口、邮件等外部依赖的适配；把可识别的底层错误翻译为
// service 哨兵；ORM 与三方 SDK 类型不出本层。
package repo
