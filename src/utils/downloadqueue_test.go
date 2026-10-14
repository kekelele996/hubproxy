package utils

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const testIdentity = "1.2.3.4|test-agent"

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

func isAttached(q *DownloadQueue, id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.tickets[id]
	return ok && t.attached
}

// startAwait 在后台 goroutine 中等待票据，返回票据指针和取消函数
func startAwait(q *DownloadQueue, t *Ticket) (context.CancelFunc, chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := q.Await(ctx, t.ID, testIdentity)
		errCh <- err
	}()
	return cancel, errCh
}

func TestDownloadQueueFIFOOrder(t *testing.T) {
	q := NewDownloadQueue(1, 10, time.Minute)

	t1, err := q.Join("t1", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancel1, errCh1 := startAwait(q, t1)
	defer cancel1()
	waitFor(t, time.Second, func() bool { return q.Stats().Active == 1 })

	t2, err := q.Join("t2", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancel2, errCh2 := startAwait(q, t2)
	defer cancel2()
	waitFor(t, time.Second, func() bool { return isAttached(q, t2.ID) })

	t3, err := q.Join("t3", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancel3, errCh3 := startAwait(q, t3)
	defer cancel3()
	waitFor(t, time.Second, func() bool { return isAttached(q, t3.ID) })

	stats := q.Stats()
	if stats.Active != 1 || stats.Waiting != 2 {
		t.Fatalf("stats = %+v, want active=1 waiting=2", stats)
	}

	st2, ok := q.StatusOf(t2.ID)
	if !ok || st2.Position != 0 || st2.State != TicketWaiting {
		t.Fatalf("t2 status = %+v ok=%v", st2, ok)
	}
	st3, ok := q.StatusOf(t3.ID)
	if !ok || st3.Position != 1 {
		t.Fatalf("t3 status = %+v ok=%v", st3, ok)
	}

	// 释放第一个名额，必须严格按 FIFO 唤醒 t2，t3 继续等待
	q.Release(t1)
	waitFor(t, time.Second, func() bool { return t2.State() == TicketActive })
	if t3.State() != TicketWaiting {
		t.Fatalf("t3 不应提前获得名额, state=%s", t3.State())
	}
	st3, _ = q.StatusOf(t3.ID)
	if st3.Position != 0 {
		t.Fatalf("t3 前移后位置 = %d, want 0", st3.Position)
	}

	q.Release(t2)
	waitFor(t, time.Second, func() bool { return t3.State() == TicketActive })
	if err := <-errCh1; err != nil {
		t.Fatalf("t1 await err = %v", err)
	}
	if err := <-errCh2; err != nil {
		t.Fatalf("t2 await err = %v", err)
	}

	q.Release(t3)
	if err := <-errCh3; err != nil {
		t.Fatalf("t3 await err = %v", err)
	}

	stats = q.Stats()
	if stats.Active != 0 || stats.Waiting != 0 || stats.Completed != 3 {
		t.Fatalf("完成后 stats = %+v", stats)
	}
}

func TestDownloadQueueRejectsWhenFull(t *testing.T) {
	q := NewDownloadQueue(1, 1, time.Minute)

	t1, err := q.Join("t1", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancel1, _ := startAwait(q, t1)
	defer cancel1()
	waitFor(t, time.Second, func() bool { return q.Stats().Active == 1 })

	t2, err := q.Join("t2", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancel2, _ := startAwait(q, t2)
	defer cancel2()
	waitFor(t, time.Second, func() bool { return q.Stats().Waiting == 1 })

	if _, err := q.Join("t3", testIdentity); err != ErrQueueFull {
		t.Fatalf("队满错误 = %v, want ErrQueueFull", err)
	}
}

func TestDownloadQueueCancelWhileWaiting(t *testing.T) {
	q := NewDownloadQueue(1, 10, time.Minute)

	holder, err := q.Join("holder", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancelHolder, _ := startAwait(q, holder)
	defer cancelHolder()
	waitFor(t, time.Second, func() bool { return q.Stats().Active == 1 })

	waiting, err := q.Join("waiting", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancelWaiting, errCh := startAwait(q, waiting)
	waitFor(t, time.Second, func() bool { return isAttached(q, waiting.ID) })

	// 客户端断开：取消等待，票据应从队列移除且不消耗名额
	cancelWaiting()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("取消后 await 应返回错误")
		}
	case <-time.After(time.Second):
		t.Fatal("取消后 await 未退出")
	}
	if _, ok := q.StatusOf(waiting.ID); ok {
		t.Fatal("取消后票据仍在队列中")
	}

	next, err := q.Join("next", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancelNext, errChNext := startAwait(q, next)
	defer cancelNext()
	waitFor(t, time.Second, func() bool { return isAttached(q, next.ID) })

	q.Release(holder)
	waitFor(t, time.Second, func() bool { return next.State() == TicketActive })
	if err := <-errChNext; err != nil {
		t.Fatalf("next await err = %v", err)
	}
	q.Release(next)
}

func TestDownloadQueueAttachTimeout(t *testing.T) {
	q := NewDownloadQueue(1, 10, time.Minute)
	q.attachTimeout = 50 * time.Millisecond

	holder, err := q.Join("holder", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancelHolder, _ := startAwait(q, holder)
	defer cancelHolder()
	waitFor(t, time.Second, func() bool { return q.Stats().Active == 1 })

	// prepare 后下载请求迟迟不来，票据应过期失效
	abandoned, err := q.Join("abandoned", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(90 * time.Millisecond)
	if _, err := q.Await(context.Background(), abandoned.ID, testIdentity); err != ErrTicketNotFound {
		t.Fatalf("过期票据错误 = %v, want ErrTicketNotFound", err)
	}

	// 过期票据不能堵住后来者
	next, err := q.Join("next", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancelNext, errChNext := startAwait(q, next)
	defer cancelNext()
	q.Release(holder)
	waitFor(t, time.Second, func() bool { return next.State() == TicketActive })
	if err := <-errChNext; err != nil {
		t.Fatalf("next await err = %v", err)
	}
	q.Release(next)
}

func TestDownloadQueueWaitTimeout(t *testing.T) {
	q := NewDownloadQueue(1, 10, 80*time.Millisecond)
	q.attachTimeout = time.Minute

	holder, err := q.Join("holder", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancelHolder, _ := startAwait(q, holder)
	defer cancelHolder()
	waitFor(t, time.Second, func() bool { return q.Stats().Active == 1 })

	waiting, err := q.Join("waiting", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancelWaiting, errCh := startAwait(q, waiting)
	defer cancelWaiting()

	select {
	case err := <-errCh:
		if err != ErrQueueTimeout {
			t.Fatalf("等待超时错误 = %v, want ErrQueueTimeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("等待超时后 await 未退出")
	}
	if _, ok := q.StatusOf(waiting.ID); ok {
		t.Fatal("超时票据仍在队列中")
	}
}

func TestDownloadQueueIdentityMismatch(t *testing.T) {
	q := NewDownloadQueue(1, 10, time.Minute)
	ticket, err := q.Join("t", "ip-a|ua")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Await(context.Background(), ticket.ID, "ip-b|ua"); err != ErrTicketNotFound {
		t.Fatalf("身份不匹配错误 = %v, want ErrTicketNotFound", err)
	}
}

func TestDownloadQueueStatsAndETA(t *testing.T) {
	q := NewDownloadQueue(2, 10, time.Minute)

	t1, err := q.Acquire(context.Background(), "t1", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	q.Release(t1)

	stats := q.Stats()
	if !stats.Enabled || stats.Completed != 1 || stats.AvgDownloadSeconds <= 0 {
		t.Fatalf("stats = %+v", stats)
	}

	holder1, err := q.Acquire(context.Background(), "h1", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	holder2, err := q.Acquire(context.Background(), "h2", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Release(holder1)
	defer q.Release(holder2)

	waiting, err := q.Join("waiting", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cancel, _ := startAwait(q, waiting)
	defer cancel()
	waitFor(t, time.Second, func() bool { return q.Stats().Waiting == 1 })

	st, ok := q.StatusOf(waiting.ID)
	if !ok {
		t.Fatal("票据状态不存在")
	}
	if st.Position != 0 || st.EstimatedWaitSeconds < 1 {
		t.Fatalf("等待票据状态 = %+v, 期望位置0且有预计等待秒数", st)
	}
	if stats.EstimatedWaitSeconds < 0 {
		t.Fatalf("全局预计等待 = %d", stats.EstimatedWaitSeconds)
	}
}

func TestDownloadQueueDisabledHelpers(t *testing.T) {
	old := GlobalDownloadQueue
	GlobalDownloadQueue = nil
	defer func() { GlobalDownloadQueue = old }()

	if QueueEnabled() {
		t.Fatal("队列应为未启用状态")
	}
	ticket, err := JoinDownload("x", testIdentity)
	if ticket != nil || err != nil {
		t.Fatalf("未启用时 Join = %v, %v", ticket, err)
	}
	ticket, err = AwaitDownload(context.Background(), "any", testIdentity)
	if ticket != nil || err != nil {
		t.Fatalf("未启用时 Await = %v, %v", ticket, err)
	}
	ReleaseDownload(nil) // 不应 panic

	stats := GlobalQueueStats()
	if stats.Enabled {
		t.Fatalf("未启用时 stats.Enabled = true")
	}
}

func TestQueueStatusHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	old := GlobalDownloadQueue
	defer func() { GlobalDownloadQueue = old }()

	router := gin.New()
	router.GET("/api/queue/status", QueueStatusHandler)

	// 未启用
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/queue/status", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["enabled"] != false {
		t.Fatalf("enabled = %v", body["enabled"])
	}

	// 启用后返回队列字段
	GlobalDownloadQueue = NewDownloadQueue(3, 7, time.Minute)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/queue/status", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["enabled"] != true || body["max_concurrent"].(float64) != 3 || body["max_queue_size"].(float64) != 7 {
		t.Fatalf("响应 = %#v", body)
	}

	// 未知票据返回404
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/queue/status?ticket=not-exist", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("未知票据 status = %d, want 404", w.Code)
	}
}

func TestDownloadQueueConcurrentAcquire(t *testing.T) {
	// 高并发压力：同时 Acquire 的任务数远超并发上限，活跃数永远不超限，全部完成
	q := NewDownloadQueue(3, 200, time.Minute)

	const total = 60
	var wg sync.WaitGroup
	var peak int
	var peakMu sync.Mutex

	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticket, err := q.Acquire(context.Background(), "job", testIdentity)
			if err != nil {
				t.Errorf("Acquire err = %v", err)
				return
			}
			active := q.Stats().Active
			peakMu.Lock()
			if active > peak {
				peak = active
			}
			peakMu.Unlock()
			time.Sleep(5 * time.Millisecond)
			q.Release(ticket)
		}()
	}
	wg.Wait()

	if peak > 3 {
		t.Fatalf("峰值并发 = %d, 超过上限 3", peak)
	}
	if q.Stats().Completed != total {
		t.Fatalf("完成数 = %d, want %d", q.Stats().Completed, total)
	}
}
