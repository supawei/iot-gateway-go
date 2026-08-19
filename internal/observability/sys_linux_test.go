//go:build linux

package observability

import (
	"math"
	"testing"
)

func TestParseProcRSS(t *testing.T) {
	cases := []struct {
		name string
		data string
		want int64
		ok   bool
	}{
		{"normal", "VmRSS:\t12345 kB\n", 12345 * 1024, true},
		{"missing", "MemTotal: 100 kB\n", 0, false},
		{"non-numeric", "VmRSS: abc kB\n", 0, false},
		{"empty", "", 0, false},
	}
	for _, c := range cases {
		got, ok := parseProcRSS([]byte(c.data))
		if ok != c.ok || got != c.want {
			t.Fatalf("%s: parseProcRSS=%d(ok=%v) want %d(ok=%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestMemUsedPercent(t *testing.T) {
	data := []byte("MemTotal: 1000 kB\nMemFree: 500 kB\nMemAvailable: 300 kB\n")
	got, ok := memUsedPercent(data)
	if !ok {
		t.Fatal("expected ok")
	}
	want := float64(1000-300) / 1000 * 100 // 70
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("memUsedPercent=%v want %v", got, want)
	}
	// 无 MemTotal -> ok=false
	if _, ok := memUsedPercent([]byte("MemFree: 500 kB\n")); ok {
		t.Fatal("expected ok=false when MemTotal missing")
	}
}

func TestDiskUsedPercent(t *testing.T) {
	dir := t.TempDir()
	m := diskUsedPercent(dir)
	v, ok := m[dir]
	if !ok {
		t.Fatalf("missing disk entry for %s", dir)
	}
	if v < 0 || v > 100 {
		t.Fatalf("disk used percent=%v out of [0,100]", v)
	}
	// 空路径与不存在路径被跳过
	if got := diskUsedPercent(""); len(got) != 0 {
		t.Fatalf("empty path should be skipped, got %v", got)
	}
	if got := diskUsedPercent("/no/such/path/iot-gateway-test-xyz"); len(got) != 0 {
		t.Fatalf("non-existent path should be skipped, got %v", got)
	}
}
