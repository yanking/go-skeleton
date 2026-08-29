// Package repo 是 price 的数据访问层：实现 service 声明的仓储接口，把可识别的
// 底层错误翻译为 service 哨兵；GORM 等 ORM 类型不出本层。
package repo
