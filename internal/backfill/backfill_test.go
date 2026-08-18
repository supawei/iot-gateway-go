package backfill

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"iot-gateway-go/internal/model"
)

func newTestStore(t *testing.T, max int) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := New(db, max)
	if err != nil {
		t.Fatalf("New backfill store: %v", err)
	}
	return s
}

func dp(device, point string, ts int64, value interface{}, q model.Quality) model.DataPoint {
	return model.DataPoint{
		DeviceID:  device,
		Point:     point,
		Value:     value,
		Timestamp: time.UnixMilli(ts),
		Quality:   q,
	}
}

func TestSavePeekAckOrder(t *testing.T) {
	s := newTestStore(t, 0)

	err := s.Save("out-1", []model.DataPoint{
		dp("d1", "p1", 1000, 1.5, model.QualityGood),
		dp("d1", "p2", 1001, "str", model.QualityGood),
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// 另一输出互不影响
	if err := s.Save("out-2", []model.DataPoint{dp("d2", "p1", 2000, 9, model.QualityBad)}); err != nil {
		t.Fatalf("save out-2: %v", err)
	}

	n, err := s.CountByOutput("out-1")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("count out-1 = %d, want 2", n)
	}
	total, _ := s.TotalCount()
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}

	items, err := s.Peek("out-1", 10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("peek len = %d, want 2", len(items))
	}
	// FIFO:先入先出
	if items[0].DP.Point != "p1" || items[1].DP.Point != "p2" {
		t.Fatalf("order wrong: %+v", items)
	}
	// JSON 往返还原值类型与质量
	if items[0].DP.Value.(float64) != 1.5 {
		t.Fatalf("value round-trip = %#v", items[0].DP.Value)
	}
	if items[1].DP.Value.(string) != "str" {
		t.Fatalf("string round-trip = %#v", items[1].DP.Value)
	}
	if items[0].DP.Quality != model.QualityGood {
		t.Fatalf("quality round-trip = %q", items[0].DP.Quality)
	}
	if items[0].DP.Timestamp.UnixMilli() != 1000 {
		t.Fatalf("ts round-trip = %v", items[0].DP.Timestamp)
	}

	// Ack 后不再返回
	ids := []int64{items[0].ID, items[1].ID}
	if err := s.Ack("out-1", ids); err != nil {
		t.Fatalf("ack: %v", err)
	}
	rest, _ := s.Peek("out-1", 10)
	if len(rest) != 0 {
		t.Fatalf("after ack peek len = %d, want 0", len(rest))
	}
	// out-2 不受影响
	n2, _ := s.CountByOutput("out-2")
	if n2 != 1 {
		t.Fatalf("out-2 count after ack = %d, want 1", n2)
	}

	// Ack 幂等:重复确认不报错
	if err := s.Ack("out-1", ids); err != nil {
		t.Fatalf("ack idempotent: %v", err)
	}
}

func TestSaveEvictionKeepsNewest(t *testing.T) {
	s := newTestStore(t, 3)

	var batch []model.DataPoint
	for i := 0; i < 5; i++ {
		batch = append(batch, dp("d1", "p", int64(1000+i), int64(i), model.QualityGood))
	}
	if err := s.Save("out-1", batch); err != nil {
		t.Fatalf("save: %v", err)
	}

	n, _ := s.CountByOutput("out-1")
	if n != 3 {
		t.Fatalf("count = %d, want 3 (cap)", n)
	}
	items, _ := s.Peek("out-1", 10)
	if len(items) != 3 {
		t.Fatalf("peek len = %d, want 3", len(items))
	}
	// 淘汰最旧,保留最新 3 条(value=2,3,4)。JSON 数值经 float64 往返。
	for i, it := range items {
		if got := it.DP.Value.(float64); got != float64(2+i) {
			t.Fatalf("item %d value = %v, want %d (newest kept)", i, got, 2+i)
		}
	}
}

func TestDropOutput(t *testing.T) {
	s := newTestStore(t, 0)
	if err := s.Save("out-1", []model.DataPoint{dp("d", "p", 1, 1, model.QualityGood)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("out-2", []model.DataPoint{dp("d", "p", 1, 1, model.QualityGood)}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropOutput("out-1"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	n1, _ := s.CountByOutput("out-1")
	n2, _ := s.CountByOutput("out-2")
	if n1 != 0 || n2 != 1 {
		t.Fatalf("after drop out-1: n1=%d n2=%d", n1, n2)
	}
}

func TestNilValueRoundTrip(t *testing.T) {
	s := newTestStore(t, 0)
	if err := s.Save("out-1", []model.DataPoint{dp("d", "p", 1, nil, model.QualityBad)}); err != nil {
		t.Fatal(err)
	}
	items, _ := s.Peek("out-1", 10)
	if len(items) != 1 || items[0].DP.Value != nil || items[0].DP.Quality != model.QualityBad {
		t.Fatalf("nil value round-trip broken: %+v", items)
	}
}

func TestEmptySave(t *testing.T) {
	s := newTestStore(t, 0)
	if err := s.Save("out-1", nil); err != nil {
		t.Fatalf("empty save: %v", err)
	}
}
