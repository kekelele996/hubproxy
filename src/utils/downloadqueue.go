package utils

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
)

// 下载队列相关常量
const (
	// defaultMaxQueueSize 默认排队容量
	defaultMaxQueueSize = 100
	// defaultQueueTimeout 默认最长排队等待时间
	defaultQueueTimeout = 10 * time.Minute
	// attachTimeout prepare 阶段提前取票后，下载请求必须到达的最长时间
	attachTimeout = 30 * time.Second
	// doneTicketTTL 已完成票据在内存中的保留时间，供前端最后一次轮询
	doneTicketTTL = 5 * time.Minute
	// janitorInterval 清理过期票据的周期
	janitorInterval = 10 * time.Second
	// ewmaAlpha 下载耗时指数移动平均的平滑系数
	ewmaAlpha = 0.3
)

var (
	// ErrQueueFull 排队队列已满
	ErrQueueFull = errors.New("下载队列已满，请稍后再试")
	// ErrQueueTimeout 排队等待超时
	ErrQueueTimeout = errors.New("排队等待超时，请稍后重试")
	// ErrTicketNotFound 票据无效、过期或不属于当前客户端
	ErrTicketNotFound = errors.New("排队票据无效或已过期")
)

// TicketState 票据状态
type TicketState string

const (
	// TicketWaiting 排队等待中
	TicketWaiting TicketState = "waiting"
	// TicketActive 已获得下载名额，正在下载
	TicketActive TicketState = "active"
	// TicketDone 下载已完成（票据短暂保留供状态查询）
	TicketDone TicketState = "done"
)

// Ticket 排队票据
type Ticket struct {
	ID       string
	Label    string
	identity string

	mu         sync.RWMutex
	state      TicketState
	attached   bool
	enqueuedAt time.Time
	startedAt  time.Time
	endedAt    time.Time
	ready      chan struct{}
}

// State 返回票据当前状态
func (t *Ticket) State() TicketState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state
}

func (t *Ticket) setState(state TicketState) {
	t.mu.Lock()
	t.state = state
	t.mu.Unlock()
}

// WaitedSeconds 返回从入队到开始下载实际等待的秒数
func (t *Ticket) WaitedSeconds() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.startedAt.IsZero() {
		return time.Since(t.enqueuedAt).Seconds()
	}
	return t.startedAt.Sub(t.enqueuedAt).Seconds()
}

// QueueStats 队列整体状态
type QueueStats struct {
	Enabled              bool    `json:"enabled"`
	Active               int     `json:"active"`
	Waiting              int     `json:"waiting"`
	MaxConcurrent        int     `json:"max_concurrent"`
	MaxQueueSize         int     `json:"max_queue_size"`
	Completed            int64   `json:"completed"`
	AvgDownloadSeconds   float64 `json:"avg_download_seconds,omitempty"`
	EstimatedWaitSeconds int64   `json:"estimated_wait_seconds,omitempty"`
}

// TicketStatus 单张票据的排队状态
type TicketStatus struct {
	ID                   string      `json:"id"`
	State                TicketState `json:"state"`
	Label                string      `json:"label"`
	Position             int         `json:"position"`
	EnqueuedAtUnix       int64       `json:"enqueued_at_unix"`
	WaitSeconds          float64     `json:"wait_seconds"`
	EstimatedWaitSeconds int64       `json:"estimated_wait_seconds,omitempty"`
}

// DownloadQueue 全局下载并发控制队列（FIFO）
type DownloadQueue struct {
	mu sync.Mutex

	maxConcurrent int
	maxQueueSize  int
	maxWait       time.Duration
	attachTimeout time.Duration

	active  int
	waiting []*Ticket
	tickets map[string]*Ticket

	avgDuration time.Duration
	samples     int64
	completed   int64

	stop chan struct{}
}

// GlobalDownloadQueue 全局下载队列实例；nil 表示未启用并发限制
var GlobalDownloadQueue *DownloadQueue

// NewDownloadQueue 创建下载队列
func NewDownloadQueue(maxConcurrent, maxQueueSize int, maxWait time.Duration) *DownloadQueue {
	if maxConcurrent <= 0 {
		return nil
	}
	if maxQueueSize <= 0 {
		maxQueueSize = defaultMaxQueueSize
	}
	if maxWait <= 0 {
		maxWait = defaultQueueTimeout
	}
	return &DownloadQueue{
		maxConcurrent: maxConcurrent,
		maxQueueSize:  maxQueueSize,
		maxWait:       maxWait,
		attachTimeout: attachTimeout,
		tickets:       make(map[string]*Ticket),
		stop:          make(chan struct{}),
	}
}

// InitGlobalDownloadQueue 根据配置初始化全局下载队列，maxConcurrent<=0 时关闭限制
func InitGlobalDownloadQueue() {
	if GlobalDownloadQueue != nil {
		close(GlobalDownloadQueue.stop)
		GlobalDownloadQueue = nil
	}

	cfg := config.GetConfig()
	if cfg.Download.MaxConcurrent <= 0 {
		return
	}

	maxWait, err := time.ParseDuration(cfg.Download.QueueTimeout)
	if err != nil || maxWait <= 0 {
		maxWait = defaultQueueTimeout
	}

	q := NewDownloadQueue(cfg.Download.MaxConcurrent, cfg.Download.MaxQueueSize, maxWait)
	GlobalDownloadQueue = q
	go q.janitor()
}

func randomTicketID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// rand.Read 极少失败，退回时间戳保证可用性
		return fmt.Sprintf("t-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Join 进入排队（尚未开始等待），用于 prepare 提前取票。
// identity 用于绑定取票和下载请求（IP + User-Agent）。
func (q *DownloadQueue) Join(label, identity string) (*Ticket, error) {
	now := time.Now()

	q.mu.Lock()
	defer q.mu.Unlock()

	q.sweepLocked(now)

	if len(q.waiting) >= q.maxQueueSize {
		return nil, ErrQueueFull
	}

	t := &Ticket{
		ID:         randomTicketID(),
		Label:      label,
		identity:   identity,
		state:      TicketWaiting,
		enqueuedAt: now,
		ready:      make(chan struct{}),
	}
	q.waiting = append(q.waiting, t)
	q.tickets[t.ID] = t
	return t, nil
}

// Await 绑定票据并阻塞等待下载名额。
func (q *DownloadQueue) Await(ctx context.Context, ticketID, identity string) (*Ticket, error) {
	now := time.Now()

	q.mu.Lock()
	t, ok := q.tickets[ticketID]
	if !ok {
		q.mu.Unlock()
		return nil, ErrTicketNotFound
	}
	if t.identity != identity || t.State() != TicketWaiting {
		q.mu.Unlock()
		return nil, ErrTicketNotFound
	}
	if now.Sub(t.enqueuedAt) > q.attachTimeout {
		q.removeWaitingLocked(t)
		delete(q.tickets, t.ID)
		q.grantLocked(now)
		q.mu.Unlock()
		return nil, ErrTicketNotFound
	}

	t.attached = true
	ready := t.ready
	deadline := t.enqueuedAt.Add(q.maxWait)
	q.grantLocked(now)
	q.mu.Unlock()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case <-ready:
		return t, nil
	case <-ctx.Done():
		q.removeTicket(t.ID)
		return nil, ctx.Err()
	case <-timer.C:
		q.removeTicket(t.ID)
		return nil, ErrQueueTimeout
	}
}

// Acquire 立即入队并等待下载名额，用于无 prepare 阶段的代理下载。
func (q *DownloadQueue) Acquire(ctx context.Context, label, identity string) (*Ticket, error) {
	t, err := q.Join(label, identity)
	if err != nil {
		return nil, err
	}
	return q.Await(ctx, t.ID, identity)
}

// Release 归还下载名额并尝试唤醒队首
func (q *DownloadQueue) Release(t *Ticket) {
	if t == nil {
		return
	}

	t.mu.Lock()
	if t.state != TicketActive {
		t.mu.Unlock()
		return
	}
	t.state = TicketDone
	t.endedAt = time.Now()
	duration := t.endedAt.Sub(t.startedAt)
	t.mu.Unlock()

	q.mu.Lock()
	defer q.mu.Unlock()

	q.active--
	q.completed++
	if duration > 0 {
		if q.samples == 0 {
			q.avgDuration = duration
		} else {
			q.avgDuration = time.Duration((1-ewmaAlpha)*float64(q.avgDuration) + ewmaAlpha*float64(duration))
		}
		q.samples++
	}
	q.grantLocked(time.Now())
}

// grantLocked 按 FIFO 把空闲名额分配给已就位（下载请求已到达）的队首票据
func (q *DownloadQueue) grantLocked(now time.Time) {
	for q.active < q.maxConcurrent {
		// 先丢弃队首已过期的票据
		for len(q.waiting) > 0 {
			head := q.waiting[0]
			expired := false
			if !head.attached && now.Sub(head.enqueuedAt) > q.attachTimeout {
				expired = true
			} else if head.attached && now.Sub(head.enqueuedAt) > q.maxWait {
				expired = true
			}
			if !expired {
				break
			}
			q.waiting = q.waiting[1:]
			delete(q.tickets, head.ID)
		}

		if len(q.waiting) == 0 {
			return
		}

		next := q.waiting[0]
		// 严格 FIFO：队首下载请求还没到达时，不允许后来者插队
		if !next.attached {
			return
		}

		q.waiting = q.waiting[1:]
		q.active++
		next.mu.Lock()
		next.state = TicketActive
		next.startedAt = now
		next.mu.Unlock()
		close(next.ready)
	}
}

// removeWaitingLocked 从等待队列中移除票据（调用方持锁）
func (q *DownloadQueue) removeWaitingLocked(target *Ticket) {
	for i, t := range q.waiting {
		if t == target {
			q.waiting = append(q.waiting[:i], q.waiting[i+1:]...)
			return
		}
	}
}

// removeTicket 取消等待中的票据并重新调度
func (q *DownloadQueue) removeTicket(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t, ok := q.tickets[id]; ok && t.State() == TicketWaiting {
		q.removeWaitingLocked(t)
		delete(q.tickets, id)
		q.grantLocked(time.Now())
	}
}

// sweepLocked 清理已完成且超过保留期的票据（调用方持锁）
func (q *DownloadQueue) sweepLocked(now time.Time) {
	for id, t := range q.tickets {
		if t.State() == TicketDone {
			t.mu.RLock()
			ended := t.endedAt
			t.mu.RUnlock()
			if !ended.IsZero() && now.Sub(ended) > doneTicketTTL {
				delete(q.tickets, id)
			}
		}
	}
}

func (q *DownloadQueue) janitor() {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-q.stop:
			return
		case now := <-ticker.C:
			q.mu.Lock()
			q.sweepLocked(now)
			q.grantLocked(now)
			q.mu.Unlock()
		}
	}
}

// estimateLocked 估算队中位置为 position（前面还有 position 个等待者）时的等待秒数。
// 吞吐按 maxConcurrent/平均下载耗时 估算，仅供“大概还要多久”参考。
func (q *DownloadQueue) estimateLocked(position int) (int64, bool) {
	if q.samples == 0 {
		return 0, false
	}
	seconds := float64(position+1) / float64(q.maxConcurrent) * q.avgDuration.Seconds()
	return int64(math.Ceil(seconds)), true
}

// Stats 返回队列整体状态；新请求的预计等待按直接排到队尾估算
func (q *DownloadQueue) Stats() QueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()

	stats := QueueStats{
		Enabled:       true,
		Active:        q.active,
		Waiting:       len(q.waiting),
		MaxConcurrent: q.maxConcurrent,
		MaxQueueSize:  q.maxQueueSize,
		Completed:     q.completed,
	}
	if q.samples > 0 {
		stats.AvgDownloadSeconds = math.Round(q.avgDuration.Seconds()*10) / 10
		if secs, ok := q.estimateLocked(len(q.waiting)); ok {
			stats.EstimatedWaitSeconds = secs
		}
	}
	return stats
}

// StatusOf 查询单张票据状态
func (q *DownloadQueue) StatusOf(ticketID string) (TicketStatus, bool) {
	now := time.Now()

	q.mu.Lock()
	defer q.mu.Unlock()

	t, ok := q.tickets[ticketID]
	if !ok {
		return TicketStatus{}, false
	}

	t.mu.RLock()
	state := t.state
	enqueued := t.enqueuedAt
	started := t.startedAt
	t.mu.RUnlock()

	status := TicketStatus{
		ID:             t.ID,
		State:          state,
		Label:          t.Label,
		Position:       -1,
		EnqueuedAtUnix: enqueued.Unix(),
		WaitSeconds:    math.Round(now.Sub(enqueued).Seconds()*10) / 10,
	}

	if state == TicketWaiting {
		for i, wt := range q.waiting {
			if wt == t {
				status.Position = i
				if secs, ok := q.estimateLocked(i); ok {
					status.EstimatedWaitSeconds = secs
				}
				break
			}
		}
	} else if state == TicketActive {
		if !started.IsZero() {
			status.WaitSeconds = math.Round(started.Sub(enqueued).Seconds()*10) / 10
		}
	}

	return status, true
}

// ---------------- 全局便捷函数（队列未启用时全部透传） ----------------

// QueueEnabled 是否已启用下载并发限制
func QueueEnabled() bool {
	return GlobalDownloadQueue != nil
}

// JoinDownload 全局排队取票
func JoinDownload(label, identity string) (*Ticket, error) {
	if GlobalDownloadQueue == nil {
		return nil, nil
	}
	return GlobalDownloadQueue.Join(label, identity)
}

// AwaitDownload 全局票据等待；ticket 为空时直接透传（队列未启用）
func AwaitDownload(ctx context.Context, ticketID, identity string) (*Ticket, error) {
	if GlobalDownloadQueue == nil || ticketID == "" {
		return nil, nil
	}
	return GlobalDownloadQueue.Await(ctx, ticketID, identity)
}

// AcquireDownload 全局立即排队并等待
func AcquireDownload(ctx context.Context, label, identity string) (*Ticket, error) {
	if GlobalDownloadQueue == nil {
		return nil, nil
	}
	return GlobalDownloadQueue.Acquire(ctx, label, identity)
}

// ReleaseDownload 全局释放名额
func ReleaseDownload(t *Ticket) {
	if GlobalDownloadQueue == nil || t == nil {
		return
	}
	GlobalDownloadQueue.Release(t)
}

// GlobalQueueStats 返回全局队列状态（未启用时 Enabled=false）
func GlobalQueueStats() QueueStats {
	if GlobalDownloadQueue == nil {
		return QueueStats{Enabled: false}
	}
	return GlobalDownloadQueue.Stats()
}

// QueueStatusHandler GET /api/queue/status，支持 ?ticket=xxx 查询个人排队位置
func QueueStatusHandler(c *gin.Context) {
	resp := gin.H{}
	stats := GlobalQueueStats()
	resp["enabled"] = stats.Enabled
	if stats.Enabled {
		resp["active"] = stats.Active
		resp["waiting"] = stats.Waiting
		resp["max_concurrent"] = stats.MaxConcurrent
		resp["max_queue_size"] = stats.MaxQueueSize
		resp["completed"] = stats.Completed
		if stats.AvgDownloadSeconds > 0 {
			resp["avg_download_seconds"] = stats.AvgDownloadSeconds
		}
		if stats.EstimatedWaitSeconds > 0 {
			resp["estimated_wait_seconds"] = stats.EstimatedWaitSeconds
		}
	}

	if ticketID := c.Query("ticket"); ticketID != "" {
		if GlobalDownloadQueue == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "下载排队未启用"})
			return
		}
		status, ok := GlobalDownloadQueue.StatusOf(ticketID)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "排队票据无效或已过期"})
			return
		}
		resp["ticket"] = status
	}

	c.JSON(http.StatusOK, resp)
}

// WriteQueueError 把排队错误转成合适的 HTTP 响应，并带 Retry-After
func WriteQueueError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrQueueFull):
		retryAfter := GlobalQueueStats().EstimatedWaitSeconds
		if retryAfter <= 0 {
			retryAfter = 10
		}
		c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":       "当前下载人数较多，排队队列已满，请稍后再试",
			"retry_after": retryAfter,
		})
	case errors.Is(err, ErrQueueTimeout):
		c.Header("Retry-After", "5")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":       "排队等待时间过长，请重新发起下载",
			"retry_after": 5,
		})
	case errors.Is(err, ErrTicketNotFound):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "下载排队失败，请稍后再试"})
	}
}
