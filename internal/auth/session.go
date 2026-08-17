package auth

import (
	"sync"
	"time"
)

// session 是已登录管理员的会话记录。
type session struct {
	userID  string
	expires time.Time
}

// sessionStore 是内存态会话表(token -> session),带 TTL 过期清理。
type sessionStore struct {
	mu   sync.Mutex
	data map[string]session
}

func newSessionStore() *sessionStore {
	return &sessionStore{data: make(map[string]session)}
}

func (s *sessionStore) put(token, userID string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[token] = session{userID: userID, expires: time.Now().Add(ttl)}
}

func (s *sessionStore) get(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.data[token]
	if !ok {
		return "", false
	}
	if time.Now().After(ss.expires) {
		delete(s.data, token)
		return "", false
	}
	return ss.userID, true
}

func (s *sessionStore) delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, token)
}
