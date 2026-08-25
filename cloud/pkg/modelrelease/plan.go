// Package modelrelease 实现 v0.7.0 灰度发布执行层（控制器 + 部署执行器 +
// 算法纯函数），消费 modelrepo 的存储面，被 cmd/cloudcore 装配层注入。
//
// 设计定稿 §5/§6（subagent_01）+ 主线裁决 D1-D9（decision.md）模块划分：
//   - plan.go   （WBS-4）灰度算法纯函数：百分比选择（分母=创建时刻 Ready
//     快照、上取整、字典序）、archs 过滤、批次规划（批序号预分配）；
//   - state.go  （WBS-5）状态机合法性表与控制器不变式（D8：succeeded 前
//     全部 TargetNodes perNode 终态 deployed 且无 failed）；
//   - deploy.go （WBS-6）部署执行器 DeployVersion（podsync 镜像 + config-sync
//     元数据保留键合并平铺 + 部署影子写穿，错误映射到机器可读文案）；
//   - lock.go   （WBS-6）领跑锁能力面 LockKV：外部模式 = 租约锁
//     grant-per-claim + 值内编码过期时间（键存在且 expiresAt 新鲜 → 他副本
//     持有，跳过；过期/值损坏 → 接管重绑租约）；embed/内存 = no-op 恒成功；
//   - release.go（WBS-6）发布控制器：扫描 ticker（默认 5s，可注入时钟）、
//     pending→running 认领（CAS）+ 锁、批调度（NextBatchAt=pauseBetween
//     持久化跨接管）、cancel 批次边界停止 + 剩余 skipped、rollback 逆序批次
//     （pause=0）与执行期复查（D2/D4 回滚 TOCTOU）、succeeded 前 D8 不变式、
//     终态 guard 释放（D9/N-1 密钥保留策略见 releaseGuard 注释）。
//
// D9 登记项（代码注释落点，见各函数）：
//   - N-1 终态 release 键保留策略：ReleaseGuard 只删 guard 键，release 头与
//     perNode 键永久保留作审计痕迹（etcdctl 可手动清理）；
//   - N-3 批间 pause 丢失：批完成 → NextBatchAt 写 head 之间存在崩溃窗口，
//     接管后旧 NextBatchAt（过去时刻）→ 下一批立即开跑，该次 pause 不计；
//   - N-4 终态 release 常驻内存无 GC：内存缓存持有全部 release/perNode
//     （含终态），与 KNOWN-ISSUES L28（无分页）同族，长期运行线性增长。
package modelrelease

import (
	"fmt"

	"edgeflow/cloud/pkg/modelrepo"
)

// ── 百分比节点选择（设计 §5.3，纯函数；A1）──────────────────────────────

// SelectPercentageNodes 按比例选择目标节点：
//
//   - 分母 = 发布创建时刻的 Ready 节点快照（运行期不重算）；
//   - n = ceil(len(ready) × pct / 100)，且 1 ≤ n ≤ len(ready)
//     （上取整：宁可多发 1 台也不发 0 台，保证管理员可验证）；
//   - 确定性：NodeID 字典序排序后取前 n（跨副本可复现，配合 CAS 无歧义）。
//
// 错误：len(ready)==0 → 包装 modelrepo.ErrNoReadyNodes（API → 422
// `no ready nodes`）；pct 越界 [1,100] → 参数错误（400 族）。
func SelectPercentageNodes(ready []string, pct int) ([]string, error) {
	if pct < 1 || pct > 100 {
		return nil, fmt.Errorf("modelrelease: percentage must be within 1..100, got %d", pct)
	}
	if len(ready) == 0 {
		return nil, fmt.Errorf("%w: no ready nodes", modelrepo.ErrNoReadyNodes)
	}
	n := (len(ready)*pct + 99) / 100 // ceil(len*pct/100)
	if n < 1 {
		n = 1
	}
	if n > len(ready) {
		n = len(ready)
	}
	sorted := modelrepo.SortNodes(ready) // 字典序复制，不入参原地改
	return sorted[:n], nil
}

// NodeRef 是参与百分比选择的节点最小引用（设计 WBS-4：用本地小结构体，
// 不 import registry，控制器/API 侧负责从 registry.NodeInfo 转换）。
type NodeRef struct {
	NodeID string
	Arch   string
}

// ApplyArchFilter 按版本支持架构过滤节点（设计 §5.3：`版本 Archs 非空时`；
// archs 为空 = 不限制，返回原集合拷贝）。过滤是无序保留原序的。
func ApplyArchFilter(nodes []NodeRef, archs []string) []NodeRef {
	if len(archs) == 0 {
		return append([]NodeRef(nil), nodes...)
	}
	want := make(map[string]bool, len(archs))
	for _, a := range archs {
		want[a] = true
	}
	out := make([]NodeRef, 0, len(nodes))
	for _, n := range nodes {
		if want[n.Arch] {
			out = append(out, n)
		}
	}
	return out
}

// SelectPercentageNodesByArch 是带架构过滤的百分比选择（设计 §5.3）：
// Ready 快照先按 node.arch ∈ version.archs 过滤，再对过滤结果执行
// SelectPercentageNodes。错误：
//   - ready 为 0 → ErrNoReadyNodes（`no ready nodes`，422）；
//   - 过滤后 0 台 → ErrNoReadyNodes（文案含 `no ready nodes for archs [...]`，
//     422）；
//   - pct 越界 → 参数错误。
//
// 注：白名单（nodeIDs）模式不做 arch 过滤（用户显式指定即认可，设计 §5.3）。
func SelectPercentageNodesByArch(ready []NodeRef, pct int, archs []string) ([]string, error) {
	if len(ready) == 0 {
		return nil, fmt.Errorf("%w: no ready nodes", modelrepo.ErrNoReadyNodes)
	}
	filtered := ApplyArchFilter(ready, archs)
	if len(filtered) == 0 {
		return nil, fmt.Errorf("%w: no ready nodes for archs %v", modelrepo.ErrNoReadyNodes, archs)
	}
	ids := make([]string, 0, len(filtered))
	for _, n := range filtered {
		ids = append(ids, n.NodeID)
	}
	return SelectPercentageNodes(ids, pct)
}

// ── 批次规划（设计 §5.4，纯函数；A2）────────────────────────────────────

// BuildBatches 把有序目标节点快照按 batchSize 切分为批次（每批 batchSize
// 个节点，最后一批可不满；batchSize 语义 = 批粒度与暂停节奏，批内逐节点
// **串行**——主线裁决 D6 维持串行，R-4 文案修正，批内并发排 P2）。
//
// 批次序号约定：返回的第 i 个批次序号为 i+1（1 起）；perNode 创建时按
// 该序号预分配 Batch 字段（设计 §2.3，不可变）。控制器与创建 API 共用
// 本函数，保证批边界一致。batchSize<1 防御性按 1 处理；空输入 → nil。
func BuildBatches(ordered []string, batchSize int) [][]string {
	if batchSize < 1 {
		batchSize = 1
	}
	if len(ordered) == 0 {
		return nil
	}
	out := make([][]string, 0, (len(ordered)+batchSize-1)/batchSize)
	for start := 0; start < len(ordered); start += batchSize {
		end := start + batchSize
		if end > len(ordered) {
			end = len(ordered)
		}
		out = append(out, append([]string(nil), ordered[start:end]...))
	}
	return out
}

// BatchNumber 返回 TargetNodes 中第 orderedIndex 个节点（0 起）所属批次
// 序号（1 起；设计 §2.3 批序号创建时预分配，不可变）。与 BuildBatches
// 的切分口径一致（batchSize<1 防御性按 1 处理）。
func BatchNumber(orderedIndex, batchSize int) int {
	if batchSize < 1 {
		batchSize = 1
	}
	if orderedIndex < 0 {
		return 1
	}
	return orderedIndex/batchSize + 1
}
