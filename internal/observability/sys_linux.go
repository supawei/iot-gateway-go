//go:build linux

package observability

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processRSS 读 /proc/self/status 的 VmRSS(KB)转字节(Linux 专属)。
func processRSS() (int64, bool) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	return parseProcRSS(data)
}

// parseProcRSS 从 /proc/self/status 内容解析 VmRSS 字节数。
func parseProcRSS(data []byte) (int64, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line) // ["VmRSS:", "12345", "kB"]
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// systemMemUsedPercent 读 /proc/meminfo,返回 (MemTotal - MemAvailable) / MemTotal * 100。
// 用 MemAvailable(含可回收缓存)而非 MemFree,反映实际可用压力。
func systemMemUsedPercent() (float64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	return memUsedPercent(data)
}

// memUsedPercent 从 /proc/meminfo 内容算系统内存占用率。
func memUsedPercent(data []byte) (float64, bool) {
	var total, avail int64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, _ = strconv.ParseInt(fields[1], 10, 64)
		case "MemAvailable:":
			avail, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}
	if total <= 0 {
		return 0, false
	}
	return float64(total-avail) / float64(total) * 100, true
}

// diskUsedPercent 对各路径所在文件系统返回 used%。
// used% = (Blocks - Bavail) / Blocks * 100,Bavail 为非 root 可用块(运维视角)。
// dataPath 必查;logPath 非空则并查(同分区则两条同值,可接受且基数极低)。
func diskUsedPercent(paths ...string) map[string]float64 {
	out := make(map[string]float64)
	var stat syscall.Statfs_t
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := syscall.Statfs(p, &stat); err != nil {
			continue
		}
		out[p] = fsUsedPercent(&stat)
	}
	return out
}

func fsUsedPercent(stat *syscall.Statfs_t) float64 {
	if stat.Blocks == 0 {
		return 0
	}
	return float64(stat.Blocks-stat.Bavail) / float64(stat.Blocks) * 100
}
