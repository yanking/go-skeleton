package neokred

import (
	"context"
	"fmt"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
)

// PaymentCallback Neokred 不提供代收回调，状态经查询轮询获知。
func (a *Client) PaymentCallback(context.Context, map[string]string, string) (adapter.CallbackOut, error) {
	return adapter.CallbackOut{}, fmt.Errorf("%w: neokred 状态只经查询轮询获知", adapter.ErrCallbackUnsupported)
}

// PayoutCallback Neokred 不提供代付回调。
func (a *Client) PayoutCallback(context.Context, map[string]string, string) (adapter.CallbackOut, error) {
	return adapter.CallbackOut{}, fmt.Errorf("%w: neokred 状态只经查询轮询获知", adapter.ErrCallbackUnsupported)
}
