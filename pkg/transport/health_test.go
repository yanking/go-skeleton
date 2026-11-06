package transport

// 探针与摘流的回归网。这组行为的特点是「平时完全看不出」：探针恒绿、停机照常，
// 只有在滚动更新或依赖挂掉时才显出代价，所以每一条都要钉住。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/health/grpc_health_v1"
)

// probeStatus 打一个探针并返回状态码与响应体，不借用 get()——摘流窗口里的请求
// 允许失败，get() 会直接 Fatal。
func probeStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestDrainBeforeClose 钉住摘流顺序：就绪探针先转红、监听后关。
//
// 顺序反了就是「收到 TERM 的瞬间拒连」——LB 与 kube-proxy 摘流要数百毫秒到数秒，
// 而 Shutdown 的第一件事就是关 listener，那段窗口的新连接拿到 TCP 层的
// connection refused（不是 503），客户端重试策略往往不覆盖，于是每次发布都丢
// 一批请求，还表现为客户端侧的连接错误、很难归因到发布。
func TestDrainBeforeClose(t *testing.T) {
	srv := NewRESTServer(context.Background(),
		Config{HTTPAddr: "127.0.0.1:0", DrainDelay: 400 * time.Millisecond},
		WithDirectGateway(guardGateway))
	served := start(t, srv)
	base := "http://" + srv.httpLn.Addr().String()

	// 服务中：两个探针都绿。
	if code, body := probeStatus(t, base+"/readyz"); code != http.StatusOK {
		t.Fatalf("服务中 /readyz got %d %q, want 200", code, body)
	}
	if code, _ := probeStatus(t, base+"/healthz"); code != http.StatusOK {
		t.Fatalf("服务中 /healthz got %d, want 200", code)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()

	// 进入摘流窗口：就绪已转红，但端口还在接受连接——这正是摘流的意义。
	time.Sleep(150 * time.Millisecond)
	code, body := probeStatus(t, base+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("摘流窗口内 /readyz got %d %q, want 503", code, body)
	}
	if !strings.Contains(body, "停机中") {
		t.Errorf("/readyz 应说明原因, got %q", body)
	}
	// liveness 仍绿：它转红会让编排系统强杀进程，而停机中的实例正需要时间排空。
	if code, _ := probeStatus(t, base+"/healthz"); code != http.StatusOK {
		t.Errorf("摘流窗口内 /healthz got %d, want 200（liveness 不该随停机转红）", code)
	}

	if err := <-stopped; err != nil {
		t.Errorf("Stop got %v, want nil", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Start 收尾异常, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Stop 之后 Start 未在 5s 内返回")
	}
}

// TestGRPCHealthFlipsOnDrain 钉住 gRPC 探针也参与摘流。原先 health.NewServer()
// 的句柄就地丢弃，于是实例「永远健康」——连停机中都报 SERVING，gRPC 客户端侧的
// 健康检查负载均衡器完全看不到实例正在退出。
func TestGRPCHealthFlipsOnDrain(t *testing.T) {
	srv := NewRPCServer(Config{GRPCAddr: "127.0.0.1:0", DrainDelay: 400 * time.Millisecond},
		WithService(registerEcho))
	served := start(t, srv)
	conn := dial(t, srv)

	if got := checkHealth(t, conn); got != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("服务中 health got %v, want SERVING", got)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()

	time.Sleep(150 * time.Millisecond)
	if got := checkHealth(t, conn); got != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Errorf("摘流窗口内 health got %v, want NOT_SERVING", got)
	}

	if err := <-stopped; err != nil {
		t.Errorf("Stop got %v, want nil", err)
	}
	<-served
}

// TestReadinessChecks 钉住依赖就绪检查。这是「装配期不 ping 库」那个取舍的
// 另一半：不 ping 是因为 DB 比进程晚就绪是常态，但连不上库的实例不该接流量——
// 没有这一环，那样的实例照样通过就绪探针，编排系统继续发流量，每个请求都 500。
func TestReadinessChecks(t *testing.T) {
	var broken atomic.Bool
	srv := NewRESTServer(context.Background(),
		Config{HTTPAddr: "127.0.0.1:0", DrainDelay: -1},
		WithDirectGateway(guardGateway),
		WithReadinessCheck(func(context.Context) error {
			if broken.Load() {
				return errors.New("数据库不可达")
			}
			return nil
		}))
	served := start(t, srv)
	base := "http://" + srv.httpLn.Addr().String()

	if code, body := probeStatus(t, base+"/readyz"); code != http.StatusOK {
		t.Errorf("检查通过时 /readyz got %d %q, want 200", code, body)
	}

	broken.Store(true)
	code, body := probeStatus(t, base+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("检查失败时 /readyz got %d, want 503", code)
	}
	if !strings.Contains(body, "数据库不可达") {
		t.Errorf("/readyz 应带上检查给出的原因, got %q", body)
	}
	// liveness 不查依赖：重启进程治不好一个挂掉的数据库。
	if code, _ := probeStatus(t, base+"/healthz"); code != http.StatusOK {
		t.Errorf("依赖不可用时 /healthz got %d, want 200", code)
	}

	stop(t, srv, served)
}

// TestReflectionOffByDefault 钉住 reflection 默认关。它恒定免鉴权，开着就等于把
// 完整的服务与字段结构对任何能连到 gRPC 端口的人公开（同集群 pod、SSRF、端口
// 误暴露都算），而那正是构造攻击载荷的第一步。
func TestReflectionOffByDefault(t *testing.T) {
	const reflectionSvc = "grpc.reflection.v1.ServerReflection"

	t.Run("默认不注册", func(t *testing.T) {
		srv := NewRPCServer(Config{GRPCAddr: "127.0.0.1:0", DrainDelay: -1}, WithService(registerEcho))
		if _, ok := srv.grpc.GetServiceInfo()[reflectionSvc]; ok {
			t.Error("默认不应注册 reflection")
		}
	})

	t.Run("显式开启才注册", func(t *testing.T) {
		srv := NewRPCServer(Config{GRPCAddr: "127.0.0.1:0", DrainDelay: -1, Reflection: true},
			WithService(registerEcho))
		if _, ok := srv.grpc.GetServiceInfo()[reflectionSvc]; !ok {
			t.Error("配了 reflection: true 应注册")
		}
	})
}

// TestDrainDelayDefaults 零值取缺省、负值即不等。
func TestDrainDelayDefaults(t *testing.T) {
	if got := (Config{}).drainDelay(); got != defaultDrainDelay {
		t.Errorf("零值 got %v, want %v", got, defaultDrainDelay)
	}
	if got := (Config{DrainDelay: -1}).drainDelay(); got != 0 {
		t.Errorf("负值应表示不等, got %v", got)
	}
	if got := (Config{DrainDelay: time.Second}).drainDelay(); got != time.Second {
		t.Errorf("显式值 got %v, want 1s", got)
	}
}
