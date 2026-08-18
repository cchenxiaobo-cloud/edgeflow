// Package resource 提供资源量的解析与格式化（WBS 6.5 资源调度）。
//
// 纯函数、零依赖，供云边两侧共用：
//   - edged：超卖率校验、docker 资源参数生成（edge/pkg/edged）
//   - cloudcore：Pod 下发前置校验（cmd/cloudcore）
//   - edgecore：PodSync 落盘前的超卖拒绝（cmd/edgecore）
//
// 格式沿用 K8s 习惯：CPU "250m"（毫核）或 "1"（核）；内存 "64Mi"（二进制）
// 或 "64M"（十进制）、纯数字（字节）。
package resource

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrResourceExhausted 是「超出节点资源」错误的前缀标记（云边契约新增）：
// 边缘超卖校验拒绝时返回以该标记开头的错误，云端 API 识别后返回 4xx
// （409 超出节点资源），而不是通用 502（边缘拒绝）。
const ErrResourceExhausted = "EDGEFLOW_RESOURCE_EXHAUSTED"

// ParseCPU 解析 CPU 资源量字符串，返回毫核（milliCPUs）。
//
// 支持格式（与 K8s 对齐）：
//   - "250m" → 250（毫核）
//   - "1" / "1.0" → 1000（1 核 = 1000m）
//   - "0.5" → 500
//   - 空字符串 → 0, nil（零值 = 不限制）
//
// 不支持格式：科学计数法（"1e3"）、负值、前导 "+"、非有限值
// （"Inf"/"NaN"/"Infinity"）、超出 int64 毫核范围的值。
func ParseCPU(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "m") {
		num := strings.TrimSuffix(s, "m")
		// 拒绝空数值（"m"）与前导 "+"（"+250m"）：strconv.ParseInt 会静默
		// 接受 "+250"，而本包格式契约不接受前导正号（负号经 v<0 拒绝）。
		if num == "" || num[0] == '+' {
			return 0, fmt.Errorf("无效的 CPU 格式 %q: 不支持前导 +", s)
		}
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("无效的 CPU 格式 %q: %w", s, err)
		}
		if v < 0 {
			return 0, fmt.Errorf("CPU 不能为负值: %q", s)
		}
		return v, nil
	}
	// 纯数字（核数）：支持小数；拒绝科学计数法（与 K8s 一致："1e3" 非法）
	if strings.ContainsAny(s, "eE") {
		return 0, fmt.Errorf("无效的 CPU 格式 %q: 不支持科学计数法", s)
	}
	// 与毫核分支一致：拒绝前导 "+"（strconv.ParseFloat 会静默接受 "+1"）。
	if s[0] == '+' {
		return 0, fmt.Errorf("无效的 CPU 格式 %q: 不支持前导 +", s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的 CPU 格式 %q: %w", s, err)
	}
	// ParseFloat 接受 "Inf"/"NaN"/"Infinity"（任意大小写、可带符号）：
	// 非有限值必须拒绝，否则后续 int64(math.Round(f*1000)) 是未定义行为。
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("无效的 CPU 格式 %q: 非有限数值", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("CPU 不能为负值: %q", s)
	}
	// f×1000 必须落在 int64 毫核范围内。安全边界用 2^63 做浮点比较：
	// float64 无法精确表示 MaxInt64（2^63-1，其最近可表示值是 2^63），
	// 因此 milli >= 2^63 一律拒绝——恰好 2^63 转 int64 同样是未定义行为。
	milli := f * 1000
	if milli >= 9223372036854775808.0 { // 2^63
		return 0, fmt.Errorf("CPU 值超出 int64 范围: %q", s)
	}
	return int64(math.Round(milli)), nil
}

// FormatCPU 把毫核格式化为 docker --cpus 参数值：
// "250m" → "0.25"，"1000" → "1"，"1500" → "1.5"。
func FormatCPU(milli int64) string {
	if milli%1000 == 0 {
		return strconv.FormatInt(milli/1000, 10)
	}
	return strconv.FormatFloat(float64(milli)/1000, 'f', -1, 64)
}

// ParseMemory 解析内存资源量字符串，返回字节数。
//
// 支持格式（与 K8s 对齐）：
//   - 纯数字 → 字节："67108864" → 67108864
//   - Ki/Mi/Gi/Ti/Pi/Ei 后缀（二进制单位，1024 为底）："64Mi" → 67108864
//   - k/M/G/T/P/E 后缀（十进制单位，1000 为底）："64M" → 64000000
//   - 空字符串 → 0, nil（零值 = 不限制）
//
// 不支持格式：分数（"0.5Gi"）、科学计数法（"1e6"）、负值。
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// 后缀映射（K8s 习惯：二进制后缀优先，长后缀先匹配）
	multipliers := map[string]int64{
		"Ki": 1 << 10,
		"Mi": 1 << 20,
		"Gi": 1 << 30,
		"Ti": 1 << 40,
		"Pi": 1 << 50,
		"Ei": 1 << 60,
		"k":  1000,
		"K":  1000,
		"M":  1000 * 1000,
		"G":  1000 * 1000 * 1000,
		"T":  1000 * 1000 * 1000 * 1000,
		"P":  1000 * 1000 * 1000 * 1000 * 1000,
		"E":  1000 * 1000 * 1000 * 1000 * 1000 * 1000,
	}

	// 按后缀长度从长到短尝试匹配（Gi 优先于 G，Ki 优先于 K）
	suffixes := make([]string, 0, len(multipliers))
	for suf := range multipliers {
		suffixes = append(suffixes, suf)
	}
	// 稳定排序：长度降序
	for i := 0; i < len(suffixes); i++ {
		for j := i + 1; j < len(suffixes); j++ {
			if len(suffixes[j]) > len(suffixes[i]) {
				suffixes[i], suffixes[j] = suffixes[j], suffixes[i]
			}
		}
	}
	for _, suffix := range suffixes {
		mult := multipliers[suffix]
		if strings.HasSuffix(s, suffix) {
			numStr := strings.TrimSuffix(s, suffix)
			v, err := strconv.ParseInt(numStr, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("无效的内存格式 %q: %w", s, err)
			}
			if v < 0 {
				return 0, fmt.Errorf("内存不能为负值: %q", s)
			}
			if v > math.MaxInt64/mult {
				return 0, fmt.Errorf("内存值溢出: %q", s)
			}
			return v * mult, nil
		}
	}

	// 纯数字 = 字节
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的内存格式 %q: %w", s, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("内存不能为负值: %q", s)
	}
	return v, nil
}

// FormatMemoryBytes 把字节数格式化为适合 docker --memory 参数的值
// （docker CLI 接受纯字节数）。67108864 → "67108864"。
func FormatMemoryBytes(bytes int64) string {
	return strconv.FormatInt(bytes, 10)
}

// FormatMemorySwapBytes 返回适合 docker --memory-swap 的值：
// 与 memory 一致 = 禁用 swap（v0.2.0 语义：swap 上限 = 内存上限）。
func FormatMemorySwapBytes(bytes int64) string {
	return strconv.FormatInt(bytes, 10)
}
