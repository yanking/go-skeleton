package neokred

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
	"github.com/yanking/go-skeleton/pkg/httpc"
	"golang.org/x/sync/singleflight"
)

// tokenHolder Dashboard 登录态：缓存 token，刷新经 singleflight 防并发重复登入。
type tokenHolder struct {
	conf    Platform
	http    *httpc.Client
	barrier singleflight.Group
	mu      sync.RWMutex
	token   string
}

func newTokenHolder(conf Platform, hc *httpc.Client) *tokenHolder {
	return &tokenHolder{conf: conf, http: hc}
}

// loginResult 登录响应。
type loginResult struct {
	baseResult
	Data struct {
		Token string `json:"token"`
	} `json:"data"`
}

// fetch 取缓存 token，无则触发登录。
func (t *tokenHolder) fetch(ctx context.Context) (string, error) {
	t.mu.RLock()
	token := t.token
	t.mu.RUnlock()
	if token != "" {
		return token, nil
	}
	return t.refresh(ctx)
}

// refresh 登录换取新 token；singleflight 保证并发调用只登一次。
func (t *tokenHolder) refresh(ctx context.Context) (string, error) {
	_, err, _ := t.barrier.Do("login", func() (any, error) {
		params := map[string]any{
			"email":    t.conf.Email,
			"password": t.conf.Password,
		}
		code, body, err := t.http.PostJSON(ctx, t.conf.DashboardAPIs.Login, nil, params, 0)
		if err != nil {
			return nil, fmt.Errorf("登录 neokred: %w", err)
		}
		if code != 200 {
			return nil, fmt.Errorf("%w: 登录 http %d, body %s", adapter.ErrChannelRejected, code, body)
		}

		var result loginResult
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			return nil, fmt.Errorf("%w: 登录响应 %s: %v", adapter.ErrBadResponse, body, err)
		}
		if result.StatusCode != 200 {
			return nil, fmt.Errorf("%w: 登录业务失败, body %s", adapter.ErrChannelRejected, body)
		}

		t.mu.Lock()
		t.token = "Bearer " + result.Data.Token
		t.mu.Unlock()
		return nil, nil
	})
	if err != nil {
		return "", err
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.token, nil
}
