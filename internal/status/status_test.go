package status

import (
	"testing"
	"time"
)

func TestRegistryOnlineOffline(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Get("d1"); ok {
		t.Fatal("unknown device should not be found")
	}

	now := time.Now()
	r.SetOnline("d1", now)
	got, ok := r.Get("d1")
	if !ok || !got.Online || !got.LastCollect.Equal(now) || got.LastError != "" {
		t.Fatalf("online status: %+v", got)
	}

	r.SetOffline("d1", "connection refused")
	got, _ = r.Get("d1")
	if got.Online || got.LastError != "connection refused" {
		t.Fatalf("offline status: %+v", got)
	}
	if !got.LastCollect.Equal(now) {
		t.Fatalf("offline should keep last collect time: %+v", got)
	}
}

func TestRegistryListSorted(t *testing.T) {
	r := NewRegistry()
	r.SetOnline("b", time.Now())
	r.SetOffline("a", "err")
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("len=%d", len(list))
	}
	if list[0].DeviceID != "a" || list[1].DeviceID != "b" {
		t.Fatalf("not sorted: %+v", list)
	}
}
