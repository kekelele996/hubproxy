package utils

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 访问资源类型
const (
	AccessTypeDocker   = "docker"
	AccessTypeGitHub   = "github"
	AccessTypeToken    = "token"
	AccessTypeImageTar = "image_tar"
	AccessTypeAPI      = "api"
	AccessTypeOther    = "other"
)

// handler 可通过这些 Context Key 标注本次代理访问的资源类型与目标
const (
	ContextKeyAccessType   = "access_type"
	ContextKeyAccessTarget = "access_target"
)

const (
	// maxTargetLength 目标字段最大长度，防止异常长 URL 占用内存
	maxTargetLength = 512
	// maxUserAgentLength UA 最大长度
	maxUserAgentLength = 256
)

// AccessRecord 单次访问记录
type AccessRecord struct {
	ID         int64     `json:"id"`
	Time       time.Time `json:"time"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Target     string    `json:"target"`
	StatusCode int       `json:"status_code"`
	BytesIn    int64     `json:"bytes_in"`
	BytesOut   int64     `json:"bytes_out"`
	DurationMs int64     `json:"duration_ms"`
}

// AccessLogFilter 访问记录查询过滤条件
type AccessLogFilter struct {
	IP        string    `json:"ip"`
	Type      string    `json:"type"`
	Target    string    `json:"target"`
	Status    int       `json:"status"` // 0 表示不过滤
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
}

// Match 判断记录是否匹配过滤条件
func (f AccessLogFilter) Match(r AccessRecord) bool {
	if f.IP != "" && r.IP != f.IP {
		return false
	}
	if f.Type != "" && r.Type != f.Type {
		return false
	}
	if f.Target != "" && !strings.Contains(strings.ToLower(r.Target), strings.ToLower(f.Target)) {
		return false
	}
	if f.Status != 0 && r.StatusCode != f.Status {
		return false
	}
	if !f.StartTime.IsZero() && r.Time.Before(f.StartTime) {
		return false
	}
	if !f.EndTime.IsZero() && r.Time.After(f.EndTime) {
		return false
	}
	return true
}

// AccessSummaryGroup 汇总分组
type AccessSummaryGroup struct {
	Key      string    `json:"key"`
	Requests int64     `json:"requests"`
	BytesIn  int64     `json:"bytes_in"`
	BytesOut int64     `json:"bytes_out"`
	Errors   int64     `json:"errors"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
}

// AccessTotals 总体汇总
type AccessTotals struct {
	Requests int64     `json:"requests"`
	BytesIn  int64     `json:"bytes_in"`
	BytesOut int64     `json:"bytes_out"`
	Errors   int64     `json:"errors"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
}

// AccessLogStore 访问记录内存存储（环形缓冲，容量满后淘汰最旧记录）
type AccessLogStore struct {
	mu       sync.RWMutex
	records  []AccessRecord
	capacity int
	head     int // 最旧记录的下标
	count    int
	nextID   int64
	enabled  bool
}

// NewAccessLogStore 创建访问记录存储
func NewAccessLogStore(enabled bool, capacity int) *AccessLogStore {
	if capacity <= 0 {
		capacity = 10000
	}
	return &AccessLogStore{
		records:  make([]AccessRecord, capacity),
		capacity: capacity,
		enabled:  enabled,
	}
}

// Enabled 是否启用记录
func (s *AccessLogStore) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// Add 追加一条访问记录
func (s *AccessLogStore) Add(record AccessRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	record.ID = s.nextID

	if s.count < s.capacity {
		s.records[(s.head+s.count)%s.capacity] = record
		s.count++
		return
	}

	// 缓冲已满，覆盖最旧记录
	s.records[s.head] = record
	s.head = (s.head + 1) % s.capacity
}

// snapshot 返回按时间倒序（最新在前）的记录副本
func (s *AccessLogStore) snapshot() []AccessRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]AccessRecord, 0, s.count)
	for i := s.count - 1; i >= 0; i-- {
		result = append(result, s.records[(s.head+i)%s.capacity])
	}
	return result
}

// Query 按条件分页查询明细，返回当前页记录与匹配总数（按时间倒序）
func (s *AccessLogStore) Query(filter AccessLogFilter, page, pageSize int) ([]AccessRecord, int) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}

	all := s.snapshot()
	matched := make([]AccessRecord, 0, len(all))
	for _, record := range all {
		if filter.Match(record) {
			matched = append(matched, record)
		}
	}

	total := len(matched)
	start := (page - 1) * pageSize
	if start >= total {
		return []AccessRecord{}, total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return matched[start:end], total
}

// Summary 按指定维度汇总：ip / day / hour / target
func (s *AccessLogStore) Summary(groupBy string, filter AccessLogFilter) ([]AccessSummaryGroup, AccessTotals) {
	groups := make(map[string]*AccessSummaryGroup)
	var totals AccessTotals

	for _, record := range s.snapshot() {
		if !filter.Match(record) {
			continue
		}

		key := summaryKey(groupBy, record)
		group, exists := groups[key]
		if !exists {
			group = &AccessSummaryGroup{Key: key, FirstAt: record.Time, LastAt: record.Time}
			groups[key] = group
		}

		group.Requests++
		group.BytesIn += record.BytesIn
		group.BytesOut += record.BytesOut
		if record.StatusCode >= http.StatusBadRequest {
			group.Errors++
		}
		if record.Time.Before(group.FirstAt) {
			group.FirstAt = record.Time
		}
		if record.Time.After(group.LastAt) {
			group.LastAt = record.Time
		}

		totals.Requests++
		totals.BytesIn += record.BytesIn
		totals.BytesOut += record.BytesOut
		if record.StatusCode >= http.StatusBadRequest {
			totals.Errors++
		}
		if totals.FirstAt.IsZero() || record.Time.Before(totals.FirstAt) {
			totals.FirstAt = record.Time
		}
		if record.Time.After(totals.LastAt) {
			totals.LastAt = record.Time
		}
	}

	result := make([]AccessSummaryGroup, 0, len(groups))
	for _, group := range groups {
		result = append(result, *group)
	}

	// 流量大的分组排前面，便于快速定位"谁在猛拉"
	sort.Slice(result, func(i, j int) bool {
		if result[i].BytesOut != result[j].BytesOut {
			return result[i].BytesOut > result[j].BytesOut
		}
		return result[i].Key < result[j].Key
	})

	return result, totals
}

func summaryKey(groupBy string, record AccessRecord) string {
	switch groupBy {
	case "day":
		return record.Time.Format("2006-01-02")
	case "hour":
		return record.Time.Format("2006-01-02 15") + "时"
	case "target":
		if record.Target != "" {
			return record.Target
		}
		return record.Path
	case "ip":
		fallthrough
	default:
		return record.IP
	}
}

// GlobalAccessLogger 全局访问记录器
var GlobalAccessLogger *AccessLogStore

// InitAccessLogger 初始化全局访问记录器
func InitAccessLogger(enabled bool, capacity int) {
	GlobalAccessLogger = NewAccessLogStore(enabled, capacity)
}

// countingResponseWriter 统计实际写出字节数的 ResponseWriter
type countingResponseWriter struct {
	gin.ResponseWriter
	bytesWritten int64
}

func (w *countingResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.bytesWritten += int64(n)
	return n, err
}

func (w *countingResponseWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	w.bytesWritten += int64(n)
	return n, err
}

// AccessLogMiddleware 访问记录中间件，记录每次代理访问的来源、目标、时间与流量
func AccessLogMiddleware(store *AccessLogStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil || !store.Enabled() || isAccessLogSkipped(c.Request.URL.Path) {
			c.Next()
			return
		}

		start := time.Now()
		writer := &countingResponseWriter{ResponseWriter: c.Writer}
		c.Writer = writer

		c.Next()

		var bytesIn int64
		if c.Request.ContentLength > 0 {
			bytesIn = c.Request.ContentLength
		}

		recordType, target := resolveAccessResource(c)

		store.Add(AccessRecord{
			Time:       start,
			IP:         c.ClientIP(),
			UserAgent:  truncateString(c.Request.UserAgent(), maxUserAgentLength),
			Method:     c.Request.Method,
			Path:       c.Request.URL.Path,
			Type:       recordType,
			Target:     truncateString(target, maxTargetLength),
			StatusCode: c.Writer.Status(),
			BytesIn:    bytesIn,
			BytesOut:   writer.bytesWritten,
			DurationMs: time.Since(start).Milliseconds(),
		})
	}
}

// isAccessLogSkipped 前端页面、静态资源、健康检查与管理接口本身不记录
func isAccessLogSkipped(path string) bool {
	switch path {
	case "/", "/images", "/search", "/admin/access", "/favicon.ico", "/ready":
		return true
	}
	if strings.HasPrefix(path, "/assets/") {
		return true
	}
	if strings.HasPrefix(path, "/api/admin/") {
		return true
	}
	return false
}

// resolveAccessResource 解析本次访问的资源类型与目标；
// 优先使用 handler 通过 Context 标注的结果，缺失时按路径兜底解析。
func resolveAccessResource(c *gin.Context) (string, string) {
	recordType, _ := c.Get(ContextKeyAccessType)
	target, _ := c.Get(ContextKeyAccessTarget)

	recType, _ := recordType.(string)
	targetText, _ := target.(string)
	if recType != "" {
		return recType, targetText
	}

	path := c.Request.URL.Path
	switch {
	case strings.HasPrefix(path, "/v2/"):
		return AccessTypeDocker, parseDockerTargetFromPath(strings.TrimPrefix(path, "/v2/"))
	case path == "/token" || strings.HasPrefix(path, "/token/"):
		return AccessTypeToken, c.Query("service")
	case strings.HasPrefix(path, "/api/image/"):
		if image := c.Query("image"); image != "" {
			return AccessTypeImageTar, image
		}
		return AccessTypeImageTar, path
	case strings.HasPrefix(path, "/api/"):
		return AccessTypeAPI, path
	default:
		// GitHub/HF 等走 NoRoute 代理，路径即上游 URL
		return AccessTypeGitHub, strings.TrimPrefix(path, "/")
	}
}

// parseDockerTargetFromPath 从 Registry v2 路径中解析镜像名作为兜底
func parseDockerTargetFromPath(pathWithoutV2 string) string {
	var imageName string
	switch {
	case strings.Contains(pathWithoutV2, "/manifests/"):
		imageName = strings.SplitN(pathWithoutV2, "/manifests/", 2)[0]
	case strings.Contains(pathWithoutV2, "/blobs/"):
		imageName = strings.SplitN(pathWithoutV2, "/blobs/", 2)[0]
	case strings.Contains(pathWithoutV2, "/tags/list"):
		imageName = strings.SplitN(pathWithoutV2, "/tags/list", 2)[0]
	default:
		return ""
	}

	// Docker Hub 官方镜像省略 library/ 前缀
	if imageName != "" && !strings.Contains(imageName, "/") {
		imageName = "library/" + imageName
	}
	return imageName
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("%s...", s[:max])
}
