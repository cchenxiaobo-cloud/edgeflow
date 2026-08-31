// v0.26.0 配置文件支持：EDGEFLOW_MQTT_CONFIG 指向 JSON / YAML
// （逐行 parser，仅支持扁平 key: value 形式）配置文件，为 New() 的字段
// 回退链增加「With 选项 > 环境变量 > 配置文件 > 默认值」中的配置文件层。
//
// 设计要点：
//   - fileValues 是扁平 string→string 键值表；数值/布尔字段（如
//     keep_alive_seconds、tls_insecure）由调用方按需转换，加载层不做
//     类型解释；
//   - YAML 路径是刻意最小化的逐行 parser：支持 "key: value"（冒号后
//     必须有空格或行尾）与可选的引号包裹值；不支持嵌套、锚点、多文档
//     与块标量——EdgeFlow 的 MQTT 配置面就是一张扁平表；
//   - .json 走 encoding/json 解 map[string]string（数字/布尔值会被
//     encoding/json 拒绝，属预期：字段值统一按字符串书写）；
//   - 不支持的扩展名与文件不存在均返回错误（New() 记日志后忽略文件
//     继续装配，属软失败语义）。
package mqtt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fileValues 是配置文件的扁平键值表（key 一律小写化；value 去首尾
// 空白与可选成对引号）。键面（v0.26.0）：
//
//	broker / topics / device_name / namespace / cmd_topic /
//	keep_alive_seconds / tls_ca_path / tls_insecure / config_path
//
// config_path 仅作键面保留（未来外层引用），当前无行为。
type fileValues map[string]string

// loadConfigFile 按扩展名加载配置文件：.yaml/.yml 用最小逐行 parser，
// .json 用 encoding/json，其他扩展名报错。
func loadConfigFile(path string) (fileValues, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("配置文件不存在: %s", path)
		}
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return parseYAMLValues(string(data)), nil
	case ".json":
		return parseJSONValues(data)
	default:
		return nil, fmt.Errorf("不支持的配置文件类型: %s（仅支持 .yaml/.yml/.json）", filepath.Ext(path))
	}
}

// parseYAMLValues 逐行解析扁平 "key: value" 文本：跳过空行与 # 注释
// 行；首个冒号切分 key/value（value 去引号）；key 小写化。无冒号的
// 非空行视为格式错误行，跳过（软失败：不因个别坏行拒绝整份文件）。
func parseYAMLValues(text string) fileValues {
	fv := make(fileValues)
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue // 无冒号的坏行：跳过（软失败）
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		fv[key] = unquoteValue(strings.TrimSpace(value))
	}
	return fv
}

// unquoteValue 去除值两侧的一对成对引号（' 或 "；只剥一层，且要求
// 首尾同为引号才剥，避免把 it's 这类值剥坏）。
func unquoteValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// parseJSONValues 解析 JSON 配置为 fileValues。仅接受字符串值的扁平
// 对象（数字/布尔/嵌套结构与 encoding/json 解 map[string]string 的
// 语义冲突，直接报错——比静默丢字段安全）。
func parseJSONValues(data []byte) (fileValues, error) {
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("JSON 配置解析失败: %w", err)
	}
	fv := make(fileValues, len(raw))
	for k, v := range raw {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		fv[k] = strings.TrimSpace(v)
	}
	return fv, nil
}
