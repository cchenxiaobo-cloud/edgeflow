// Package edged — 资源调度与超卖（WBS 6.5 v0.2.0）。
//
// 本文件定义：
//   - 超卖率校验器（CheckOvercommit）：节点容量 → 已部署 request 求和 →
//     新部署超出超卖率上限则拒绝（错误带 ErrResourceExhausted 标记）
//   - NodeCapacity：节点资源总量（可配置或探测）
//   - request≤limit 校验（ValidateRequestLimit）
//   - docker 资源参数生成（DockerResourceArgs）
//
// 资源量解析（ParseCPU/ParseMemory 等）在共享包 edgeflow/pkg/resource 中，
// 云端（cloudcore 前置校验）与边缘（edged/edgecore）共用同一套实现。
package edged

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/resource"
)

// NodeCapacity 是节点的资源总量。
type NodeCapacity struct {
	CPUMilliCores int64 // CPU 毫核数（如 4000 = 4 核）
	MemoryBytes   int64 // 内存字节数
}

// DefaultOvercommitRates 返回默认超卖率（CPU 150%、内存 150%），
// 环境变量可覆盖（v0.2.0 简化策略：恒固定值，不支持热重载）：
//   - EDGEFLOW_EDGECORE_OVERCOMMIT_CPU_RATE：CPU 超卖率（>0，如 "1.5"）
//   - EDGEFLOW_EDGECORE_OVERCOMMIT_MEMORY_RATE：内存超卖率（>0）
//
// 非法值（含 strconv.ParseFloat 会静默接受的 "Inf"/"NaN" 等非有限值）
// 一律回退默认值——非有限超卖率会让上限计算变成平台相关行为。
func DefaultOvercommitRates() OvercommitRates {
	r := OvercommitRates{CPU: 1.5, Memory: 1.5}
	if v := os.Getenv("EDGEFLOW_EDGECORE_OVERCOMMIT_CPU_RATE"); v != "" {
		// math.IsFinite 在 Go 1.26 尚未提供，用等价的 NaN/Inf 显式拒绝
		// （NaN > 0 为 false 虽已回退，但显式拒绝保证语义不依赖比较顺序）。
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && !math.IsNaN(f) && !math.IsInf(f, 0) {
			r.CPU = f
		}
	}
	if v := os.Getenv("EDGEFLOW_EDGECORE_OVERCOMMIT_MEMORY_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && !math.IsNaN(f) && !math.IsInf(f, 0) {
			r.Memory = f
		}
	}
	return r
}

// OvercommitRates 是超卖率上限配置（ratio，1.5 = 150%）。
type OvercommitRates struct {
	CPU    float64
	Memory float64
}

// DetectNodeCapacity 探测本机资源总量：
//   - CPU：runtime.NumCPU() × 1000（毫核）
//   - 内存：Linux 读 /proc/meminfo 的 MemTotal（非 Linux 返回 0）
//   - 环境变量覆盖（可配置）：
//     EDGEFLOW_EDGECORE_NODE_CPU_MILLI（毫核）、
//     EDGEFLOW_EDGECORE_NODE_MEMORY_BYTES（字节）
func DetectNodeCapacity() NodeCapacity {
	c := NodeCapacity{
		CPUMilliCores: int64(runtime.NumCPU()) * 1000,
		MemoryBytes:   detectMemoryBytes(),
	}
	if v := os.Getenv("EDGEFLOW_EDGECORE_NODE_CPU_MILLI"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.CPUMilliCores = n
		}
	}
	if v := os.Getenv("EDGEFLOW_EDGECORE_NODE_MEMORY_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.MemoryBytes = n
		}
	}
	return c
}

// detectMemoryBytes 尝试从 /proc/meminfo 读取物理内存总量（字节）；
// 非 Linux（如 macOS）或读取失败返回 0——调用方应按需用环境变量覆盖。
func detectMemoryBytes() int64 {
	// 仅 Linux 支持 /proc/meminfo；其他平台返回 0（用环境变量覆盖）
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return kb * 1024 // /proc/meminfo 单位为 kB
			}
		}
	}
	return 0
}

// ValidateRequestLimit 校验资源字段：
//   - 非空字段全部做格式校验（单边畸形值也拒绝——不能因为缺少另一边就
//     fail-open；云端已前置校验，这里是边缘兜底防御，P2-m2 选方案 a）；
//   - request 与 limit 都非空时校验 request ≤ limit。
func ValidateRequestLimit(r metamanager.ResourceRequirements) error {
	// CPU
	var reqCPU, limCPU int64
	if r.CPURequest != "" {
		v, err := resource.ParseCPU(r.CPURequest)
		if err != nil {
			return fmt.Errorf("CPU request 解析失败: %w", err)
		}
		reqCPU = v
	}
	if r.CPULimit != "" {
		v, err := resource.ParseCPU(r.CPULimit)
		if err != nil {
			return fmt.Errorf("CPU limit 解析失败: %w", err)
		}
		limCPU = v
	}
	if r.CPURequest != "" && r.CPULimit != "" && reqCPU > limCPU {
		return fmt.Errorf("CPU request (%s=%dm) 超过 CPU limit (%s=%dm)",
			r.CPURequest, reqCPU, r.CPULimit, limCPU)
	}
	// Memory
	var reqMem, limMem int64
	if r.MemoryRequest != "" {
		v, err := resource.ParseMemory(r.MemoryRequest)
		if err != nil {
			return fmt.Errorf("Memory request 解析失败: %w", err)
		}
		reqMem = v
	}
	if r.MemoryLimit != "" {
		v, err := resource.ParseMemory(r.MemoryLimit)
		if err != nil {
			return fmt.Errorf("Memory limit 解析失败: %w", err)
		}
		limMem = v
	}
	if r.MemoryRequest != "" && r.MemoryLimit != "" && reqMem > limMem {
		return fmt.Errorf("Memory request (%s=%d bytes) 超过 Memory limit (%s=%d bytes)",
			r.MemoryRequest, reqMem, r.MemoryLimit, limMem)
	}
	return nil
}

// CheckOvercommit 校验新 workload 加入后是否超出节点资源超卖上限。
//
// 参数：
//   - newReq：新 workload 的 request
//   - existingCPU / existingMemory：已有 workload 的 request 总和（已解析）
//   - cap：节点容量
//   - rates：超卖率上限
//
// 返回：nil 表示允许部署；非 nil 表示超出资源上限，错误信息以
// resource.ErrResourceExhausted 为前缀（云端识别后返回 4xx）。
//
// 边界语义：总和 == 上限 → 允许（只有严格超过才拒绝）。
func CheckOvercommit(newReq metamanager.ResourceRequirements, existingCPU, existingMemory int64, cap NodeCapacity, rates OvercommitRates) error {
	newCPU, _ := resource.ParseCPU(newReq.CPURequest)
	newMem, _ := resource.ParseMemory(newReq.MemoryRequest)

	totalCPU := existingCPU + newCPU
	totalMem := existingMemory + newMem

	// 舍入口径统一为 math.Round（P1-m4）：原 int64(float64(...)) 转换是
	// 向零截断（正数即向下取整），上限最多偏小 1m，且无注释背书；
	// Round 语义明确（最近整数，.5 向远离零取整）。契约边界
	// 「总和 == 上限允许、超过才拒绝」不受影响——典型值 × 1.5 恰为整数。
	cpuLimit := int64(math.Round(float64(cap.CPUMilliCores) * rates.CPU))
	memLimit := int64(math.Round(float64(cap.MemoryBytes) * rates.Memory))

	var reasons []string
	if newCPU > 0 && cap.CPUMilliCores > 0 && totalCPU > cpuLimit {
		reasons = append(reasons, fmt.Sprintf("CPU: 请求 %dm + 已有 %dm = %dm > 上限 %dm（节点 %dm × %.0f%%）",
			newCPU, existingCPU, totalCPU, cpuLimit, cap.CPUMilliCores, rates.CPU*100))
	}
	if newMem > 0 && cap.MemoryBytes > 0 && totalMem > memLimit {
		reasons = append(reasons, fmt.Sprintf("Memory: 请求 %d + 已有 %d = %d > 上限 %d（节点 %d × %.0f%%）",
			newMem, existingMemory, totalMem, memLimit, cap.MemoryBytes, rates.Memory*100))
	}

	if len(reasons) > 0 {
		return fmt.Errorf("%s: 超出节点资源上限: %s", resource.ErrResourceExhausted, strings.Join(reasons, "; "))
	}
	return nil
}

// SumPodRequests 计算一组 Pod 的 request 总和（含副本数乘数）：
// 每个 Pod 的 request × replicas（replicas ≤ 0 视为 1）累加。
// 返回 (totalCPU milliCores, totalMemory bytes)。
// 解析失败（脏数据）的字段按 0 处理（只记跳过，不中断整体校验）。
func SumPodRequests(pods []metamanager.Pod) (int64, int64) {
	var cpu, mem int64
	for _, p := range pods {
		replicas := p.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		c, _ := resource.ParseCPU(p.Resources.CPURequest)
		m, _ := resource.ParseMemory(p.Resources.MemoryRequest)
		cpu += c * int64(replicas)
		mem += m * int64(replicas)
	}
	return cpu, mem
}

// DockerResourceArgs 返回 docker run 的资源限制参数：
//   - CPU limit 非空 → --cpus=<value>（如 250m → --cpus=0.25）
//   - Memory limit 非空 → --memory=<bytes> --memory-swap=<bytes>（swap=memory，禁用 swap）
//
// 返回的 args 可直接追加到 exec("run", "-d", ...) 的参数列表。
func DockerResourceArgs(r metamanager.ResourceRequirements) ([]string, error) {
	var args []string
	if r.CPULimit != "" {
		cpu, err := resource.ParseCPU(r.CPULimit)
		if err != nil {
			return nil, fmt.Errorf("CPU limit 解析失败: %w", err)
		}
		if cpu > 0 {
			args = append(args, "--cpus="+resource.FormatCPU(cpu))
		}
	}
	if r.MemoryLimit != "" {
		mem, err := resource.ParseMemory(r.MemoryLimit)
		if err != nil {
			return nil, fmt.Errorf("Memory limit 解析失败: %w", err)
		}
		if mem > 0 {
			args = append(args, "--memory="+resource.FormatMemoryBytes(mem))
			args = append(args, "--memory-swap="+resource.FormatMemorySwapBytes(mem))
		}
	}
	return args, nil
}
