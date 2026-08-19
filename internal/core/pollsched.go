package core

import (
	"hash/fnv"
	"sync"
	"time"
)

// pollScheduler 是轮询采集的定时调度器,替代 robfig/cron。
//
// 背景:robfig/cron v3.0.1 的 ConstantDelaySchedule(`@every`/`Every`)会把 <1s 的
// 间隔**钳到 1s**,并**静默截掉任何亚秒余量**(`@every 1500ms` 实际 = 1s),且
// `Next()` 对齐秒边界导致同间隔设备在同一秒边界集体触发。见 docs/scale-testing.md §6.1。
//
// 设计:
//   - 常驻 goroutine 恒为 1,与设备数解耦(满足大规模与 ARM 低资源约束);
//   - 按"下次触发时间"用小顶堆排序,到点**批量触发所有到期设备**(固定速率、不追补
//     落后刻度,沿用旧 cron "一次醒来最多触发一次"的语义,避免补发惊群);
//   - 首次触发相位 = fnv(deviceID) % interval,同间隔设备按 ID 哈希错峰分布,
//     避免旧 cron 秒边界集体触发的"惊群"(thundering herd);
//   - 触发即向 worker pool **非阻塞投递**(满则跳过),调度 goroutine 内绝不做 I/O。
type pollScheduler struct {
	mu    sync.Mutex
	heap  pollHeap              // 按 next 升序
	byID  map[string]*pollEntry // deviceID -> 当前条目(remove 惰性标记 stale)
	wake  chan struct{}         // 结构变更时唤醒调度 goroutine 重排 timer
	done  chan struct{}         // stop 信号
	stopC chan struct{}         // run 退出后关闭,供 stop 等待

	started  bool
	stopOnce sync.Once
}

// pollEntry 是一个设备的调度条目;interval 变化时旧条目标记 stale 惰性删除。
type pollEntry struct {
	deviceID string
	job      *deviceJob
	interval time.Duration
	next     time.Time
	stale    bool
}

func newPollScheduler() *pollScheduler {
	return &pollScheduler{
		byID:  make(map[string]*pollEntry),
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
		stopC: make(chan struct{}),
	}
}

// start 启动调度 goroutine。
func (ps *pollScheduler) start() {
	ps.mu.Lock()
	if ps.started {
		ps.mu.Unlock()
		return
	}
	ps.started = true
	ps.mu.Unlock()
	go ps.run()
}

// schedule 注册或更新设备的轮询计划(间隔变化也走此路径,保留 job 指针)。
// 旧条目标记 stale,由调度循环弹出时清理,无需 O(n) 堆内删除。
func (ps *pollScheduler) schedule(deviceID string, interval time.Duration, job *deviceJob) {
	if interval <= 0 {
		interval = defaultInterval
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if e, ok := ps.byID[deviceID]; ok {
		e.stale = true
		delete(ps.byID, deviceID)
	}
	e := &pollEntry{
		deviceID: deviceID,
		job:      job,
		interval: interval,
		next:     time.Now().Add(hashPhase(deviceID, interval)),
	}
	ps.byID[deviceID] = e
	ps.heap.push(e)
	ps.nudgeLocked()
}

// remove 移除设备的轮询计划(惰性删除)。
func (ps *pollScheduler) remove(deviceID string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if e, ok := ps.byID[deviceID]; ok {
		e.stale = true
		delete(ps.byID, deviceID)
		ps.nudgeLocked()
	}
}

// stop 停止调度 goroutine 并等待其退出;幂等,未启动时直接返回。
func (ps *pollScheduler) stop() {
	ps.mu.Lock()
	started := ps.started
	ps.mu.Unlock()
	if !started {
		return
	}
	ps.stopOnce.Do(func() {
		close(ps.done)
		<-ps.stopC
	})
}

// nudgeLocked 唤醒调度 goroutine 重排 timer(须持 ps.mu)。
func (ps *pollScheduler) nudgeLocked() {
	select {
	case ps.wake <- struct{}{}:
	default:
	}
}

// run 是唯一调度 goroutine:睡到堆顶到期 → 批量触发全部到期条目 → 重排 → 循环。
func (ps *pollScheduler) run() {
	defer close(ps.stopC)
	for {
		ps.mu.Lock()
		ps.heap.dropStale()
		if ps.heap.len() == 0 {
			ps.mu.Unlock()
			select {
			case <-ps.done:
				return
			case <-ps.wake:
			}
			continue
		}
		delay := time.Until(ps.heap.peek().next)
		if delay < 0 {
			delay = 0
		}
		ps.mu.Unlock()

		timer := time.NewTimer(delay)
		select {
		case <-ps.done:
			timer.Stop()
			return
		case <-ps.wake:
			timer.Stop()
		case <-timer.C:
		}

		now := time.Now()
		due := make([]*pollEntry, 0, 8)
		ps.mu.Lock()
		ps.heap.dropStale()
		for ps.heap.len() > 0 {
			e := ps.heap.peek()
			if e.stale {
				ps.heap.pop()
				continue
			}
			if e.next.After(now) {
				break
			}
			ps.heap.pop()
			// 固定速率推进:落后超过一个周期则不追补(避免补发惊群)。
			e.next = e.next.Add(e.interval)
			if e.next.Before(now) {
				e.next = now.Add(e.interval)
			}
			ps.heap.push(e)
			due = append(due, e)
		}
		ps.mu.Unlock()

		for _, e := range due {
			e.job.fire()
		}
	}
}

// hashPhase 给设备一个稳定的相位偏移,让同间隔设备错峰分布,避免集体触发。
// interval 恒为正(调用方已兜底)。
func hashPhase(deviceID string, interval time.Duration) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(deviceID))
	return time.Duration(uint64(h.Sum32()) % uint64(interval))
}

// ---- 小顶堆(pollEntry by next) ----

type pollHeap []*pollEntry

func (h *pollHeap) len() int { return len(*h) }

func (h *pollHeap) peek() *pollEntry { return (*h)[0] }

func (h *pollHeap) push(e *pollEntry) {
	*h = append(*h, e)
	up(*h, len(*h)-1)
}

func (h *pollHeap) pop() *pollEntry {
	old := *h
	n := len(old)
	e := old[0]
	old[0] = old[n-1]
	*h = old[:n-1]
	down(*h, 0)
	return e
}

// dropStale 清理堆顶的惰性删除条目,避免空转。
func (h *pollHeap) dropStale() {
	for h.len() > 0 && (*h)[0].stale {
		h.pop()
	}
}

func entryLess(a, b *pollEntry) bool {
	if a.next.Equal(b.next) {
		return a.deviceID < b.deviceID // 同刻触发时确定性排序
	}
	return a.next.Before(b.next)
}

func up(h pollHeap, i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !entryLess(h[i], h[p]) {
			break
		}
		h[i], h[p] = h[p], h[i]
		i = p
	}
}

func down(h pollHeap, i int) {
	n := len(h)
	for {
		l := 2*i + 1
		if l >= n {
			break
		}
		r := l + 1
		m := l
		if r < n && entryLess(h[r], h[l]) {
			m = r
		}
		if !entryLess(h[m], h[i]) {
			break
		}
		h[i], h[m] = h[m], h[i]
		i = m
	}
}
