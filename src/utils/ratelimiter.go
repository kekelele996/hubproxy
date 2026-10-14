package utils

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
	"hubproxy/config"
)

const (
	CleanupInterval = 20 * time.Minute
	MaxIPCacheSize  = 10000
)

// 可信反代
var trustedProxyCIDRs = []string{
	"127.0.0.0/8",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// ConfigureTrustedProxies 可信反代
func ConfigureTrustedProxies(router *gin.Engine) {
	_ = router.SetTrustedProxies(trustedProxyCIDRs)
}

// IPRateLimiter IP限流器结构体
type IPRateLimiter struct {
	ips   map[string]*rateLimiterEntry
	mu    *sync.RWMutex
	cfgMu sync.RWMutex // 保护 r、b、whitelist、blacklist 的热更新

	r                rate.Limit
	b                int
	whitelist        []*net.IPNet
	blacklist        []*net.IPNet
	whitelistLimiter *rate.Limiter // 全局共享的白名单限流器
}

// rateLimiterEntry 限流器条目
type rateLimiterEntry struct {
	limiter    *rate.Limiter
	lastAccess time.Time
}

// parseIPCIDRList 将配置中的 IP/CIDR 字符串列表解析为 CIDR 列表
func parseIPCIDRList(items []string, listName string) []*net.IPNet {
	cidrs := make([]*net.IPNet, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			if !strings.Contains(item, "/") {
				item = item + "/32"
			}
			_, ipnet, err := net.ParseCIDR(item)
			if err == nil {
				cidrs = append(cidrs, ipnet)
			} else {
				fmt.Printf("警告: 无效的%sIP格式: %s\n", listName, item)
			}
		}
	}
	return cidrs
}

// rateParams 计算令牌桶速率与突发容量，配置非法时 ok 返回 false
func rateParams(cfg *config.AppConfig) (r rate.Limit, b int, ok bool) {
	if cfg.RateLimit.RequestLimit <= 0 || cfg.RateLimit.PeriodHours <= 0 {
		return 0, 0, false
	}
	return rate.Limit(float64(cfg.RateLimit.RequestLimit) / (cfg.RateLimit.PeriodHours * 3600)),
		cfg.RateLimit.RequestLimit, true
}

// newIPRateLimiter 根据配置创建限流器
func newIPRateLimiter(cfg *config.AppConfig) *IPRateLimiter {
	r, b, _ := rateParams(cfg)

	limiter := &IPRateLimiter{
		ips:              make(map[string]*rateLimiterEntry),
		mu:               &sync.RWMutex{},
		r:                r,
		b:                b,
		whitelist:        parseIPCIDRList(cfg.Security.WhiteList, "白名单"),
		blacklist:        parseIPCIDRList(cfg.Security.BlackList, "黑名单"),
		whitelistLimiter: rate.NewLimiter(rate.Inf, b),
	}

	go limiter.cleanupRoutine()

	return limiter
}

// InitGlobalLimiter 初始化全局限流器，并在配置热加载时平滑更新黑白名单与限流额度
func InitGlobalLimiter() *IPRateLimiter {
	limiter := newIPRateLimiter(config.GetConfig())

	config.OnReload(func(cfg *config.AppConfig) {
		limiter.applyConfig(cfg)
	})

	return limiter
}

// applyConfig 在不重启的前提下应用最新配置。
// 名单整体原子替换；限流额度变化时按新参数重建令牌桶，
// 已经通过校验、正在处理中的请求持有旧桶引用，不受影响。
func (i *IPRateLimiter) applyConfig(cfg *config.AppConfig) {
	whitelist := parseIPCIDRList(cfg.Security.WhiteList, "白名单")
	blacklist := parseIPCIDRList(cfg.Security.BlackList, "黑名单")

	newR, newB, valid := rateParams(cfg)

	i.cfgMu.Lock()
	i.whitelist = whitelist
	i.blacklist = blacklist
	oldR, oldB := i.r, i.b
	if valid {
		i.r, i.b = newR, newB
	} else {
		newR, newB = oldR, oldB
		fmt.Printf("警告: 限流配置无效(requestLimit=%d, periodHours=%g)，保持当前限流参数\n",
			cfg.RateLimit.RequestLimit, cfg.RateLimit.PeriodHours)
	}
	i.cfgMu.Unlock()

	if oldR == newR && oldB == newB {
		return
	}

	// 额度变化时按新参数重建各 IP 的令牌桶：收紧立即生效、放宽立即获得新额度。
	// 已通过限流、正在处理中的请求持有旧桶引用，不受影响。
	i.mu.Lock()
	for ip, entry := range i.ips {
		i.ips[ip] = &rateLimiterEntry{
			limiter:    rate.NewLimiter(newR, newB),
			lastAccess: entry.lastAccess,
		}
	}
	i.mu.Unlock()
}

// cleanupRoutine 定期清理过期的限流器
func (i *IPRateLimiter) cleanupRoutine() {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		expired := make([]string, 0)

		i.mu.RLock()
		for ip, entry := range i.ips {
			if now.Sub(entry.lastAccess) > 2*time.Hour {
				expired = append(expired, ip)
			}
		}
		i.mu.RUnlock()

		if len(expired) > 0 || len(i.ips) > MaxIPCacheSize {
			i.mu.Lock()
			for _, ip := range expired {
				delete(i.ips, ip)
			}

			if len(i.ips) > MaxIPCacheSize {
				i.ips = make(map[string]*rateLimiterEntry)
			}
			i.mu.Unlock()
		}
	}
}

// extractIPFromAddress 从地址中提取纯IP
func extractIPFromAddress(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

// normalizeIPForRateLimit 标准化IP地址用于限流
func normalizeIPForRateLimit(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ipStr
	}

	if ip.To4() != nil {
		return ipStr
	}

	ipv6 := ip.To16()
	for i := 8; i < 16; i++ {
		ipv6[i] = 0
	}
	return ipv6.String() + "/64"
}

// isIPInCIDRList 检查IP是否在CIDR列表中
func isIPInCIDRList(ip string, cidrList []*net.IPNet) bool {
	cleanIP := extractIPFromAddress(ip)
	parsedIP := net.ParseIP(cleanIP)
	if parsedIP == nil {
		return false
	}

	for _, cidr := range cidrList {
		if cidr.Contains(parsedIP) {
			return true
		}
	}
	return false
}

// GetLimiter 获取指定IP的限流器
func (i *IPRateLimiter) GetLimiter(ip string) (*rate.Limiter, bool) {
	cleanIP := extractIPFromAddress(ip)

	// 取一份名单与限流参数快照，配置热更新发生在本函数之后也不影响本次判定
	i.cfgMu.RLock()
	blacklisted := isIPInCIDRList(cleanIP, i.blacklist)
	whitelisted := isIPInCIDRList(cleanIP, i.whitelist)
	whitelistLimiter := i.whitelistLimiter
	r, b := i.r, i.b
	i.cfgMu.RUnlock()

	if blacklisted {
		return nil, false
	}

	if whitelisted {
		return whitelistLimiter, true
	}

	normalizedIP := normalizeIPForRateLimit(cleanIP)

	now := time.Now()

	var entry *rateLimiterEntry
	i.mu.RLock()
	_, exists := i.ips[normalizedIP]
	i.mu.RUnlock()

	if exists {
		i.mu.Lock()
		if entry, stillExists := i.ips[normalizedIP]; stillExists {
			entry.lastAccess = now
			i.mu.Unlock()
			return entry.limiter, true
		}
		i.mu.Unlock()
	}

	i.mu.Lock()
	if entry, exists := i.ips[normalizedIP]; exists {
		entry.lastAccess = now
		i.mu.Unlock()
		return entry.limiter, true
	}

	entry = &rateLimiterEntry{
		limiter:    rate.NewLimiter(r, b),
		lastAccess: now,
	}
	i.ips[normalizedIP] = entry
	i.mu.Unlock()

	return entry.limiter, true
}

// RateLimitMiddleware 速率限制中间件
func RateLimitMiddleware(limiter *IPRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/" || path == "/images" || path == "/search" ||
			path == "/favicon.ico" ||
			strings.HasPrefix(path, "/assets/") {
			c.Next()
			return
		}

		cleanIP := extractIPFromAddress(c.ClientIP())

		ipLimiter, allowed := limiter.GetLimiter(cleanIP)

		if !allowed {
			c.JSON(403, gin.H{
				"error": "您已被限制访问",
			})
			c.Abort()
			return
		}

		if !ipLimiter.Allow() {
			c.JSON(429, gin.H{
				"error": "请求频率过快，暂时限制访问",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}
