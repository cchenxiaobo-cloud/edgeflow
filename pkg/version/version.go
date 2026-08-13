// Package version 提供版本信息。
//
// Version / GitCommit / BuildTime 三个变量在编译时通过 -ldflags 注入，
// 未注入时使用默认值（适用于本地 go run 场景）。
// 注入示例：
//
//	go build -ldflags "-X edgeflow/pkg/version.Version=v0.1.0 \
//	 -X edgeflow/pkg/version.GitCommit=abc123 \
//	 -X edgeflow/pkg/version.BuildTime=2026-08-13T09:00:00+08:00" ./cmd/cloudcore
package version

import (
	"fmt"
	"runtime"
)

// 以下变量会被 Makefile 中的 -ldflags 覆盖，不要在这里写死具体版本号。
var (
	// Version 是版本号，默认 dev。
	Version = "dev"
	// GitCommit 是构建对应的 Git 提交号，默认 unknown。
	GitCommit = "unknown"
	// BuildTime 是构建时间，默认 unknown。
	BuildTime = "unknown"
)

// Info 是版本信息的结构化表示，同时用于 JSON 序列化输出。
type Info struct {
	Version   string `json:"version"`
	GitCommit string `json:"gitCommit"`
	BuildTime string `json:"buildTime"`
	GoVersion string `json:"goVersion"`
}

// Get 返回当前程序的版本信息。
func Get() Info {
	return Info{
		Version:   Version,
		GitCommit: GitCommit,
		BuildTime: BuildTime,
		GoVersion: runtime.Version(),
	}
}

// String 返回适合打印的单行版本文本，如：
// version=v0.1.0 gitCommit=abc123 buildTime=2026-08-13T09:00:00+08:00 goVersion=go1.26.2
func (i Info) String() string {
	return fmt.Sprintf("version=%s gitCommit=%s buildTime=%s goVersion=%s",
		i.Version, i.GitCommit, i.BuildTime, i.GoVersion)
}
