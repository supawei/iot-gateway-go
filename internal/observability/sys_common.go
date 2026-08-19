package observability

// memStat 系统内存快照(字节)。sys_linux.go 从 /proc/meminfo 填充,非 Linux 平台返回零值 + ok=false。
type memStat struct {
	total int64 // MemTotal
	avail int64 // MemAvailable
}

// diskStat 单个文件系统容量快照(字节)。sys_linux.go 从 syscall.Statfs 填充。
type diskStat struct {
	total int64 // 总容量 Blocks×Bsize
	free  int64 // 非 root 可用容量 Bavail×Bsize
}

// memUsedPercentOf 按 MemAvailable 口径算内存占用率 (total-avail)/total×100。
func memUsedPercentOf(ms memStat) float64 {
	if ms.total <= 0 {
		return 0
	}
	return float64(ms.total-ms.avail) / float64(ms.total) * 100
}

// diskUsedPercentOf 按非 root 可用容量口径算磁盘占用率 (total-free)/total×100。
func diskUsedPercentOf(s diskStat) float64 {
	if s.total <= 0 {
		return 0
	}
	return float64(s.total-s.free) / float64(s.total) * 100
}
