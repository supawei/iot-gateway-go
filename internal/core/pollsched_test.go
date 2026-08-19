package core

import (
	"context"
	"testing"
	"time"
)

// newTestJob 构造一个投递到独立 taskCh 的 deviceJob,供 pollScheduler 直测。
func newTestJob(id string) (*deviceJob, chan collectTask) {
	taskCh := make(chan collectTask, 256)
	return &deviceJob{taskCh: taskCh, deviceID: id, ctx: context.Background()}, taskCh
}

// TestPollSchedulerSubSecond 验证 pollScheduler 支持亚秒级精确节拍
// (robfig/cron 会把 @every 50ms 钳成 1s;pollScheduler 应 ≈20 次/s)。
func TestPollSchedulerSubSecond(t *testing.T) {
	ps := newPollScheduler()
	ps.start()
	defer ps.stop()

	job, taskCh := newTestJob("d1")
	ps.schedule("d1", 50*time.Millisecond, job)

	time.Sleep(320 * time.Millisecond) // 相位 ∈[0,50ms) → 期望 6~7 次
	fires := len(taskCh)
	if fires < 5 || fires > 8 {
		t.Fatalf("fires in 320ms = %d, want ~6 (50ms interval, cron would be 0~1)", fires)
	}
}

// TestPollSchedulerRemove 验证 remove 后不再触发(惰性删除生效)。
func TestPollSchedulerRemove(t *testing.T) {
	ps := newPollScheduler()
	ps.start()
	defer ps.stop()

	job, taskCh := newTestJob("d1")
	ps.schedule("d1", 30*time.Millisecond, job)

	time.Sleep(120 * time.Millisecond)
	before := len(taskCh)
	if before == 0 {
		t.Fatal("expected at least one fire before remove")
	}

	ps.remove("d1")
	time.Sleep(150 * time.Millisecond)
	if after := len(taskCh); after != before {
		t.Fatalf("fires after remove: before=%d after=%d, want unchanged", before, after)
	}
}

// TestPollSchedulerReschedule 验证间隔变化即时生效(重排而非重建)。
func TestPollSchedulerReschedule(t *testing.T) {
	ps := newPollScheduler()
	ps.start()
	defer ps.stop()

	job, taskCh := newTestJob("d1")
	ps.schedule("d1", 200*time.Millisecond, job)

	time.Sleep(250 * time.Millisecond)
	slow := len(taskCh) // 200ms → 约 1~2 次
	if slow == 0 {
		t.Fatal("expected fire at 200ms interval")
	}

	ps.schedule("d1", 50*time.Millisecond, job) // 复用同 job 指针
	time.Sleep(300 * time.Millisecond)
	delta := len(taskCh) - slow
	if delta < 4 {
		t.Fatalf("fires after reschedule to 50ms = %d, want ~6 (>4)", delta)
	}
}

// TestPollSchedulerStopIdempotent 验证 stop 幂等(不 panic、可重复调用)。
func TestPollSchedulerStopIdempotent(t *testing.T) {
	ps := newPollScheduler()
	ps.start()
	job, _ := newTestJob("d1")
	ps.schedule("d1", 50*time.Millisecond, job)
	time.Sleep(80 * time.Millisecond)
	ps.stop()
	ps.stop() // 二次调用不应 panic/死锁
}

// TestPollHeapOrdering 小顶堆按 next 升序,同刻按 deviceID 确定性排序。
func TestPollHeapOrdering(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	var h pollHeap
	h.push(&pollEntry{deviceID: "c", next: base.Add(3 * time.Second)})
	h.push(&pollEntry{deviceID: "a", next: base.Add(1 * time.Second)})
	h.push(&pollEntry{deviceID: "b", next: base.Add(2 * time.Second)})
	h.push(&pollEntry{deviceID: "d", next: base.Add(1 * time.Second)})

	if got := h.peek().deviceID; got != "a" {
		t.Fatalf("peek = %s, want a", got)
	}
	var order []string
	for h.len() > 0 {
		order = append(order, h.pop().deviceID)
	}
	want := []string{"a", "d", "b", "c"} // 同刻 a<d(字典序)
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestPollHeapDropStale 验证堆顶惰性条目被清理,不影响后续弹出。
func TestPollHeapDropStale(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	var h pollHeap
	stale := &pollEntry{deviceID: "x", next: base, stale: true}
	h.push(stale)
	h.push(&pollEntry{deviceID: "a", next: base.Add(time.Second)})
	h.dropStale()
	if h.len() != 1 || h.peek().deviceID != "a" {
		t.Fatalf("after dropStale: len=%d peek=%v, want 1/a", h.len(), h.peek())
	}
}

// TestPollSchedulerMultipleDevices 多设备同间隔时按相位错峰,总触发速率 = 设备数/周期。
func TestPollSchedulerMultipleDevices(t *testing.T) {
	ps := newPollScheduler()
	ps.start()
	defer ps.stop()

	const n = 20
	chans := make([]chan collectTask, n)
	for i := 0; i < n; i++ {
		job, taskCh := newTestJob("dev" + string(rune('a'+i)))
		chans[i] = taskCh
		ps.schedule("dev"+string(rune('a'+i)), 100*time.Millisecond, job)
	}

	time.Sleep(650 * time.Millisecond) // 100ms 间隔 → 每设备 ≈6~7 次
	total := int64(0)
	for _, c := range chans {
		total += int64(len(c))
	}
	// 期望 ≈ n×6.5=130;robfig/cron 时代(1s 节拍)只有 ≈20。宽区间防 CI 抖动。
	if total < 100 || total > 160 {
		t.Fatalf("total fires(20 dev @100ms, 650ms) = %d, want ~130", total)
	}
}
