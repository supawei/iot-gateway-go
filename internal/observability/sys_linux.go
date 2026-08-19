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

// systemMem 读 /proc/meminfo,返回总内存与可用内存(字节)。
// 可用内存用 MemAvailable(含可回收缓存)而非 MemFree,反映实际可用余量。
func systemMem() (memStat, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return memStat{}, false
	}
	return parseMemInfo(data)
}

// parseMemInfo 从 /proc/meminfo 内容解析总内存与可用内存(kB→字节)。
func parseMemInfo(data []byte) (memStat, bool) {
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
		return memStat{}, false
	}
	return memStat{total: total * 1024, avail: avail * 1024}, true
}

// systemMemUsedPercent 读 /proc/meminfo,返回内存占用率 ((total-avail)/total×100)。
func systemMemUsedPercent() (float64, bool) {
	ms, ok := systemMem()
	if !ok {
		return 0, false
	}
	return memUsedPercentOf(ms), true
}

// memUsedPercent 从 /proc/meminfo 内容算系统内存占用率。
func memUsedPercent(data []byte) (float64, bool) {
	ms, ok := parseMemInfo(data)
	if !ok {
		return 0, false
	}
	return memUsedPercentOf(ms), true
}

// diskStats 对各路径所在文件系统返回总容量与可用容量(字节)。
// 可用容量取 Bavail(非 root 可用块,运维视角,与 used% 口径一致)。
// dataPath 必查;logPath 非空则并查(同分区则两条同值,可接受且基数极低)。
func diskStats(paths ...string) map[string]diskStat {
	out := make(map[string]diskStat)
	var stat syscall.Statfs_t
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := syscall.Statfs(p, &stat); err != nil {
			continue
		}
		out[p] = fsDiskStat(&stat)
	}
	return out
}

func fsDiskStat(stat *syscall.Statfs_t) diskStat {
	return diskStat{
		total: int64(stat.Blocks) * stat.Bsize,
		free:  int64(stat.Bavail) * stat.Bsize,
	}
}

// diskUsedPercent 对各路径所在文件系统返回 used% ((total-free)/total×100)。
func diskUsedPercent(paths ...string) map[string]float64 {
	out := make(map[string]float64)
	for p, s := range diskStats(paths...) {
		out[p] = diskUsedPercentOf(s)
	}
	return out
}
