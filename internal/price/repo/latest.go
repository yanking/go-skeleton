package repo

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Latest 行情最新值仓储：把最新一条推送写入 Redis，供查询侧直接读取，
// 不经数据库往返。
type Latest struct {
	rdb redis.UniversalClient
}

// NewLatest 构造仓储；rdb 句柄由装配层（initial）从 redis 组件解嵌传入。
func NewLatest(rdb redis.UniversalClient) *Latest {
	return &Latest{rdb: rdb}
}

// Set 写入 key 对应的最新行情快照。key 形如 price:{exchange}:{market}:{symbol}:{stream}，
// 由调用方拼好传入。
//
// 刻意不设 TTL：断流后若 key 过期消失，消费方看到的是「没有这个标的」（误导）；
// 不设 TTL 则消费方读到的是「有，但数据已陈旧」，后者才是事实真相——陈旧程度可
// 由 payload 内的时间戳自行判断。不要因为「看起来像忘了设过期时间」而顺手补上。
func (r *Latest) Set(ctx context.Context, key string, payload []byte) error {
	if err := r.rdb.Set(ctx, key, payload, 0).Err(); err != nil {
		return fmt.Errorf("写入最新行情: %w", err)
	}
	return nil
}
