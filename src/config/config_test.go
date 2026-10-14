package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func writeTempConfig(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReloadConfigPicksUpFileChanges(t *testing.T) {
	path := writeTempConfig(t, "config.toml", `
[rateLimit]
requestLimit = 100
periodHours = 1.0

[security]
blackList = ["10.0.0.1"]

[access]
proxy = "socks5://127.0.0.1:1080"
`)
	t.Setenv("CONFIG_PATH", path)
	t.Setenv("RATE_LIMIT", "120")

	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg := GetConfig(); cfg.RateLimit.RequestLimit != 120 || cfg.Access.Proxy == "" {
		t.Fatalf("初始配置不符合预期: requestLimit=%d proxy=%q", cfg.RateLimit.RequestLimit, cfg.Access.Proxy)
	}

	var hookCount int
	var lastHookLimit int
	OnReload(func(cfg *AppConfig) {
		hookCount++
		lastHookLimit = cfg.RateLimit.RequestLimit
	})

	// 改配置：黑名单、代理、文件中的限流额度；环境变量 RATE_LIMIT 仍优先生效
	if err := os.WriteFile(path, []byte(`
[rateLimit]
requestLimit = 300
periodHours = 2.0

[security]
blackList = ["10.0.0.2", "10.0.0.3"]

[access]
proxy = ""
`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ReloadConfig(); err != nil {
		t.Fatal(err)
	}

	cfg := GetConfig()
	if cfg.Access.Proxy != "" {
		t.Fatalf("代理未热更新: %q", cfg.Access.Proxy)
	}
	if len(cfg.Security.BlackList) != 2 || cfg.Security.BlackList[0] != "10.0.0.2" {
		t.Fatalf("黑名单未热更新: %#v", cfg.Security.BlackList)
	}
	if cfg.RateLimit.RequestLimit != 120 { // 环境变量覆盖优先
		t.Fatalf("环境变量覆盖失效: requestLimit=%d", cfg.RateLimit.RequestLimit)
	}
	if cfg.RateLimit.PeriodHours != 2.0 {
		t.Fatalf("限流周期未热更新: %v", cfg.RateLimit.PeriodHours)
	}
	if hookCount != 1 || lastHookLimit != 120 {
		t.Fatalf("重载回调状态异常: count=%d limit=%d", hookCount, lastHookLimit)
	}
}

func TestReloadInvalidConfigKeepsPrevious(t *testing.T) {
	path := writeTempConfig(t, "config.toml", `
[rateLimit]
requestLimit = 321
periodHours = 1.0
`)
	t.Setenv("CONFIG_PATH", path)

	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}

	hookCalled := false
	OnReload(func(*AppConfig) { hookCalled = true })

	if err := os.WriteFile(path, []byte(`[rateLimit`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ReloadConfig(); err == nil {
		t.Fatal("非法 TOML 应当返回错误")
	}

	cfg := GetConfig()
	if cfg.RateLimit.RequestLimit != 321 {
		t.Fatalf("非法配置污染了运行中的配置: requestLimit=%d", cfg.RateLimit.RequestLimit)
	}
	if hookCalled {
		t.Fatal("解析失败时不应触发重载回调")
	}
}

func TestConfigSnapshotStaysStableAcrossReload(t *testing.T) {
	path := writeTempConfig(t, "config.toml", `
[security]
whiteList = ["10.1.1.1"]
blackList = ["10.2.2.2"]

[access]
whiteList = ["allowed/repo"]
blackList = ["blocked/repo"]
`)
	t.Setenv("CONFIG_PATH", path)
	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}

	// 模拟一个已经拿到配置、正在处理中的请求
	inFlight := GetConfig()
	if inFlight.Security.WhiteList[0] != "10.1.1.1" {
		t.Fatalf("初始白名单异常: %#v", inFlight.Security.WhiteList)
	}

	// 热加载成新配置
	if err := os.WriteFile(path, []byte(`
[security]
whiteList = ["10.9.9.9"]
blackList = []

[access]
whiteList = []
blackList = []

[registries."custom.example"]
upstream = "custom.example"
enabled = false
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ReloadConfig(); err != nil {
		t.Fatal(err)
	}

	// 进行中的请求仍持有旧配置，不受热加载影响
	if inFlight.Security.WhiteList[0] != "10.1.1.1" ||
		len(inFlight.Security.BlackList) != 1 ||
		inFlight.Access.WhiteList[0] != "allowed/repo" {
		t.Fatalf("在途请求持有的配置快照被热加载改变: %#v", inFlight)
	}
	if _, existed := inFlight.Registries["custom.example"]; existed {
		t.Fatal("旧配置快照中不应出现新加载的 registry")
	}

	// 新请求看到的是新配置
	fresh := GetConfig()
	if fresh.Security.WhiteList[0] != "10.9.9.9" {
		t.Fatalf("新请求未拿到新配置: %#v", fresh.Security.WhiteList)
	}
	if _, exists := fresh.Registries["custom.example"]; !exists {
		t.Fatal("新配置中应包含新加载的 registry")
	}
}

func TestReloadWatcherPicksUpFileChange(t *testing.T) {
	path := writeTempConfig(t, "config.toml", `
[rateLimit]
requestLimit = 100
periodHours = 1.0
`)
	t.Setenv("CONFIG_PATH", path)
	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartReloadWatcher(ctx, 20*time.Millisecond)

	if err := os.WriteFile(path, []byte(`
[rateLimit]
requestLimit = 777
periodHours = 1.0
`), 0644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if GetConfig().RateLimit.RequestLimit == 777 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("配置文件变更后未在预期时间内热加载")
}
