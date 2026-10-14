package config

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// RegistryMapping Registry映射配置
type RegistryMapping struct {
	Upstream string `toml:"upstream"`
	AuthHost string `toml:"authHost"`
	AuthType string `toml:"authType"`
	Enabled  bool   `toml:"enabled"`
}

// AppConfig 应用配置结构体
type AppConfig struct {
	Server struct {
		Host           string `toml:"host"`
		Port           int    `toml:"port"`
		FileSize       int64  `toml:"fileSize"`
		EnableH2C      bool   `toml:"enableH2C"`
		EnableFrontend bool   `toml:"enableFrontend"`
	} `toml:"server"`

	RateLimit struct {
		RequestLimit int     `toml:"requestLimit"`
		PeriodHours  float64 `toml:"periodHours"`
	} `toml:"rateLimit"`

	Security struct {
		WhiteList []string `toml:"whiteList"`
		BlackList []string `toml:"blackList"`
	} `toml:"security"`

	Access struct {
		WhiteList []string `toml:"whiteList"`
		BlackList []string `toml:"blackList"`
		Proxy     string   `toml:"proxy"`
	} `toml:"access"`

	Download struct {
		MaxImages int `toml:"maxImages"`
	} `toml:"download"`

	Registries map[string]RegistryMapping `toml:"registries"`

	TokenCache struct {
		Enabled    bool   `toml:"enabled"`
		DefaultTTL string `toml:"defaultTTL"`
	} `toml:"tokenCache"`
}

var (
	appConfig     *AppConfig
	appConfigLock sync.RWMutex

	cachedConfig     *AppConfig
	configCacheTime  time.Time
	configCacheTTL   = 5 * time.Second
	configCacheMutex sync.RWMutex

	// reloadHooks 配置成功热加载后触发的回调，各运行时组件在此平滑应用新配置
	reloadHooksMu sync.RWMutex
	reloadHooks   []func(*AppConfig)

	// watcher 相关状态：lastWatchedBytes 记录上次已感知到的配置文件内容
	watcherMu        sync.Mutex
	lastWatchedBytes []byte
	watcherStarted   bool
)

// DefaultConfigWatchInterval 配置文件变更轮询间隔
const DefaultConfigWatchInterval = 3 * time.Second

// DefaultConfig 返回默认配置
func DefaultConfig() *AppConfig {
	return &AppConfig{
		Server: struct {
			Host           string `toml:"host"`
			Port           int    `toml:"port"`
			FileSize       int64  `toml:"fileSize"`
			EnableH2C      bool   `toml:"enableH2C"`
			EnableFrontend bool   `toml:"enableFrontend"`
		}{
			Host:           "0.0.0.0",
			Port:           5000,
			FileSize:       2 * 1024 * 1024 * 1024,
			EnableH2C:      false,
			EnableFrontend: true,
		},
		RateLimit: struct {
			RequestLimit int     `toml:"requestLimit"`
			PeriodHours  float64 `toml:"periodHours"`
		}{
			RequestLimit: 500,
			PeriodHours:  3.0,
		},
		Security: struct {
			WhiteList []string `toml:"whiteList"`
			BlackList []string `toml:"blackList"`
		}{
			WhiteList: []string{},
			BlackList: []string{},
		},
		Access: struct {
			WhiteList []string `toml:"whiteList"`
			BlackList []string `toml:"blackList"`
			Proxy     string   `toml:"proxy"`
		}{
			WhiteList: []string{},
			BlackList: []string{},
			Proxy:     "",
		},
		Download: struct {
			MaxImages int `toml:"maxImages"`
		}{
			MaxImages: 10,
		},
		Registries: map[string]RegistryMapping{
			"ghcr.io": {
				Upstream: "ghcr.io",
				AuthHost: "ghcr.io/token",
				AuthType: "github",
				Enabled:  true,
			},
			"gcr.io": {
				Upstream: "gcr.io",
				AuthHost: "gcr.io/v2/token",
				AuthType: "google",
				Enabled:  true,
			},
			"quay.io": {
				Upstream: "quay.io",
				AuthHost: "quay.io/v2/auth",
				AuthType: "quay",
				Enabled:  true,
			},
			"registry.k8s.io": {
				Upstream: "registry.k8s.io",
				AuthHost: "registry.k8s.io",
				AuthType: "anonymous",
				Enabled:  true,
			},
		},
		TokenCache: struct {
			Enabled    bool   `toml:"enabled"`
			DefaultTTL string `toml:"defaultTTL"`
		}{
			Enabled:    true,
			DefaultTTL: "20m",
		},
	}
}

// GetConfig 安全地获取配置副本
func GetConfig() *AppConfig {
	configCacheMutex.RLock()
	if cachedConfig != nil && time.Since(configCacheTime) < configCacheTTL {
		config := cachedConfig
		configCacheMutex.RUnlock()
		return config
	}
	configCacheMutex.RUnlock()

	configCacheMutex.Lock()
	defer configCacheMutex.Unlock()

	if cachedConfig != nil && time.Since(configCacheTime) < configCacheTTL {
		return cachedConfig
	}

	appConfigLock.RLock()
	if appConfig == nil {
		appConfigLock.RUnlock()
		defaultCfg := DefaultConfig()
		cachedConfig = defaultCfg
		configCacheTime = time.Now()
		return defaultCfg
	}

	configCopy := *appConfig
	configCopy.Security.WhiteList = append([]string(nil), appConfig.Security.WhiteList...)
	configCopy.Security.BlackList = append([]string(nil), appConfig.Security.BlackList...)
	configCopy.Access.WhiteList = append([]string(nil), appConfig.Access.WhiteList...)
	configCopy.Access.BlackList = append([]string(nil), appConfig.Access.BlackList...)
	if appConfig.Registries != nil {
		configCopy.Registries = make(map[string]RegistryMapping, len(appConfig.Registries))
		for domain, mapping := range appConfig.Registries {
			configCopy.Registries[domain] = mapping
		}
	}
	appConfigLock.RUnlock()

	cachedConfig = &configCopy
	configCacheTime = time.Now()

	return cachedConfig
}

// setConfig 安全地设置配置
func setConfig(cfg *AppConfig) {
	appConfigLock.Lock()
	defer appConfigLock.Unlock()
	appConfig = cfg

	configCacheMutex.Lock()
	cachedConfig = nil
	configCacheMutex.Unlock()
}

func configFilePath() string {
	if path := strings.TrimSpace(os.Getenv("CONFIG_PATH")); path != "" {
		return path
	}
	return "config.toml"
}

func LoadConfig() error {
	cfg, fileData, err := readConfigFromFile()
	if err != nil {
		return err
	}
	if fileData == nil {
		fmt.Printf("未找到配置文件 %s，使用默认配置\n", configFilePath())
	}

	setConfig(cfg)
	setLastWatchedBytes(fileData)

	return nil
}

// readConfigFromFile 读取并解析配置文件：默认值 -> TOML 覆盖 -> 环境变量覆盖。
// 文件不存在时返回默认配置且 fileData 为 nil；解析失败时返回错误，由调用方决定保留旧配置。
func readConfigFromFile() (cfg *AppConfig, fileData []byte, err error) {
	cfg = DefaultConfig()
	path := configFilePath()

	data, readErr := os.ReadFile(path)
	if readErr == nil {
		fileData = data
		if parseErr := toml.Unmarshal(data, cfg); parseErr != nil {
			return nil, nil, fmt.Errorf("解析配置文件 %s 失败: %v", path, parseErr)
		}
	} else if !os.IsNotExist(readErr) {
		// 文件存在但暂时不可读（权限、IO 错误等），不要用默认配置覆盖线上配置
		return nil, nil, fmt.Errorf("读取配置文件 %s 失败: %v", path, readErr)
	}

	overrideFromEnv(cfg)
	return cfg, fileData, nil
}

// ReloadConfig 重新读取配置文件并热更新到运行中的服务。
// 解析或读取失败时保留当前配置继续服务，返回错误供调用方记录日志。
func ReloadConfig() error {
	cfg, fileData, err := readConfigFromFile()
	if err != nil {
		fmt.Printf("配置热加载失败，继续使用当前配置: %v\n", err)
		return err
	}

	setConfig(cfg)
	setLastWatchedBytes(fileData)
	notifyReloadHooks(cfg)

	fmt.Printf("配置已热加载并即时生效: %s\n", configFilePath())
	return nil
}

// OnReload 注册配置热加载成功后的回调。回调在执行热加载的 goroutine 中同步执行，
// 各组件应在回调内只做无阻塞的原子切换。
func OnReload(hook func(*AppConfig)) {
	if hook == nil {
		return
	}
	reloadHooksMu.Lock()
	reloadHooks = append(reloadHooks, hook)
	reloadHooksMu.Unlock()
}

func notifyReloadHooks(cfg *AppConfig) {
	reloadHooksMu.RLock()
	hooks := make([]func(*AppConfig), 0, len(reloadHooks))
	hooks = append(hooks, reloadHooks...)
	reloadHooksMu.RUnlock()

	for _, hook := range hooks {
		hook(cfg)
	}
}

// StartReloadWatcher 启动配置文件变更监听。文件内容发生变化时自动热加载，
// 无需重启服务；解析失败时跳过本次变更并继续使用当前配置，待文件再次变化后重试。
func StartReloadWatcher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultConfigWatchInterval
	}

	watcherMu.Lock()
	if watcherStarted {
		watcherMu.Unlock()
		return
	}
	watcherStarted = true
	watcherMu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkConfigFileChange()
			}
		}
	}()
}

// checkConfigFileChange 比较配置文件内容与上次感知到的内容，变化则触发热加载
func checkConfigFileChange() {
	data, err := os.ReadFile(configFilePath())
	if err != nil {
		// 文件不存在或暂时不可读时跳过，等下一个周期再试
		return
	}

	watcherMu.Lock()
	unchanged := bytes.Equal(data, lastWatchedBytes)
	watcherMu.Unlock()
	if unchanged {
		return
	}

	if reloadErr := ReloadConfig(); reloadErr != nil {
		// 记录本次内容，避免同一份坏配置每个周期重复解析、刷屏日志
		setLastWatchedBytes(data)
	}
}

func setLastWatchedBytes(data []byte) {
	watcherMu.Lock()
	lastWatchedBytes = append(lastWatchedBytes[:0:0], data...)
	watcherMu.Unlock()
}

// overrideFromEnv 从环境变量覆盖配置
func overrideFromEnv(cfg *AppConfig) {
	if val := os.Getenv("SERVER_HOST"); val != "" {
		cfg.Server.Host = val
	}
	if val := os.Getenv("SERVER_PORT"); val != "" {
		if port, err := strconv.Atoi(val); err == nil && port > 0 {
			cfg.Server.Port = port
		}
	}
	if val := os.Getenv("ENABLE_H2C"); val != "" {
		if enable, err := strconv.ParseBool(val); err == nil {
			cfg.Server.EnableH2C = enable
		}
	}
	if val := os.Getenv("ENABLE_FRONTEND"); val != "" {
		if enable, err := strconv.ParseBool(val); err == nil {
			cfg.Server.EnableFrontend = enable
		}
	}
	if val := os.Getenv("MAX_FILE_SIZE"); val != "" {
		if size, err := strconv.ParseInt(val, 10, 64); err == nil && size > 0 {
			cfg.Server.FileSize = size
		}
	}

	if val := os.Getenv("RATE_LIMIT"); val != "" {
		if limit, err := strconv.Atoi(val); err == nil && limit > 0 {
			cfg.RateLimit.RequestLimit = limit
		}
	}
	if val := os.Getenv("RATE_PERIOD_HOURS"); val != "" {
		if period, err := strconv.ParseFloat(val, 64); err == nil && period > 0 {
			cfg.RateLimit.PeriodHours = period
		}
	}

	if val := os.Getenv("IP_WHITELIST"); val != "" {
		cfg.Security.WhiteList = append(cfg.Security.WhiteList, strings.Split(val, ",")...)
	}
	if val := os.Getenv("IP_BLACKLIST"); val != "" {
		cfg.Security.BlackList = append(cfg.Security.BlackList, strings.Split(val, ",")...)
	}

	if val, ok := os.LookupEnv("ACCESS_PROXY"); ok {
		cfg.Access.Proxy = strings.TrimSpace(val)
	}

	if val := os.Getenv("MAX_IMAGES"); val != "" {
		if maxImages, err := strconv.Atoi(val); err == nil && maxImages > 0 {
			cfg.Download.MaxImages = maxImages
		}
	}
}
