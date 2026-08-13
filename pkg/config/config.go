// Package config 提供轻量级配置加载能力（零第三方依赖，纯标准库）。
//
// 当前支持 cloudcore 的监听端口配置，加载优先级（从高到低）：
//
//	命令行 --port > 环境变量 EDGEFLOW_CLOUDCORE_PORT > 配置文件 > 默认值 8080
//
// 配置文件为 JSON 格式，默认路径 config/cloudcore.json（可用 --config 覆盖），
// 例如：
//
//	{"port": 8080}
//
// 文件不存在时静默使用默认值；文件存在但内容无法解析时返回错误，
// 避免用户配置写错却毫不知情地跑在错误端口上。
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
	// DefaultPath 是默认配置文件路径，可用 --config 覆盖。
	DefaultPath = "config/cloudcore.json"
	// PortEnvVar 是用于覆盖监听端口的环境变量名。
	PortEnvVar = "EDGEFLOW_CLOUDCORE_PORT"
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
	// SourceFlag 表示端口来自命令行 --port。
	SourceFlag PortSource = "命令行 --port"
)

// Config 是 cloudcore 的运行时配置。
type Config struct {
	// Port 是 HTTP 服务监听端口。
	Port int
	// PortSource 记录端口最终来源，便于启动日志展示生效配置。
	PortSource PortSource
}

// fileConfig 对应配置文件（config/cloudcore.json）的磁盘格式。
type fileConfig struct {
	Port int `json:"port"`
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
func Load(filePath string, flagPort int, flagSet bool) (*Config, error) {
	cfg := &Config{Port: DefaultPort, PortSource: SourceDefault}

	// 第 1 优先级：命令行 --port（显式指定后不再看其他来源）
	if flagSet {
		if err := validatePort(flagPort); err != nil {
			return nil, fmt.Errorf("--port 非法: %w", err)
		}
		cfg.Port = flagPort
		cfg.PortSource = SourceFlag
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
		return cfg, nil
	}

	// 第 3 优先级：配置文件（不存在时保持默认值，不报错）
	if filePath == "" {
		filePath = DefaultPath
	}
	port, err := loadFile(filePath)
	if err != nil {
		return nil, err
	}
	if port != 0 {
		cfg.Port = port
		cfg.PortSource = SourceFile
	}
	return cfg, nil
}

// loadFile 读取并解析配置文件，返回其中声明的端口。
// 文件不存在时返回 0（表示"未提供端口"），解析失败时返回错误。
func loadFile(filePath string) (int, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("读取配置文件 %s 失败: %w", filePath, err)
	}
	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return 0, fmt.Errorf("解析配置文件 %s 失败（请检查 JSON 格式与字段类型）: %w", filePath, err)
	}
	if err := validatePort(fc.Port); err != nil {
		return 0, fmt.Errorf("配置文件 %s 中的端口非法: %w", filePath, err)
	}
	return fc.Port, nil
}

// validatePort 校验端口是否在合法范围 1-65535 内。
func validatePort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("端口 %d 不在合法范围 1-65535 内", p)
	}
	return nil
}
