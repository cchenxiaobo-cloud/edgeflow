// Package config 提供轻量级配置加载能力（零第三方依赖，纯标准库）。
//
// 当前支持 cloudcore 的监听端口配置，加载优先级（从高到低）：
//
//	命令行 --port > 环境变量 EDGEFLOW_CLOUDCORE_PORT > 配置文件 > 默认值 8080
//
// 配置文件为 JSON 格式，默认路径 config/cloudcore.json（可用 --config 覆盖），
// 例如：
//
//	{"port": 8080, "hubPort": 10000, "compress": true}
//
// compress 为云边通道 gzip 压缩开关（WBS 4.4），缺省 true（默认开启）；
// 显式 false 关闭（边缘经协商自动回落明文，旧版本互操作不受影响）。
//
// 文件不存在时静默使用默认值；文件存在但内容无法解析时返回错误，
// 避免用户配置写错却毫不知情地跑在错误端口上。
//
// 热重载（WBS 2.7）：LoadReload 提供重载语义的加载（配置文件缺失视为错误，
// 保持旧配置），配合 reload.go 的 Reloader 实现 SIGHUP / 定时 mtime 热重载。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
)

// 默认值与常量定义。
const (
	// DefaultPort 是未指定任何配置时的默认监听端口。
	DefaultPort = 8080
	// DefaultHubPort 是 CloudHub（云边 WebSocket 通道）的默认监听端口。
	// 与 cloud/pkg/cloudhub.DefaultHubPort 同值（跨层常量，pkg 不反向依赖 cloud 包）。
	DefaultHubPort = 10000
	// DefaultPath 是默认配置文件路径，可用 --config 覆盖。
	DefaultPath = "config/cloudcore.json"
	// PortEnvVar 是用于覆盖监听端口的环境变量名。
	PortEnvVar = "EDGEFLOW_CLOUDCORE_PORT"
	// HubPortEnvVar 是用于覆盖 CloudHub 监听端口的环境变量名。
	// 与 cloud/pkg/cloudhub.EnvHubPort 同值（跨层常量，pkg 不反向依赖 cloud 包）。
	// 0 表示随机可用端口（测试用）。
	HubPortEnvVar = "EDGEFLOW_CLOUDCORE_HUB_PORT"
)

// PortSource 描述端口值的最终来源，用于启动时打印生效配置。
type PortSource string

const (
	// SourceDefault 表示端口来自内置默认值。
	SourceDefault PortSource = "默认值"
	// SourceFile 表示端口来自配置文件。
	SourceFile PortSource = "配置文件"
	// SourceEnv 表示端口来自环境变量 EDGEFLOW_CLOUDCORE_PORT。
	SourceEnv PortSource = "环境变量 EDGEFLOW_CLOUDCORE_PORT"
	// SourceHubEnv 表示 CloudHub 端口来自环境变量 EDGEFLOW_CLOUDCORE_HUB_PORT。
	SourceHubEnv PortSource = "环境变量 EDGEFLOW_CLOUDCORE_HUB_PORT"
	// SourceFlag 表示端口来自命令行 --port。
	SourceFlag PortSource = "命令行 --port"
)

// Config 是 cloudcore 的运行时配置。
type Config struct {
	// Port 是 HTTP 服务监听端口（/healthz、/api/v1/*、/metrics）。
	Port int
	// PortSource 记录端口最终来源，便于启动日志展示生效配置。
	PortSource PortSource
	// HubPort 是 CloudHub（云边 WebSocket 通道）监听端口。
	HubPort int
	// HubPortSource 记录 CloudHub 端口最终来源。
	HubPortSource PortSource
	// Compress 是云边通道 gzip 压缩开关（WBS 4.4）：默认开启（true）。
	// 关闭后云端不向边缘确认压缩能力，边缘经协商自动回落明文——
	// 单开关控制双向，与旧版本（v1.0）互操作不受影响。
	// 变更需重启生效（热重载时保持旧值，与 hubPort 同策略）。
	Compress bool
}

// fileConfig 对应配置文件（config/cloudcore.json）的磁盘格式。
type fileConfig struct {
	// Port 是 HTTP 监听端口（必填：文件存在但未声明 port 视为配置错误）。
	Port int `json:"port"`
	// HubPort 是 CloudHub 监听端口（可选：缺省回落环境变量/默认值；0 表示随机端口）。
	HubPort *int `json:"hubPort"`
	// Compress 是云边通道压缩开关（可选：缺省 true，即默认开启）。
	// 指针区分「未声明」与「显式 false」：未声明回落默认值 true。
	Compress *bool `json:"compress"`
}

// Load 按优先级加载配置：命令行 --port > 环境变量 > 配置文件 > 默认值。
//
// 参数：
//   - filePath: 配置文件路径（--config 的值，空串表示使用 DefaultPath）
//   - flagPort: --port 的参数值（未显式指定时传 0，会被忽略）
//   - flagSet:  --port 是否被显式指定
//
// 任一来源提供非法端口（不在 1-65535 范围内）都会返回错误；
// 配置文件不存在时静默回落到默认值，存在但解析失败时返回错误。
// CloudHub 端口（HubPort）同构解析：环境变量 EDGEFLOW_CLOUDCORE_HUB_PORT
// > 配置文件 hubPort > 默认值 10000（0 表示随机端口，仅测试用）。
func Load(filePath string, flagPort int, flagSet bool) (*Config, error) {
	return load(filePath, flagPort, flagSet, false)
}

// LoadReload 是热重载专用的加载入口：优先级与 Load 完全一致，
// 唯一区别是配置文件不存在时返回错误（重载语义：文件缺失视为失败，
// 保持旧配置继续运行，而不是静默回落到默认值——见 docs/ARCHITECTURE.md §5.2）。
func LoadReload(filePath string, flagPort int, flagSet bool) (*Config, error) {
	return load(filePath, flagPort, flagSet, true)
}

func load(filePath string, flagPort int, flagSet bool, missingIsError bool) (*Config, error) {
	cfg := &Config{Port: DefaultPort, PortSource: SourceDefault, HubPort: DefaultHubPort,
		HubPortSource: SourceDefault, Compress: true} // 压缩默认开启（WBS 4.4）

	// 第 1 优先级：命令行 --port（显式指定后不再看其他来源）
	if flagSet {
		if err := validatePort(flagPort); err != nil {
			return nil, fmt.Errorf("--port 非法: %w", err)
		}
		cfg.Port = flagPort
		cfg.PortSource = SourceFlag
		if err := resolveHubPortFromEnv(cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	// 第 2 优先级：环境变量 EDGEFLOW_CLOUDCORE_PORT
	if env := os.Getenv(PortEnvVar); env != "" {
		v, err := strconv.Atoi(env)
		if err != nil {
			return nil, fmt.Errorf("环境变量 %s 的值 %q 不是合法端口: %w", PortEnvVar, env, err)
		}
		if err := validatePort(v); err != nil {
			return nil, fmt.Errorf("环境变量 %s 的值 %d 非法: %w", PortEnvVar, v, err)
		}
		cfg.Port = v
		cfg.PortSource = SourceEnv
		if err := resolveHubPortFromEnv(cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	// 第 3 优先级：配置文件（不存在时保持默认值，不报错；重载语义下缺失即错误）
	if filePath == "" {
		filePath = DefaultPath
	}
	port, hubPort, compress, err := loadFile(filePath, missingIsError)
	if err != nil {
		return nil, err
	}
	if port != 0 {
		cfg.Port = port
		cfg.PortSource = SourceFile
	}
	if hubPort != nil {
		cfg.HubPort = *hubPort
		cfg.HubPortSource = SourceFile
	}
	if compress != nil {
		cfg.Compress = *compress
	}

	// CloudHub 端口：环境变量 EDGEFLOW_CLOUDCORE_HUB_PORT 覆盖文件/默认值
	if err := resolveHubPortFromEnv(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolveHubPortFromEnv 应用环境变量 EDGEFLOW_CLOUDCORE_HUB_PORT 覆盖
// （0-65535，0 表示随机端口）；未设置时保持 cfg 当前值不变。
func resolveHubPortFromEnv(cfg *Config) error {
	v := os.Getenv(HubPortEnvVar)
	if v == "" {
		return nil
	}
	p, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("环境变量 %s 的值 %q 不是合法端口", HubPortEnvVar, v)
	}
	if p < 0 || p > 65535 {
		return fmt.Errorf("环境变量 %s 的值 %q 超出端口范围 0-65535", HubPortEnvVar, v)
	}
	cfg.HubPort = p
	cfg.HubPortSource = SourceHubEnv
	return nil
}

// loadFile 读取并解析配置文件，返回其中声明的 port、hubPort 与 compress。
// 文件不存在时返回 (0, nil, nil, nil)（表示"未提供配置"，调用方使用默认值）；
// missingIsError=true 时文件不存在视为错误（热重载语义）。
// 解析失败/字段非法时返回错误。
func loadFile(filePath string, missingIsError bool) (int, *int, *bool, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if missingIsError {
				return 0, nil, nil, fmt.Errorf("配置文件 %s 不存在（热重载失败，保持旧配置）", filePath)
			}
			return 0, nil, nil, nil
		}
		return 0, nil, nil, fmt.Errorf("读取配置文件 %s 失败: %w", filePath, err)
	}
	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return 0, nil, nil, fmt.Errorf("解析配置文件 %s 失败（请检查 JSON 格式与字段类型）: %w", filePath, err)
	}
	if err := validatePort(fc.Port); err != nil {
		return 0, nil, nil, fmt.Errorf("配置文件 %s 中的端口非法: %w", filePath, err)
	}
	if fc.HubPort != nil && (*fc.HubPort < 0 || *fc.HubPort > 65535) {
		return 0, nil, nil, fmt.Errorf("配置文件 %s 中的 hubPort 非法（范围 0-65535，0 表示随机端口）: %d", filePath, *fc.HubPort)
	}
	return fc.Port, fc.HubPort, fc.Compress, nil
}

// validatePort 校验端口是否在合法范围 1-65535 内。
func validatePort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("端口 %d 不在合法范围 1-65535 内", p)
	}
	return nil
}
