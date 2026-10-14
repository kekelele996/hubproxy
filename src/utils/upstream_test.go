package utils

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hubproxy/config"
)

func newTestManager(t *testing.T, threshold int, cooldown time.Duration, hosts ...string) (*UpstreamManager, *time.Time) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := NewUpstreamManager(UpstreamManagerConfig{
		Pools:              []PoolConfig{{Name: "p", Hosts: hosts}},
		FailureThreshold:   threshold,
		Cooldown:           cooldown,
		HealthCheckEnabled: false,
	}, func(ctx context.Context, host string) error { return nil })
	m.now = func() time.Time { return now }
	return m, &now
}

func TestCandidateHostsUnknownPoolReturnsFallback(t *testing.T) {
	m, _ := newTestManager(t, 3, time.Second, "a")
	got := m.CandidateHosts("missing", []string{"x", "y"})
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("fallback = %v", got)
	}
}

func TestFailoverTripsAtThresholdAndRestores(t *testing.T) {
	m, now := newTestManager(t, 2, 30*time.Second, "primary", "secondary")

	if got := m.CandidateHosts("p", nil); len(got) != 2 || got[0] != "primary" {
		t.Fatalf("初始顺序 = %v", got)
	}

	m.RecordFailure("p", "primary")
	if !m.IsHealthy("p", "primary") {
		t.Fatalf("未达阈值不应摘除")
	}
	if got := m.CandidateHosts("p", nil); got[0] != "primary" {
		t.Fatalf("未达阈值不应改变顺序, got %v", got)
	}

	m.RecordFailure("p", "primary")
	if m.IsHealthy("p", "primary") {
		t.Fatalf("达到阈值后应摘除")
	}
	got := m.CandidateHosts("p", nil)
	if len(got) != 2 || got[0] != "secondary" || got[1] != "primary" {
		t.Fatalf("摘除后应切换到备用上游, got %v", got)
	}

	// 冷却结束进入半开，仍然排在健康主机之后
	*now = now.Add(31 * time.Second)
	got = m.CandidateHosts("p", nil)
	if len(got) != 2 || got[0] != "secondary" || got[1] != "primary" {
		t.Fatalf("半开状态应仍让位于健康主机, got %v", got)
	}

	// 半开试探成功：立即恢复，且因配置优先级最高而切回
	m.RecordSuccess("p", "primary")
	got = m.CandidateHosts("p", nil)
	if len(got) != 2 || got[0] != "primary" || got[1] != "secondary" {
		t.Fatalf("恢复后应按配置优先级切回主上游, got %v", got)
	}
}

func TestCandidateSnapshotUnaffectedByLaterSwitch(t *testing.T) {
	m, _ := newTestManager(t, 1, time.Minute, "primary", "secondary")

	inflight := m.CandidateHosts("p", nil)

	// 主上游随后被摘除，新的请求拿到新顺序，已在进行的请求快照不变
	m.RecordFailure("p", "primary")
	later := m.CandidateHosts("p", nil)

	if inflight[0] != "primary" || inflight[1] != "secondary" {
		t.Fatalf("在途请求的上游快照被切换操作影响: %v", inflight)
	}
	if later[0] != "secondary" {
		t.Fatalf("新请求应切换到备用上游, got %v", later)
	}
}

func TestAllUpstreamsDownStillReturnsCandidates(t *testing.T) {
	m, now := newTestManager(t, 1, 30*time.Second, "a", "b", "c")

	m.RecordFailure("p", "a")
	m.RecordFailure("p", "c")

	// a、c 冷却中：b 健康优先；冷却主机按恢复时间排序（同刻按配置稳定排序）
	got := m.CandidateHosts("p", nil)
	if len(got) != 3 || got[0] != "b" {
		t.Fatalf("got %v", got)
	}

	m.RecordFailure("p", "b")
	// 全部不可用时仍应返回全部候选（让请求有机会作为探针），而不是直接失败
	got = m.CandidateHosts("p", nil)
	if len(got) != 3 {
		t.Fatalf("全部不可用时也应返回候选列表, got %v", got)
	}

	*now = now.Add(31 * time.Second)
	got = m.CandidateHosts("p", nil)
	if len(got) != 3 {
		t.Fatalf("半开后应返回全部候选, got %v", got)
	}
}

func TestProbeOnceRecoversAndExtendsCooldown(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var failHosts map[string]bool
	var probed []string

	m := NewUpstreamManager(UpstreamManagerConfig{
		Pools:              []PoolConfig{{Name: "p", Hosts: []string{"primary", "secondary"}}},
		FailureThreshold:   1,
		Cooldown:           30 * time.Second,
		HealthCheckEnabled: false,
	}, func(ctx context.Context, host string) error {
		probed = append(probed, host)
		if failHosts[host] {
			return context.DeadlineExceeded
		}
		return nil
	})
	m.now = func() time.Time { return now }

	failHosts = map[string]bool{"primary": true}
	m.RecordFailure("p", "primary")
	m.RecordSuccess("p", "secondary")

	// 冷却未结束：不探测
	m.probeOnce()
	if len(probed) != 0 {
		t.Fatalf("冷却中不应探测, probed=%v", probed)
	}

	now = now.Add(31 * time.Second)
	m.probeOnce()
	if len(probed) != 1 || probed[0] != "primary" {
		t.Fatalf("应对半开的 primary 探测, probed=%v", probed)
	}
	// 探测仍失败：延长冷却，继续排在健康主机之后
	if m.IsHealthy("p", "primary") {
		t.Fatalf("探测失败不应恢复")
	}
	if got := m.CandidateHosts("p", nil); got[0] != "secondary" {
		t.Fatalf("探测失败后应继续使用备用上游, got %v", got)
	}

	// 再等一个冷却周期，探测恢复
	failHosts["primary"] = false
	now = now.Add(31 * time.Second)
	m.probeOnce()
	if !m.IsHealthy("p", "primary") {
		t.Fatalf("探测成功后应恢复健康")
	}
	if got := m.CandidateHosts("p", nil); got[0] != "primary" {
		t.Fatalf("恢复后主上游应排回首位, got %v", got)
	}
}

func TestHealthCheckLoopAutoRecovers(t *testing.T) {
	recovered := make(chan struct{}, 1)
	m := NewUpstreamManager(UpstreamManagerConfig{
		Pools:               []PoolConfig{{Name: "p", Hosts: []string{"primary"}}},
		FailureThreshold:    1,
		Cooldown:            20 * time.Millisecond,
		HealthCheckEnabled:  true,
		HealthCheckInterval: 10 * time.Millisecond,
		ProbeTimeout:        time.Second,
	}, func(ctx context.Context, host string) error {
		recovered <- struct{}{}
		return nil
	})
	defer m.Stop()

	m.RecordFailure("p", "primary")

	deadline := time.After(2 * time.Second)
	for !m.IsHealthy("p", "primary") {
		select {
		case <-deadline:
			t.Fatalf("主动探测未在预期时间内恢复上游")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestDefaultProbeInterpretsHTTPStatus(t *testing.T) {
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			t.Errorf("探测路径 = %q, want /v2/", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer alive.Close()

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	broken.Close()

	m, _ := newTestManager(t, 1, time.Second)

	// 401 表示 Registry 存活（匿名访问要求认证）
	host := alive.URL[len("http://"):]
	if err := m.defaultProbe(context.Background(), host); err != nil {
		t.Fatalf("401 响应应判定为存活: %v", err)
	}

	// 502 判定为不可用
	m502 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer m502.Close()
	if err := m.defaultProbe(context.Background(), m502.URL[len("http://"):]); err == nil {
		t.Fatalf("502 响应应判定为不可用")
	}

	// 连接失败判定为不可用
	dead := broken.URL[len("http://"):]
	if err := m.defaultProbe(context.Background(), dead); err == nil {
		t.Fatalf("连接拒绝应判定为不可用")
	}
}

func TestUpstreamManagerConcurrentAccess(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := NewUpstreamManager(UpstreamManagerConfig{
		Pools: []PoolConfig{
			{Name: "p1", Hosts: []string{"a", "b"}},
			{Name: "p2", Hosts: []string{"c"}},
		},
		FailureThreshold:    1,
		Cooldown:            20 * time.Millisecond,
		HealthCheckEnabled:  true,
		HealthCheckInterval: 5 * time.Millisecond,
		ProbeTimeout:        time.Second,
	}, func(ctx context.Context, host string) error {
		return context.DeadlineExceeded
	})
	defer m.Stop()
	m.now = func() time.Time { return now }

	done := make(chan struct{})
	for i := 0; i < 6; i++ {
		go func(id int) {
			for {
				select {
				case <-done:
					return
				default:
				}
				switch id % 4 {
				case 0:
					_ = m.CandidateHosts("p1", nil)
				case 1:
					m.RecordFailure("p1", "a")
				case 2:
					m.RecordSuccess("p1", "a")
				case 3:
					_ = m.PoolStatus("p2")
				}
			}
		}(i)
	}

	time.Sleep(150 * time.Millisecond)
	close(done)
}

func TestInitUpstreamManagerBuildsPoolsFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte(`
[upstream]
dockerHubUpstreams = ["mirror.example.com", "registry-1.docker.io"]
failureThreshold = 2
cooldown = "1s"
healthCheckEnabled = false

[registries."ghcr.io"]
upstreams = ["ghcr-mirror.example.com", "ghcr.io"]
authHost = "ghcr.io/token"
enabled = true

[registries."quay.io"]
upstream = "quay.io"
enabled = true
`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	if GlobalUpstreamManager != nil {
		GlobalUpstreamManager.Stop()
	}
	m := InitUpstreamManager()
	defer m.Stop()

	status := m.PoolStatus(DockerHubPool)
	if len(status) != 2 || status[0].Host != "mirror.example.com" {
		t.Fatalf("docker hub pool = %+v", status)
	}
	status = m.PoolStatus("ghcr.io")
	if len(status) != 2 || status[0].Host != "ghcr-mirror.example.com" || status[1].Host != "ghcr.io" {
		t.Fatalf("ghcr pool = %+v", status)
	}
	status = m.PoolStatus("quay.io")
	if len(status) != 1 || status[0].Host != "quay.io" {
		t.Fatalf("quay pool = %+v", status)
	}
}
