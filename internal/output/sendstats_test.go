package output

import (
	"errors"
	"testing"
	"time"
)

func TestSendStats(t *testing.T) {
	var s SendStats
	sent, _, lastErr, _ := s.Snapshot()
	if sent != 0 || lastErr != "" {
		t.Fatalf("initial snapshot: sent=%d err=%q", sent, lastErr)
	}

	now := time.Now()
	s.Success(now)
	s.Success(now.Add(time.Second))
	s.Failure(errors.New("boom"))

	sent, lastSent, lastErr, lastErrAt := s.Snapshot()
	if sent != 2 {
		t.Fatalf("sent = %d, want 2", sent)
	}
	if !lastSent.Equal(now.Add(time.Second)) {
		t.Fatalf("lastSent = %v, want %v", lastSent, now.Add(time.Second))
	}
	if lastErr != "boom" {
		t.Fatalf("lastErr = %q, want boom", lastErr)
	}
	if lastErrAt.IsZero() {
		t.Fatal("lastErrAt should be set")
	}
}
