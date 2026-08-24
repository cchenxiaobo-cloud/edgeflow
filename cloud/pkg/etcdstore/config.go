// Package etcdstore 提供嵌入式 etcd（go.etcd.io/etcd/server/v3/embed）基础层：
// 配置解析、embed 生命周期（Start/Close/就绪等待）、clientv3 KV 薄封装。
//
// 本包是 EdgeFlow v0.4.0 云端存储改造（内存态 → 嵌入式 etcd 写穿）的基础设施层，
// 被 registry / devicestatus 的 etcd 包装实现与 cmd/cloudcore 装配区消费。
// 设计依据：.cluster/edgeflow-v040/subagent_02.md（§4 配置表、§6 关停与坏库降级、
// §9 实施顺序第 2 步）与 subagent_03.md 前置条件（quota≤256MB、auto-compaction、
// 127.0.0.1 绑定、优雅关闭、kill -9 冒烟）。
package etcdstore

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

// 环境变量名（与 cmd/cloudcore 装配约定一致，统一 EDGEFLOW_CLOUDCORE_ 前缀，
// 风格对齐 nodecontroller.DurationsFromEnv：非法值 fail-fast 报错，不静默回退）。
const (
	// EnvEnabled 总开关：false = 完全退回 v0.3.x 纯内存行为（不建目录、不占端口、不写盘）。
	EnvEnabled = "EDGEFLOW_CLOUDCORE_ETCD_ENABLED"
	// EnvDataDir embed 数据目录（相对工作目录，自动 MkdirAll）。
	EnvDataDir = "EDGEFLOW_CLOUDCORE_ETCD_DATA_DIR"
	// EnvClientURL 客户端监听（只绑 127.0.0.1，安全底线）。
	EnvClientURL = "EDGEFLOW_CLOUDCORE_ETCD_CLIENT_URL"
	// EnvPeerURL peer 监听（单成员，仅用于 embed 内部）。
	EnvPeerURL = "EDGEFLOW_CLOUDCORE_ETCD_PEER_URL"
	// EnvQuotaBackendBytes 后端配额（默认 256MiB，见设计 §4）。
	EnvQuotaBackendBytes = "EDGEFLOW_CLOUDCORE_ETCD_QUOTA_BACKEND_BYTES"
	// EnvAutoCompactionMode 自动压缩模式：periodic / revision。
	EnvAutoCompactionMode = "EDGEFLOW_CLOUDCORE_ETCD_AUTO_COMPACTION_MODE"
	// EnvAutoCompactionRetention 压缩保留期（periodic 为时长，支持 "1h" 或秒数）。
	EnvAutoCompactionRetention = "EDGEFLOW_CLOUDCORE_ETCD_AUTO_COMPACTION_RETENTION"
	// EnvStrict 严格模式：空=off（启动失败降级纯内存 + 告警），'1'=fail-fast（拒绝启动）。
	EnvStrict = "EDGEFLOW_CLOUDCORE_ETCD_STRICT"
)

// 默认配置（对齐设计 §4 配置表；端口选 12379/12380 避开标准 2379/2380，
// 与开发机既有真实 etcd 部署共存不冲突）。
const (
	DefaultEnabled                 = true
	DefaultDataDir                 = "data/etcd"
	DefaultClientURL               = "http://127.0.0.1:12379"
	DefaultPeerURL                 = "http://127.0.0.1:12380"
	DefaultQuotaBackendBytes       = 256 * 1024 * 1024 // 256MiB（设计约束：≤256MB）
	DefaultAutoCompactionMode      = "periodic"
	DefaultAutoCompactionRetention = 1 * time.Hour
)

// Config 是 etcdstore 基础层配置（由 ConfigFromEnv 从环境变量解析）。
type Config struct {
	// Enabled 总开关；false = 纯内存（v0.3.x 行为），不创建目录、不占端口。
	Enabled bool
	// DataDir embed 数据目录（启动时 MkdirAll 自动创建）。
	DataDir string
	// ClientURL 客户端监听地址（只允许回环）。
	ClientURL string
	// PeerURL peer 监听地址（单成员，只允许回环）。
	PeerURL string
	// QuotaBackendBytes 后端配额（字节）。
	QuotaBackendBytes int64
	// AutoCompactionMode 自动压缩模式：periodic / revision。
	AutoCompactionMode string
	// AutoCompactionRetention 压缩保留期（periodic 模式为时长）。
	AutoCompactionRetention time.Duration
	// Strict 严格模式：true = embed 启动失败即拒绝启动（fail-fast）；
	// false（默认）= 降级纯内存 + 告警（由装配区消费，见设计 §6.5）。
	Strict bool
}

// DefaultConfig 返回默认配置（设计 §4 配置表的默认值全集）。
func DefaultConfig() Config {
	return Config{
		Enabled:                 DefaultEnabled,
		DataDir:                 DefaultDataDir,
		ClientURL:               DefaultClientURL,
		PeerURL:                 DefaultPeerURL,
		QuotaBackendBytes:       DefaultQuotaBackendBytes,
		AutoCompactionMode:      DefaultAutoCompactionMode,
		AutoCompactionRetention: DefaultAutoCompactionRetention,
	}
}

// ConfigFromEnv 从环境变量解析配置（未设置用默认值；非法值返回错误，
// 风格对齐 nodecontroller.DurationsFromEnv：装配期 fail-fast，不静默回退）。
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv(EnvEnabled); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 不是合法布尔值（true/false/1/0）", EnvEnabled, v)
		}
		cfg.Enabled = b
	}
	if v := os.Getenv(EnvDataDir); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv(EnvClientURL); v != "" {
		cfg.ClientURL = v
	}
	if v := os.Getenv(EnvPeerURL); v != "" {
		cfg.PeerURL = v
	}
	if v := os.Getenv(EnvQuotaBackendBytes); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 必须为正整数（字节）", EnvQuotaBackendBytes, v)
		}
		cfg.QuotaBackendBytes = n
	}
	if v := os.Getenv(EnvAutoCompactionMode); v != "" {
		if v != "periodic" && v != "revision" {
			return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 必须是 periodic 或 revision", EnvAutoCompactionMode, v)
		}
		cfg.AutoCompactionMode = v
	}
	if v := os.Getenv(EnvAutoCompactionRetention); v != "" {
		d, err := parseDurationEnv(EnvAutoCompactionRetention, v)
		if err != nil {
			return Config{}, err
		}
		cfg.AutoCompactionRetention = d
	}
	if v := os.Getenv(EnvStrict); v != "" {
		switch v {
		case "1":
			cfg.Strict = true
		case "", "0":
			cfg.Strict = false
		default:
			return Config{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 只支持空/'0'（off）或 '1'（fail-fast）", EnvStrict, v)
		}
	}

	if _, err := parseListenURL(EnvClientURL, cfg.ClientURL); err != nil {
		return Config{}, err
	}
	if _, err := parseListenURL(EnvPeerURL, cfg.PeerURL); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseDurationEnv 解析时长环境变量：优先 Go duration（"1h"、"90s"），
// 回退纯秒数（"90"）；非法或非正值返回错误（对齐 nodecontroller 约定）。
func parseDurationEnv(name, v string) (time.Duration, error) {
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("etcdstore: 环境变量 %s=%q 必须为正时长", name, v)
		}
		return d, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("etcdstore: 环境变量 %s=%q 不是合法时长（支持 Go duration 如 \"1h\" 或秒数）", name, v)
}

// parseListenURL 解析并校验监听地址：只允许 http 协议 + 回环地址
// （127.0.0.1 / localhost / ::1）。安全底线：embed 不暴露到非回环。
func parseListenURL(envName, raw string) (url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return url.URL{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 不是合法 URL: %v", envName, raw, err)
	}
	if u.Scheme != "http" {
		return url.URL{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 必须使用 http 协议（当前 %q）", envName, raw, u.Scheme)
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return url.URL{}, fmt.Errorf("etcdstore: 环境变量 %s=%q 只允许绑定回环地址（127.0.0.1/localhost/::1），当前 %q（安全底线：不暴露到非回环）", envName, raw, host)
	}
	return *u, nil
}
