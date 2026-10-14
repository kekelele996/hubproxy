package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigUsesConfigPathAndEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.toml")
	data := []byte(`
[server]
host = "127.0.0.1"
port = 5999

[access]
proxy = "socks5://127.0.0.1:1080"
`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", path)
	t.Setenv("SERVER_PORT", "6001")
	t.Setenv("ACCESS_PROXY", "")

	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}

	cfg := GetConfig()
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("Server.Host = %q", cfg.Server.Host)
	}
	if cfg.Server.Port != 6001 {
		t.Fatalf("Server.Port = %d, want 6001", cfg.Server.Port)
	}
	if cfg.Access.Proxy != "" {
		t.Fatalf("Access.Proxy = %q, want empty override", cfg.Access.Proxy)
	}
}

func TestRegistryMappingUpstreamList(t *testing.T) {
	// 旧的单个 upstream 配置仍然生效
	m := RegistryMapping{Upstream: "ghcr.io", Enabled: true}
	if got := m.UpstreamList(); len(got) != 1 || got[0] != "ghcr.io" {
		t.Fatalf("single upstream fallback = %v", got)
	}

	// upstreams 优先，单个 upstream 追加在后面并去重
	m = RegistryMapping{
		Upstream:  "ghcr.io",
		Upstreams: []string{"https://mirror.example/", "ghcr.io", " "},
	}
	got := m.UpstreamList()
	want := []string{"mirror.example", "ghcr.io"}
	if len(got) != len(want) {
		t.Fatalf("UpstreamList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("UpstreamList = %v, want %v", got, want)
		}
	}
}

func TestNormalizeUpstreamHosts(t *testing.T) {
	got := NormalizeUpstreamHosts([]string{" https://a.example/ ", "http://b.example", "a.example", "", "  "})
	want := []string{"a.example", "b.example"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestUpstreamDefaultsAndOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte(`
[upstream]
dockerHubUpstreams = ["docker.m.example.com", "registry-1.docker.io"]
cooldown = "15s"
failureThreshold = 2
`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	t.Setenv("DOCKER_HUB_UPSTREAMS", "mirror-a.example.com,https://mirror-b.example.com/,mirror-a.example.com")
	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}

	cfg := GetConfig()
	wantHosts := []string{"mirror-a.example.com", "mirror-b.example.com"}
	if len(cfg.Upstream.DockerHubUpstreams) != len(wantHosts) {
		t.Fatalf("dockerHubUpstreams = %v, want %v", cfg.Upstream.DockerHubUpstreams, wantHosts)
	}
	for i := range wantHosts {
		if cfg.Upstream.DockerHubUpstreams[i] != wantHosts[i] {
			t.Fatalf("dockerHubUpstreams = %v, want %v", cfg.Upstream.DockerHubUpstreams, wantHosts)
		}
	}
	if cfg.Upstream.FailureThreshold != 2 {
		t.Fatalf("failureThreshold = %d, want 2", cfg.Upstream.FailureThreshold)
	}
	if cfg.Upstream.Cooldown != "15s" {
		t.Fatalf("cooldown = %q, want 15s", cfg.Upstream.Cooldown)
	}
	if !cfg.Upstream.HealthCheckEnabled {
		t.Fatalf("healthCheckEnabled 默认应为 true")
	}
}

func TestUpstreamDefaultsFilledWhenSectionMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[server]\nport = 5001\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}

	cfg := GetConfig()
	if len(cfg.Upstream.DockerHubUpstreams) != 1 || cfg.Upstream.DockerHubUpstreams[0] != "registry-1.docker.io" {
		t.Fatalf("默认 Docker Hub 上游 = %v", cfg.Upstream.DockerHubUpstreams)
	}
	if cfg.Upstream.FailureThreshold != 3 {
		t.Fatalf("默认 failureThreshold = %d", cfg.Upstream.FailureThreshold)
	}
	if cfg.Upstream.Cooldown != "30s" || cfg.Upstream.HealthCheckInterval != "10s" || cfg.Upstream.ProbeTimeout != "5s" {
		t.Fatalf("默认时间参数异常: %+v", cfg.Upstream)
	}
}
