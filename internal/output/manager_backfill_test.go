package output

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"iot-gateway-go/internal/backfill"
	"iot-gateway-go/internal/model"
)

// ---- 假输出 ----

// fakeBackfillOut 是支持补传(实现 BackfillHealthy)的假输出:Publish 记录收到的点。
type fakeBackfillOut struct {
	mu      sync.Mutex
	got     []model.DataPoint
	healthy bool
}

func (f *fakeBackfillOut) Publish(dp model.DataPoint) error {
	f.mu.Lock()
	f.got = append(f.got, dp)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackfillOut) Close() error { return nil }

func (f *fakeBackfillOut) BackfillHealthy() bool { return f.healthy }

func (f *fakeBackfillOut) received() []model.DataPoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.DataPoint(nil), f.got...)
}

// blockingBackfillOut 是 Publish 会阻塞的假输出(测扇出队列满的补传入队)。
type blockingBackfillOut struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBackfillOut) Publish(dp model.DataPoint) error {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return nil
}

func (b *blockingBackfillOut) Close() error          { return nil }
func (b *blockingBackfillOut) BackfillHealthy() bool { return true }

// plainOutput 是不实现任何可选能力的普通假输出(不声明支持补传)。
type plainOutput struct{}

func (plainOutput) Publish(model.DataPoint) error { return nil }
func (plainOutput) Close() error                  { return nil }

// ---- 工具 ----

// newTestBackfill 构造内存 SQLite 补传队列。
func newTestBackfill(t *testing.T) *backfill.Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	bs, err := backfill.New(db, 0)
	if err != nil {
		t.Fatalf("backfill.New: %v", err)
	}
	return bs
}

// disableReplay 把后台重放间隔设为极大值,测试自行调用 replayOnce 驱动。
func disableReplay() func() {
	old := replayInterval
	replayInterval = time.Hour
	return func() { replayInterval = old }
}

// waitFor 轮询直到 cond 为真或超时。
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

func sampleDP(device, point string, v float64) model.DataPoint {
	return model.DataPoint{DeviceID: device, Point: point, Value: v, Timestamp: time.Now(), Quality: model.QualityGood}
}

// ---- 测试 ----

// TestReplayDeliversAndAcks 重放把持久化队列最旧一批经 slot.ch 投递,成功后 Ack 删除。
func TestReplayDeliversAndAcks(t *testing.T) {
	restore := disableReplay()
	defer restore()
	bs := newTestBackfill(t)
	fake := &fakeBackfillOut{healthy: true}
	m := NewManager(func() ([]Instance, error) {
		return []Instance{{Out: fake, ID: "o1"}}, nil
	})
	defer m.Close()
	m.SetBackfill(bs)
	if err := m.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	pts := []model.DataPoint{
		sampleDP("d1", "p1", 1),
		sampleDP("d1", "p2", 2),
		sampleDP("d2", "p1", 3),
	}
	if err := bs.Save("o1", pts); err != nil {
		t.Fatalf("save: %v", err)
	}

	m.replayOnce()

	waitFor(t, func() bool { return len(fake.received()) == 3 }, 3*time.Second)
	n, err := bs.CountByOutput("o1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("backfill count after replay = %d, want 0 (acked)", n)
	}
	// 顺序:按入队先后重放
	got := fake.received()
	for i, g := range got {
		if g.Point != pts[i].Point {
			t.Fatalf("replay order mismatch at %d: got %s want %s", i, g.Point, pts[i].Point)
		}
	}
}

// TestReplayGatedByHealth 输出不健康时不重放(队列保留)。
func TestReplayGatedByHealth(t *testing.T) {
	restore := disableReplay()
	defer restore()
	bs := newTestBackfill(t)
	fake := &fakeBackfillOut{healthy: false}
	m := NewManager(func() ([]Instance, error) {
		return []Instance{{Out: fake, ID: "o1"}}, nil
	})
	defer m.Close()
	m.SetBackfill(bs)
	if err := m.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := bs.Save("o1", []model.DataPoint{sampleDP("d", "p", 1)}); err != nil {
		t.Fatal(err)
	}

	m.replayOnce()
	time.Sleep(50 * time.Millisecond) // 给 slot goroutine 一点时间(若有意外投递)
	if len(fake.received()) != 0 {
		t.Fatalf("replayed while unhealthy: %d points delivered", len(fake.received()))
	}
	n, _ := bs.CountByOutput("o1")
	if n != 1 {
		t.Fatalf("backfill count = %d, want 1 (kept)", n)
	}
}

// TestReplaySkipsNonBackfillOutput 不声明支持补传的输出不参与重放。
func TestReplaySkipsNonBackfillOutput(t *testing.T) {
	restore := disableReplay()
	defer restore()
	bs := newTestBackfill(t)
	m := NewManager(func() ([]Instance, error) {
		return []Instance{{Out: plainOutput{}, ID: "plain"}}, nil
	})
	defer m.Close()
	m.SetBackfill(bs)
	if err := m.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := bs.Save("plain", []model.DataPoint{sampleDP("d", "p", 1)}); err != nil {
		t.Fatal(err)
	}
	m.replayOnce()
	n, _ := bs.CountByOutput("plain")
	if n != 1 {
		t.Fatalf("non-backfill output queue touched: count = %d, want 1", n)
	}
}

// TestQueueFullSavesToBackfill 扇出队列满时,启用补传的输出把点持久化而非丢弃。
func TestQueueFullSavesToBackfill(t *testing.T) {
	restore := disableReplay()
	defer restore()
	bs := newTestBackfill(t)
	blk := &blockingBackfillOut{started: make(chan struct{}), release: make(chan struct{})}
	m := NewManager(func() ([]Instance, error) {
		return []Instance{{Out: blk, ID: "o1"}}, nil
	})
	defer m.Close()
	m.SetBackfill(bs)
	if err := m.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// 先送一个点让 slot goroutine 进入阻塞的 Publish。
	m.Publish(sampleDP("d", "seed", 0))
	select {
	case <-blk.started:
	case <-time.After(3 * time.Second):
		t.Fatal("slot goroutine never entered Publish")
	}

	// goroutine 阻塞在 Publish,无人消费 channel → 填满扇出队列。
	s := m.slots[0]
	for i := 0; i < cap(s.ch); i++ {
		s.ch <- sampleDP("d", "fill", float64(i))
	}

	// 再发布一个 → 队列满 → 应入补传队列。
	m.Publish(sampleDP("d", "overflow", 999))

	n, err := bs.CountByOutput("o1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("backfill count after queue-full = %d, want 1", n)
	}
	items, _ := bs.Peek("o1", 10)
	if len(items) != 1 || items[0].DP.Point != "overflow" {
		t.Fatalf("saved point wrong: %+v", items)
	}

	close(blk.release) // 释放阻塞,让 slot 优雅退出
}

// TestReloadDropsRemovedOutputQueue 热重载后,被删除输出的残留补传队列被清空;
// 同 ID 重建的输出队列保留(续传)。
func TestReloadDropsRemovedOutputQueue(t *testing.T) {
	restore := disableReplay()
	defer restore()
	bs := newTestBackfill(t)

	fakeA := &fakeBackfillOut{healthy: true}
	fakeB := &fakeBackfillOut{healthy: true}
	build := func(ids ...string) func() ([]Instance, error) {
		return func() ([]Instance, error) {
			insts := make([]Instance, 0, len(ids))
			for i, id := range ids {
				out := Output(fakeB)
				if i == 0 {
					out = fakeA
				}
				insts = append(insts, Instance{Out: out, ID: id})
			}
			return insts, nil
		}
	}

	m := NewManager(build("a", "b"))
	defer m.Close()
	m.SetBackfill(bs)
	if err := m.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := bs.Save("a", []model.DataPoint{sampleDP("d", "p", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := bs.Save("b", []model.DataPoint{sampleDP("d", "p", 2)}); err != nil {
		t.Fatal(err)
	}

	// 热重载为只剩 a(删除 b)。
	m.build = build("a")
	if err := m.Reload(); err != nil {
		t.Fatalf("reload2: %v", err)
	}

	na, _ := bs.CountByOutput("a")
	nb, _ := bs.CountByOutput("b")
	if nb != 0 {
		t.Fatalf("removed output queue not dropped: b count = %d", nb)
	}
	if na != 1 {
		t.Fatalf("kept output queue disturbed: a count = %d, want 1", na)
	}
}
