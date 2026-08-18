package version

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	s := String()
	// 格式: "iot-gateway-go <version> (commit <c>, built <b>, <go>)"
	if !strings.HasPrefix(s, "iot-gateway-go ") {
		t.Fatalf("String() 缺少程序名前缀: %q", s)
	}
	if !strings.Contains(s, "commit "+Commit) {
		t.Errorf("String() 未包含 commit 信息: %q", s)
	}
	if !strings.Contains(s, "built "+BuildTime) {
		t.Errorf("String() 未包含 built 信息: %q", s)
	}
	if !strings.Contains(s, GoVersion) {
		t.Errorf("String() 未包含 Go 版本: %q", s)
	}
}

func TestDefaults(t *testing.T) {
	// 未经 -ldflags 注入时,占位值应保持非空,保证任何环境下均可显示。
	if Version == "" || Commit == "" || BuildTime == "" || GoVersion == "" {
		t.Fatalf("版本占位值不应为空: version=%q commit=%q built=%q go=%q",
			Version, Commit, BuildTime, GoVersion)
	}
}
