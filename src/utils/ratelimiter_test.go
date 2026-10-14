package utils

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
)

// loadLimiterTestConfig 将 body 写入临时配置并加载，返回配置文件路径
func loadLimiterTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRateLimiterHotReloadIPLists(t *testing.T) {
	path := loadLimiterTestConfig(t, `
[rateLimit]
requestLimit = 500
periodHours = 3.0

[security]
blackList = ["192.168.100.0/24"]
`)
	limiter := newIPRateLimiter(config.GetConfig())

	if _, allowed := limiter.GetLimiter("192.168.100.7:1234"); allowed {
		t.Fatal("黑名单IP初始应当被拒绝")
	}
	normalLimiter, allowed := limiter.GetLimiter("8.8.8.8:1234")
	if !allowed || normalLimiter == limiter.whitelistLimiter {
		t.Fatal("普通IP初始应当走普通限流器")
	}

	// 热更新：清空黑名单，加入白名单网段
	if err := os.WriteFile(path, []byte(`
[rateLimit]
requestLimit = 500
periodHours = 3.0

[security]
whiteList = ["10.0.0.0/8"]
blackList = []
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := config.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	limiter.applyConfig(config.GetConfig())

	if _, allowed := limiter.GetLimiter("192.168.100.7:1234"); !allowed {
		t.Fatal("黑名单移除后该IP应当放行")
	}
	whitelisted, allowed := limiter.GetLimiter("10.2.3.4:1234")
	if !allowed || whitelisted != limiter.whitelistLimiter {
		t.Fatal("白名单IP热更新后应当走不限流的共享限流器")
	}
	for i := 0; i < 10000; i++ {
		if !whitelisted.Allow() {
			t.Fatal("白名单限流器不应拦截请求")
		}
	}
}

func TestRateLimiterHotReloadQuota(t *testing.T) {
	loadLimiterTestConfig(t, `
[rateLimit]
requestLimit = 2
periodHours = 1.0
`)
	limiter := newIPRateLimiter(config.GetConfig())

	// 已存在的 IP 令牌桶（模拟一个正在访问的客户端）
	existing, allowed := limiter.GetLimiter("203.0.113.9:1234")
	if !allowed {
		t.Fatal("初始应允许访问")
	}
	if !existing.Allow() || !existing.Allow() {
		t.Fatal("突发容量为2，前两个请求应放行")
	}
	if existing.Allow() {
		t.Fatal("超出突发容量的请求应被拦截")
	}

	// 热更新额度：2/小时 -> 1000/小时，不重启、不换限流器
	path := os.Getenv("CONFIG_PATH")
	if err := os.WriteFile(path, []byte(`
[rateLimit]
requestLimit = 1000
periodHours = 1.0
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := config.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	limiter.applyConfig(config.GetConfig())

	// 在途请求持有的旧桶不被热更新修改，仍保持旧额度
	if existing.Burst() != 2 {
		t.Fatalf("在途请求的旧令牌桶被修改: burst=%d", existing.Burst())
	}

	// 同一客户端的后续请求立即拿到新额度的令牌桶
	refreshed, _ := limiter.GetLimiter("203.0.113.9:1234")
	if refreshed.Burst() != 1000 {
		t.Fatalf("新令牌桶额度未生效: burst=%d", refreshed.Burst())
	}
	if !refreshed.Allow() {
		t.Fatal("额度调大后应立即放行新请求")
	}
}

func TestRateLimiterInvalidQuotaKeepsCurrent(t *testing.T) {
	path := loadLimiterTestConfig(t, `
[rateLimit]
requestLimit = 50
periodHours = 1.0
`)
	limiter := newIPRateLimiter(config.GetConfig())
	originalR, originalB := limiter.r, limiter.b

	if err := os.WriteFile(path, []byte(`
[rateLimit]
requestLimit = 0
periodHours = 0
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := config.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	limiter.applyConfig(config.GetConfig())

	limiter.cfgMu.RLock()
	r, b := limiter.r, limiter.b
	limiter.cfgMu.RUnlock()
	if r != originalR || b != originalB {
		t.Fatalf("非法限流参数不应生效: r=%v b=%d, 原始 r=%v b=%d", r, b, originalR, originalB)
	}
}

func TestExtractIPFromAddress(t *testing.T) {
	if got := extractIPFromAddress("127.0.0.1:5000"); got != "127.0.0.1" {
		t.Fatalf("extract IPv4 = %q", got)
	}
	if got := extractIPFromAddress("[2001:db8::1]:5000"); got != "2001:db8::1" {
		t.Fatalf("extract IPv6 = %q", got)
	}
}

func TestNormalizeIPv6ForRateLimit(t *testing.T) {
	if got := normalizeIPForRateLimit("192.168.1.2"); got != "192.168.1.2" {
		t.Fatalf("IPv4 normalized = %q", got)
	}
	if got := normalizeIPForRateLimit("2001:db8::1"); got != "2001:db8::/64" {
		t.Fatalf("IPv6 normalized = %q", got)
	}
}

func TestClientIPIgnoresSpoofedXFFWithoutTrustedProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	ConfigureTrustedProxies(router)

	var got string
	router.GET("/", func(c *gin.Context) {
		got = c.ClientIP()
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.50:12345"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if got != "203.0.113.50" {
		t.Fatalf("ClientIP() = %q, want 203.0.113.50", got)
	}
}

func TestClientIPTrustsXFFFromTrustedProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	if err := router.SetTrustedProxies([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}

	var got string
	router.GET("/", func(c *gin.Context) {
		got = c.ClientIP()
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if got != "203.0.113.50" {
		t.Fatalf("ClientIP() = %q, want 203.0.113.50", got)
	}
}
