package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/user/biz"
	"github.com/yanking/go-skeleton/internal/user/server"
	"github.com/yanking/go-skeleton/internal/user/service"
	"github.com/yanking/go-skeleton/pkg/app"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// memRepo 是进程内的用户仓储，让这组端到端用例不依赖任何数据库，CI 里始终能跑。
type memRepo struct {
	users  map[int64]*biz.User
	nextID int64
}

func newMemRepo() *memRepo { return &memRepo{users: map[int64]*biz.User{}, nextID: 1} }

func (m *memRepo) Create(_ context.Context, u *biz.User) (*biz.User, error) {
	for _, existing := range m.users {
		if existing.Email == u.Email {
			return nil, biz.ErrEmailTaken
		}
	}
	saved := *u
	saved.ID = m.nextID
	saved.CreatedAt = time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	m.nextID++
	m.users[saved.ID] = &saved
	return &saved, nil
}

func (m *memRepo) Get(_ context.Context, id int64) (*biz.User, error) {
	u, ok := m.users[id]
	if !ok {
		return nil, biz.ErrUserNotFound
	}
	return u, nil
}

// run 起一整套传输层（双端口 ＋ 环回 ＋ 拦截器链），返回 HTTP 基址与停机函数。
func run(t *testing.T) (string, func()) {
	t.Helper()
	logger := discardLogger()
	uc := biz.NewUserUsecase(newMemRepo(), logger)
	svc := service.NewUserService(uc, logger)

	tp := server.New(context.Background(), server.Config{
		GRPCAddr: "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
	}, svc, nil, logger)

	ctx, cancel := context.WithCancel(context.Background())
	a := app.New(app.Config{Logger: logger, StopTimeout: 5 * time.Second}, tp.Components()...)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	return "http://" + tp.HTTPAddr(), func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("app.Run 返回错误: %v", err)
		}
	}
}

func post(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

func TestCreateUserOverHTTP(t *testing.T) {
	base, stop := run(t)
	defer stop()

	code, body := post(t, base+"/v1/users", `{"name":"颜","email":"YAN@Example.COM"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 got %d, want 200, body=%v", code, body)
	}

	user, _ := body["user"].(map[string]any)
	// 邮箱规范化是 biz 层的领域规则，这条顺带验证它穿过了整条链路。
	if got, want := user["email"], "yan@example.com"; got != want {
		t.Errorf("邮箱 got %v, want %v", got, want)
	}
	if user["id"] == nil || user["createdAt"] == nil {
		t.Errorf("响应缺少服务端生成的字段: %v", user)
	}
}

func TestProtoValidationRejectsBadEmail(t *testing.T) {
	// 校验规则只写在 .proto 的注解里，由 pkg/transport 的拦截器统一执行；
	// 这条用例证明「注解 → 拦截器 → gateway 状态码映射」整条链通了。
	base, stop := run(t)
	defer stop()

	code, body := post(t, base+"/v1/users", `{"name":"颜","email":"这不是邮箱"}`)

	if code != http.StatusBadRequest {
		t.Errorf("非法邮箱应返回 400, got %d, body=%v", code, body)
	}
}

func TestNotFoundMapsTo404(t *testing.T) {
	base, stop := run(t)
	defer stop()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/v1/users/999")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("查无此人应返回 404, got %d", resp.StatusCode)
	}
}
