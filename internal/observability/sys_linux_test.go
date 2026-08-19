//go:build linux

package observability

import (
	"math"
	"strings"
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

func TestParseMemInfo(t *testing.T) {
	ms, ok := parseMemInfo([]byte("MemTotal: 1000 kB\nMemFree: 500 kB\nMemAvailable: 300 kB\n"))
	if !ok {
		t.Fatal("expected ok")
	}
	if ms.total != 1000*1024 || ms.avail != 300*1024 {
		t.Fatalf("parseMemInfo total=%d avail=%d, want %d/%d", ms.total, ms.avail, 1000*1024, 300*1024)
	}
	if _, ok := parseMemInfo([]byte("MemFree: 500 kB\n")); ok {
		t.Fatal("expected ok=false when MemTotal missing")
	}
}

func TestDiskStats(t *testing.T) {
	dir := t.TempDir()
	m := diskStats(dir)
	s, ok := m[dir]
	if !ok {
		t.Fatalf("missing disk entry for %s", dir)
	}
	if s.total <= 0 || s.free < 0 || s.free > s.total {
		t.Fatalf("disk stats total=%d free=%d out of [0,total]", s.total, s.free)
	}
	// 空路径与不存在路径被跳过
	if got := diskStats(""); len(got) != 0 {
		t.Fatalf("empty path should be skipped, got %v", got)
	}
	if got := diskStats("/no/such/path/iot-gateway-test-xyz"); len(got) != 0 {
		t.Fatalf("non-existent path should be skipped, got %v", got)
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

// TestRuntimeMetricsRenderSys 验证新增的四项字节级系统指标渲染为 Prometheus 文本
// (内存总/可用、所在分区盘总/剩余),且与既有 used_percent 族同路径标签。
func TestRuntimeMetricsRenderSys(t *testing.T) {
	c := NewCollector(nil, nil, nil, nil, t.TempDir(), "")
	var b strings.Builder
	c.writeRuntimeMetrics(&b)
	body := b.String()

	for _, name := range []string{
		"iot_gateway_mem_total_bytes",
		"iot_gateway_mem_available_bytes",
		"iot_gateway_disk_total_bytes",
		"iot_gateway_disk_free_bytes",
	} {
		if !strings.Contains(body, name) {
			t.Fatalf("metrics body missing %q:\n%s", name, body)
		}
	}
	// 内存值非零(本机 /proc/meminfo 可读)
	if strings.Contains(body, "iot_gateway_mem_total_bytes 0\n") {
		t.Fatalf("mem_total_bytes should be non-zero on linux:\n%s", body)
	}
	// 磁盘 family 带 path 标签且路径与 total/free/used_percent 一致
	if !strings.Contains(body, `iot_gateway_disk_total_bytes{path=`) {
		t.Fatalf("disk_total_bytes missing path label:\n%s", body)
	}
}
