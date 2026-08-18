// Package version 集中管理网关版本信息。
//
// 编译期通过 -ldflags "-X" 注入真实值(见 Makefile 的 VERSION_LDFLAGS);
// 未注入时保持占位值,便于本地 go run / go test。
package version

import (
	"fmt"
	"runtime"
)

// 由构建脚本注入,例如:
//
//	-X iot-gateway-go/internal/version.Version=1.2.0
//	-X iot-gateway-go/internal/version.Commit=abe81fe
//	-X iot-gateway-go/internal/version.BuildTime=2025-08-18T08:00:00Z
var (
	// Version 语义化版本号;无 git tag 时回退到 "dev"。
	Version = "dev"
	// Commit 构建时的 git 短提交哈希。
	Commit = "none"
	// BuildTime 构建时间(UTC RFC3339)。
	BuildTime = "unknown"
)

// GoVersion 编译所用 Go 版本,由 runtime 提供,无需注入。
var GoVersion = runtime.Version()

// Info 是版本信息的结构化表示,供 REST API / Web UI 展示。
type Info struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
	GoVersion string `json:"goVersion"`
}

// Get 返回当前构建的版本信息。
func Get() Info {
	return Info{
		Name:      "iot-gateway-go",
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
		GoVersion: GoVersion,
	}
}

// String 返回单行版本描述,供 -v/--version 与 -h 帮助输出使用。
func String() string {
	info := Get()
	return fmt.Sprintf("%s %s (commit %s, built %s, %s)",
		info.Name, info.Version, info.Commit, info.BuildTime, info.GoVersion)
}
