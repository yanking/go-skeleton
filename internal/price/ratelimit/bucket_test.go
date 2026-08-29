package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestBucket_SecondCallWaitsForRefill(t *testing.T) {
	b := New(10, 1) // 每秒 10 个,桶容量 1
	ctx := context.Background()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("第二次取用只等了 %v, want ≥50ms(令牌未耗尽说明没限住)", elapsed)
	}
}

func TestBucket_RespectsContextCancel(t *testing.T) {
	b := New(0.1, 1)
	_ = b.Wait(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.Wait(ctx); err == nil {
		t.Error("ctx 超时后 Wait 应返回 error,而不是一直等")
	}
}
