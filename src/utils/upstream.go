package utils

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"hubproxy/config"
)

// isPlainHTTPHost 判断主机是否应使用明文 HTTP：回环地址、localhost 与内网地址。
// 行为与 go-containerregistry 的 name.Registry.Scheme 保持一致。
func isPlainHTTPHost(host string) bool {
	if strings.HasPrefix(host, "localhost:") || host == "localhost" {
		return true
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// DockerHubPool Docker Hub 故障切换池名称
const DockerHubPool = "docker.io"

// HostStatus 单个上游的健康状态快照
type HostStatus struct {
	Host        string    `json:"host"`
	Healthy     bool      `json:"healthy"`
	Failures    int       `json:"failures"`
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
	LastFailure time.Time `json:"lastFailure,omitempty"`
}

// PoolConfig 故障切换池配置：同一逻辑 Registry 的一组按优先级排序的上游主机
type PoolConfig struct {
	Name  string
	Hosts []string
}

// UpstreamManagerConfig 健康管理器配置
type UpstreamManagerConfig struct {
	Pools               []PoolConfig
	FailureThreshold    int
	Cooldown            time.Duration
	HealthCheckEnabled  bool
	HealthCheckInterval time.Duration
	ProbeTimeout        time.Duration
}

// UpstreamProber 探测上游是否存活
type UpstreamProber func(ctx context.Context, host string) error

type upstreamHostState struct {
	host        string
	failures    int
	downUntil   time.Time
	lastSuccess time.Time
	lastFailure time.Time
	probing     bool
}

type upstreamPoolState struct {
	hosts []string
}

// UpstreamManager 维护各上游的健康状态并决定故障切换顺序。
// 调用方通过 CandidateHosts 获取候选列表快照，后续健康状态变化
// （包括摘除、恢复）不会影响已经拿到快照的在途请求。
type UpstreamManager struct {
	mu       sync.RWMutex
	pools    map[string]*upstreamPoolState
	states   map[string]*upstreamHostState // key: pool + "\x00" + host
	cfg      UpstreamManagerConfig
	stopCh   chan struct{}
	stopOnce sync.Once
	prober   UpstreamProber
	now      func() time.Time
}

// NewUpstreamManager 创建健康管理器。enableProbe=false 时仅使用被动熔断（半开试探仍生效）。
func NewUpstreamManager(cfg UpstreamManagerConfig, prober UpstreamProber) *UpstreamManager {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	if cfg.HealthCheckInterval <= 0 {
		cfg.HealthCheckInterval = 10 * time.Second
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 5 * time.Second
	}

	m := &UpstreamManager{
		pools:  make(map[string]*upstreamPoolState),
		states: make(map[string]*upstreamHostState),
		cfg:    cfg,
		stopCh: make(chan struct{}),
		prober: prober,
		now:    time.Now,
	}
	if m.prober == nil {
		m.prober = m.defaultProbe
	}
	m.configure(cfg.Pools)

	if cfg.HealthCheckEnabled {
		go m.healthCheckLoop()
	}
	return m
}

// configure 重建池配置并保留仍在使用的主机的健康状态。
func (m *UpstreamManager) configure(pools []PoolConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := make(map[string]*upstreamPoolState, len(pools))
	for _, pool := range pools {
		if pool.Name == "" || len(pool.Hosts) == 0 {
			continue
		}
		next[pool.Name] = &upstreamPoolState{hosts: append([]string(nil), pool.Hosts...)}
		for _, host := range pool.Hosts {
			key := pool.Name + "\x00" + host
			if _, exists := m.states[key]; !exists {
				m.states[key] = &upstreamHostState{host: host}
			}
		}
	}
	m.pools = next
}

// Stop 停止后台健康检查，可重复调用。
func (m *UpstreamManager) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
}

// CandidateHosts 返回池中按优先级排序的候选上游快照：
//  1. 当前健康的主机（保持配置顺序，保证主上游恢复后优先切回）
//  2. 冷却已结束、可半开试探的主机
//  3. 仍在冷却中的主机（按恢复时间从近到远，全部不可用时的最后手段）
//
// 管理器中不存在该池时返回 fallback 本身。
func (m *UpstreamManager) CandidateHosts(pool string, fallback []string) []string {
	if m == nil {
		return append([]string(nil), fallback...)
	}

	now := m.now()
	var healthy, halfOpen, cooling []string
	var coolingUntil []time.Time

	m.mu.RLock()
	state, exists := m.pools[pool]
	if exists {
		for _, host := range state.hosts {
			hs := m.states[pool+"\x00"+host]
			switch {
			case hs == nil || hs.downUntil.IsZero() || !now.Before(hs.downUntil):
				if hs != nil && !hs.downUntil.IsZero() && !now.Before(hs.downUntil) {
					halfOpen = append(halfOpen, host)
				} else {
					healthy = append(healthy, host)
				}
			default:
				cooling = append(cooling, host)
				coolingUntil = append(coolingUntil, hs.downUntil)
			}
		}
	}
	m.mu.RUnlock()

	if !exists {
		return append([]string(nil), fallback...)
	}

	// 冷却中的主机按 downUntil 升序，最快恢复的排最前
	sort.SliceStable(cooling, func(i, j int) bool {
		return coolingUntil[i].Before(coolingUntil[j])
	})

	candidates := make([]string, 0, len(healthy)+len(halfOpen)+len(cooling))
	candidates = append(candidates, healthy...)
	candidates = append(candidates, halfOpen...)
	candidates = append(candidates, cooling...)

	// 兜底：池存在但状态异常为空时仍返回配置主机，避免请求直接失败
	if len(candidates) == 0 {
		candidates = append(candidates, fallback...)
	}
	return candidates
}

// RecordSuccess 记录一次成功调用，主机立即恢复为健康状态。
func (m *UpstreamManager) RecordSuccess(pool, host string) {
	if m == nil || host == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	hs := m.getOrCreateStateLocked(pool, host)
	hs.failures = 0
	hs.downUntil = time.Time{}
	hs.probing = false
	hs.lastSuccess = m.now()
}

// RecordFailure 记录一次可重试的失败调用，连续失败达到阈值后摘除主机并进入冷却。
func (m *UpstreamManager) RecordFailure(pool, host string) {
	if m == nil || host == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	hs := m.getOrCreateStateLocked(pool, host)
	hs.failures++
	hs.lastFailure = m.now()
	if hs.failures >= m.cfg.FailureThreshold {
		hs.downUntil = m.now().Add(m.cfg.Cooldown)
	}
}

func (m *UpstreamManager) getOrCreateStateLocked(pool, host string) *upstreamHostState {
	key := pool + "\x00" + host
	hs, exists := m.states[key]
	if !exists {
		hs = &upstreamHostState{host: host}
		m.states[key] = hs
	}
	return hs
}

// IsHealthy 返回主机当前是否健康。
func (m *UpstreamManager) IsHealthy(pool, host string) bool {
	if m == nil {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	hs, exists := m.states[pool+"\x00"+host]
	if !exists {
		return true
	}
	return hs.downUntil.IsZero() || !m.now().Before(hs.downUntil)
}

// PoolNames 返回所有故障切换池名称（排序，保证输出稳定）。
func (m *UpstreamManager) PoolNames() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.pools))
	for name := range m.pools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// PoolStatus 返回池中所有主机的健康状态快照。
func (m *UpstreamManager) PoolStatus(pool string) []HostStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	state, exists := m.pools[pool]
	if !exists {
		return nil
	}
	result := make([]HostStatus, 0, len(state.hosts))
	for _, host := range state.hosts {
		hs := m.states[pool+"\x00"+host]
		status := HostStatus{Host: host, Healthy: true}
		if hs != nil {
			status.Failures = hs.failures
			status.LastSuccess = hs.lastSuccess
			status.LastFailure = hs.lastFailure
			status.Healthy = hs.downUntil.IsZero() || !m.now().Before(hs.downUntil)
		}
		result = append(result, status)
	}
	return result
}

// healthCheckLoop 周期性主动探测处于冷却中的上游，恢复后立即标记健康，
// 使主上游恢复后无需等待用户请求试探即可切回。
func (m *UpstreamManager) healthCheckLoop() {
	ticker := time.NewTicker(m.cfg.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.probeOnce()
		}
	}
}

func (m *UpstreamManager) probeOnce() {
	type target struct{ pool, host string }
	var candidates []target

	m.mu.RLock()
	now := m.now()
	for poolName, pool := range m.pools {
		for _, host := range pool.hosts {
			hs := m.states[poolName+"\x00"+host]
			if hs == nil || hs.probing {
				continue
			}
			// 仅探测被摘除且冷却期已过（半开）的主机
			if hs.downUntil.IsZero() || now.Before(hs.downUntil) {
				continue
			}
			candidates = append(candidates, target{poolName, host})
		}
	}
	m.mu.RUnlock()

	// 加写锁确认并占位，避免与其他探测周期重复探测同一主机
	var targets []target
	m.mu.Lock()
	now = m.now()
	for _, t := range candidates {
		hs, exists := m.states[t.pool+"\x00"+t.host]
		if !exists || hs.probing || hs.downUntil.IsZero() || now.Before(hs.downUntil) {
			continue
		}
		hs.probing = true
		targets = append(targets, t)
	}
	m.mu.Unlock()

	for _, t := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.ProbeTimeout)
		err := m.prober(ctx, t.host)
		cancel()

		if err == nil {
			m.RecordSuccess(t.pool, t.host)
		} else {
			m.mu.Lock()
			if hs, exists := m.states[t.pool+"\x00"+t.host]; exists {
				hs.probing = false
				hs.failures = m.cfg.FailureThreshold
				hs.downUntil = m.now().Add(m.cfg.Cooldown)
			}
			m.mu.Unlock()
		}
	}
}

// defaultProbe 默认探测实现：请求 Registry 的 /v2/ 端点。
// 任何 HTTP 响应（含 401/403，Registry 对匿名访问通常返回 401）都说明上游存活，
// 只有连接失败、超时或 5xx 才判定为不可用。
func (m *UpstreamManager) defaultProbe(ctx context.Context, host string) error {
	scheme := "https"
	switch {
	case strings.HasPrefix(host, "http://"):
		scheme = "http"
		host = strings.TrimPrefix(host, "http://")
	case strings.HasPrefix(host, "https://"):
		host = strings.TrimPrefix(host, "https://")
	case isPlainHTTPHost(host):
		scheme = "http"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+host+"/v2/", nil)
	if err != nil {
		return err
	}

	transport := http.DefaultTransport
	if client := GetGlobalHTTPClient(); client != nil && client.Transport != nil {
		transport = client.Transport
	}
	client := &http.Client{Timeout: m.cfg.ProbeTimeout, Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("上游 %s 返回异常状态码: %d", host, resp.StatusCode)
	}
	return nil
}

// GlobalUpstreamManager 全局上游健康管理器
var GlobalUpstreamManager *UpstreamManager

// InitUpstreamManager 根据当前配置初始化全局上游健康管理器，并停止替换掉的旧实例。
func InitUpstreamManager() *UpstreamManager {
	cfg := config.GetConfig()

	pools := make([]PoolConfig, 0, len(cfg.Registries)+1)
	pools = append(pools, PoolConfig{
		Name:  DockerHubPool,
		Hosts: append([]string(nil), cfg.Upstream.DockerHubUpstreams...),
	})
	for domain, mapping := range cfg.Registries {
		if !mapping.Enabled {
			continue
		}
		hosts := mapping.UpstreamList()
		if len(hosts) > 0 {
			pools = append(pools, PoolConfig{Name: domain, Hosts: hosts})
		}
	}

	cooldown, err := time.ParseDuration(cfg.Upstream.Cooldown)
	if err != nil || cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	interval, err := time.ParseDuration(cfg.Upstream.HealthCheckInterval)
	if err != nil || interval <= 0 {
		interval = 10 * time.Second
	}
	probeTimeout, err := time.ParseDuration(cfg.Upstream.ProbeTimeout)
	if err != nil || probeTimeout <= 0 {
		probeTimeout = 5 * time.Second
	}

	if GlobalUpstreamManager != nil {
		GlobalUpstreamManager.Stop()
	}
	GlobalUpstreamManager = NewUpstreamManager(UpstreamManagerConfig{
		Pools:               pools,
		FailureThreshold:    cfg.Upstream.FailureThreshold,
		Cooldown:            cooldown,
		HealthCheckEnabled:  cfg.Upstream.HealthCheckEnabled,
		HealthCheckInterval: interval,
		ProbeTimeout:        probeTimeout,
	}, nil)
	return GlobalUpstreamManager
}
