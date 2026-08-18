package output

import (
	"sync"
	"time"
)

// SendStats 记录实际上送的成功/失败统计,供各输出内嵌(值类型),线程安全。
// 语义约定:只在「真正发送到上游」的路径上更新(如 MQTT Publish 完成、TDengine 写库成功),
// 不在 Publish 入缓冲时更新——保证 LastSentAt/LastError 反映真实上送状态。
type SendStats struct {
	mu          sync.Mutex
	sent        int64
	lastSentAt  time.Time
	lastError   string
	lastErrorAt time.Time
}

// Success 记录一次成功上送。
func (s *SendStats) Success(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent++
	s.lastSentAt = t
}

// Failure 记录一次上送失败(保留最近一次错误)。
func (s *SendStats) Failure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = err.Error()
	s.lastErrorAt = time.Now()
}

// Snapshot 返回统计快照(线程安全)。
func (s *SendStats) Snapshot() (sent int64, lastSentAt time.Time, lastError string, lastErrorAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent, s.lastSentAt, s.lastError, s.lastErrorAt
}
