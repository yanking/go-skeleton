package repo

import "errors"

// ErrRowNotFound 表示按主键或唯一键查询未命中记录，各仓储共用。
var ErrRowNotFound = errors.New("记录不存在")
