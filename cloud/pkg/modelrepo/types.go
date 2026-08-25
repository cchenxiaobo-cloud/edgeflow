// Package modelrepo 实现 v0.7.0 模型仓库/版本管理/灰度发布的数据模型与存储层。
//
// 设计定稿 §2/§3（subagent_01）+ 主线裁决 D2/D3/D4/D7/D8/D9：
//   - 数据模型：Model（模型台账）/ ModelVersion（版本台账，"Tag 即版本"）/
//     ModelRelease（灰度任务）/ NodeReleaseResult（逐节点结果）/ DeploymentState
//     （部署影子，云端 Desired 语义的派生台账）；
//   - 状态机：版本三态（draft/active/archived）与发布六态
//     （pending/running/succeeded/failed/canceled/rolled_back），全部经本包
//     纯函数表驱动（CanTransitionVersion/CanTransitionRelease）；
//   - 错误哨兵：导出集合供 API 层映射状态码（400/404/409/422）与控制器判定；
//   - 校验：modelName/version/mirror/sha256/archs/metadata 全部纯函数（§8.1）。
//
// 本文件 = 纯类型 + 纯校验，零依赖（不 import 网络/存储），可直接单测。
package modelrepo

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ── 枚举类型（设计 §2.3）─────────────────────────────────────────────────

// VersionStatus 是版本台账状态（设计 §2.4 版本状态机）。
type VersionStatus string

// 版本状态取值。
const (
	VersionStatusDraft    VersionStatus = "draft"
	VersionStatusActive   VersionStatus = "active"
	VersionStatusArchived VersionStatus = "archived"
)

// NodeRelStatus 是逐节点执行结果状态（单向终态，见 §2.4）。
type NodeRelStatus string

// 逐节点状态取值。
const (
	NodeRelPending  NodeRelStatus = "pending"
	NodeRelDeployed NodeRelStatus = "deployed"
	NodeRelFailed   NodeRelStatus = "failed"
	NodeRelSkipped  NodeRelStatus = "skipped"
)

// ReleaseStatus 是灰度发布任务状态（设计 §2.4 发布状态机）。
type ReleaseStatus string

// 发布状态取值。
const (
	ReleaseStatusPending    ReleaseStatus = "pending"
	ReleaseStatusRunning    ReleaseStatus = "running"
	ReleaseStatusSucceeded  ReleaseStatus = "succeeded"
	ReleaseStatusFailed     ReleaseStatus = "failed"
	ReleaseStatusCanceled   ReleaseStatus = "canceled"
	ReleaseStatusRolledBack ReleaseStatus = "rolled_back"
)

// IsValid 报告版本状态是否为已知取值。
func (s VersionStatus) IsValid() bool {
	switch s {
	case VersionStatusDraft, VersionStatusActive, VersionStatusArchived:
		return true
	}
	return false
}

// IsValid 报告逐节点状态是否为已知取值。
func (s NodeRelStatus) IsValid() bool {
	switch s {
	case NodeRelPending, NodeRelDeployed, NodeRelFailed, NodeRelSkipped:
		return true
	}
	return false
}

// IsValid 报告发布状态是否为已知取值。
func (s ReleaseStatus) IsValid() bool {
	switch s {
	case ReleaseStatusPending, ReleaseStatusRunning, ReleaseStatusSucceeded,
		ReleaseStatusFailed, ReleaseStatusCanceled, ReleaseStatusRolledBack:
		return true
	}
	return false
}

// IsTerminal 报告发布状态是否终态（终态 = guard 可释放、同模型可再次发布）。
func (s ReleaseStatus) IsTerminal() bool {
	switch s {
	case ReleaseStatusSucceeded, ReleaseStatusFailed, ReleaseStatusCanceled, ReleaseStatusRolledBack:
		return true
	}
	return false
}

// InFlight 报告发布状态是否在途（占用同模型 guard 键）。
func (s ReleaseStatus) InFlight() bool {
	return s == ReleaseStatusPending || s == ReleaseStatusRunning
}

// ── 状态机转换合法性表（设计 §2.4，纯函数）─────────────────────────────

// CanTransitionVersion 报告版本状态机转换是否合法（设计 §2.4）：
//
//	draft ──activate──▶ active ──archive──▶ archived
//	（archived 不可再激活——需新版本号；delete 仅限 draft/archived，由
//	 DeleteVersion 前置校验承担，不在此表）
func CanTransitionVersion(from, to VersionStatus) bool {
	switch from {
	case VersionStatusDraft:
		return to == VersionStatusActive
	case VersionStatusActive:
		return to == VersionStatusArchived
	}
	return false
}

// CanTransitionRelease 报告发布状态机转换是否合法（设计 §2.4）：
//
//	pending → running（控制器认领）/ canceled（cancel API）
//	running → succeeded / failed / canceled / rolled_back
//	succeeded|failed|canceled → rolled_back（回滚执行，逐节点逆序）
//	rolled_back → 无（终态；API 再回滚 → 409）
func CanTransitionRelease(from, to ReleaseStatus) bool {
	if !from.IsValid() || !to.IsValid() {
		return false
	}
	switch from {
	case ReleaseStatusPending:
		return to == ReleaseStatusRunning || to == ReleaseStatusCanceled
	case ReleaseStatusRunning:
		switch to {
		case ReleaseStatusSucceeded, ReleaseStatusFailed, ReleaseStatusCanceled, ReleaseStatusRolledBack:
			return true
		}
	case ReleaseStatusSucceeded, ReleaseStatusFailed, ReleaseStatusCanceled:
		return to == ReleaseStatusRolledBack
	}
	return false
}

// CanTransitionNodeResult 报告逐节点结果状态转换是否合法（设计 §2.4：
// 单向终态不回退；回滚只更新 Version 字段，状态保持 deployed/failed）：
//
//	pending → deployed|failed|skipped（首写）
//	终态 → 终态（回滚执行：failed→deployed 恢复、deployed 保持改 Version、
//	 skipped→deployed 等；禁止任何 →pending 回退）
func CanTransitionNodeResult(from, to NodeRelStatus) bool {
	if !from.IsValid() || !to.IsValid() {
		return false
	}
	if to == NodeRelPending {
		return false // 禁止回退 pending
	}
	return true
}

// ── 数据模型（设计 §2.1-2.3）────────────────────────────────────────────

// Model 是模型仓库的一级对象：模型名唯一，版本（ModelVersion）为二级对象。
type Model struct {
	Name        string            `json:"name"`        // 唯一键；^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$（禁 '/'，etcd 键安全）
	Description string            `json:"description"` // 人类可读说明（≤256 字符）
	Type        string            `json:"type"`        // 推理类型，开放字符串（≤64，字符集同 name）
	CreatedAt   int64             `json:"createdAt"`   // Unix 毫秒
	UpdatedAt   int64             `json:"updatedAt"`   // Unix 毫秒（模型或版本变更时刷新，best-effort）
	Metadata    map[string]string `json:"metadata"`    // 扩展元数据（键值），可空
}

// ModelVersion 是模型下的版本对象（"Tag 即版本"的正式台账化；发布对准入 active）。
type ModelVersion struct {
	Model     string        `json:"model"`     // 所属模型名
	Version   string        `json:"version"`   // 版本 tag；同一模型内唯一
	Mirror    string        `json:"mirror"`    // 模型镜像 ref（镜像即模型）；必填；校验见设计 §8.1
	Sha256    string        `json:"sha256"`    // 镜像摘要，^sha256:[0-9a-f]{64}$；必填；存储统一小写
	SizeBytes int64         `json:"sizeBytes"` // 镜像大小字节；>=0
	Archs     []string      `json:"archs"`     // 支持架构子集：amd64/arm64（白名单）；空 = 不限制
	Status    VersionStatus `json:"status"`    // draft|active|archived
	// PrevActive 是本版本激活时被降级的旧 active 版本（回滚目标；"" = 无）。
	// 由 ActivateVersion 写入（激活 v2 时记录被归档的 v1），发布创建时
	// 读取并落入 ModelRelease.PrevActive——保证 API 链路"先激活后发布"
	// 下回滚链不断（E2E 发现：激活后 ActiveVersion==目标，旧公式恒空）。
	PrevActive string            `json:"prevActive,omitempty"`
	CreatedAt  int64             `json:"createdAt"`
	UpdatedAt  int64             `json:"updatedAt"` // 状态变更时刷新
	Metadata   map[string]string `json:"metadata"`  // 模型参数/阈值等键值；发布时平铺进 config-sync
}

// ReleaseTarget 是发布目标选择：白名单 或 按比例（二选一）。
type ReleaseTarget struct {
	Type       string   `json:"type"`       // "nodeIDs" | "percentage"
	NodeIDs    []string `json:"nodeIDs"`    // 白名单：节点 ID 列表（创建时校验全部存在）
	Percentage int      `json:"percentage"` // 1..100（百分比）；仅 Type=percentage 时有效
}

// NodeReleaseResult 是逐节点执行结果（独立 etcd 键，设计 §3.1）。
type NodeReleaseResult struct {
	NodeID     string        `json:"nodeID"`
	Status     NodeRelStatus `json:"status"`  // pending|deployed|failed|skipped
	Version    string        `json:"version"` // 该节点当前被本次任务部署到的版本（目标版本或回滚目标版本）
	Reason     string        `json:"reason"`  // failed 时的人类可读原因（错误文本）
	Batch      int           `json:"batch"`   // 所属批次序号（1 起）；创建时预分配，不可变
	StartedAt  int64         `json:"startedAt"`
	FinishedAt int64         `json:"finishedAt"`
}

// ModelRelease 是灰度发布任务（异步执行；创建返回 202）。
type ModelRelease struct {
	ID                string        `json:"id"` // UUID（生成方式与 protocol.NewMessage 同源）；etcd 键的一部分
	Model             string        `json:"model"`
	Version           string        `json:"version"`                 // 目标版本；创建时须为 active
	Target            ReleaseTarget `json:"target"`                  // 选择方式（创建时物化）
	TargetNodes       []string      `json:"targetNodes"`             // 创建时物化的**有序**目标节点快照；运行期不随节点状态变化
	BatchSize         int           `json:"batchSize"`               // 每批节点数；>=1；默认 1；批内逐节点串行（D6）
	PauseBetween      int64         `json:"pauseBetween"`            // 批间暂停（毫秒）；>=0；默认 0
	FailFast          bool          `json:"failFast"`                // 单节点失败策略；默认 true（对齐 keadm batch fail-fast 语义）
	Status            ReleaseStatus `json:"status"`                  // pending|running|succeeded|failed|canceled|rolled_back
	PrevActive        string        `json:"prevActive"`              // 创建时模型当前 active 版本（≠ 目标版本时记录；=="" 表示无）；回滚目标
	RollbackRequested bool          `json:"rollbackRequested"`       // 回滚 API 置位，控制器异步执行
	NextBatchAt       int64         `json:"nextBatchAt"`             // 下一批最早开始时间（Unix 毫秒）；跨接管持久
	FailureReason     string        `json:"failureReason,omitempty"` // 终态失败原因（回滚中止 D2/D4、fail-fast 中止等；设计 §2.3 未列字段，
	//   为 D2「head 置 failed（reason=...）」的落点新增，omitempty 向后兼容，实施登记）
	CreatedAt  int64 `json:"createdAt"`
	StartedAt  int64 `json:"startedAt"`
	FinishedAt int64 `json:"finishedAt"`
}

// DeploymentState 是部署影子（云端 Desired 语义的派生台账，设计 §6.3）：
// 键 /edgeflow/models/deployments/<model>/<nodeID>；普通 Put 整值覆盖
// （派生台账非权威指令，无 CAS 需求——v0.6.0 审稿线索 3 论证，设计 R16 口径）。
type DeploymentState struct {
	Model     string `json:"model"`
	NodeID    string `json:"nodeID"`
	Version   string `json:"version"`
	Mirror    string `json:"mirror"`
	ReleaseID string `json:"releaseID"`
	UpdatedAt int64  `json:"updatedAt"`
}

// ── 错误哨兵（导出，供 API 映射与控制器判定；设计 WBS-1）───────────────

var (
	// ErrModelNotFound 模型不存在（API → 404；其子资源一律 404）。
	ErrModelNotFound = errors.New("modelrepo: model not found")
	// ErrModelExists 模型已存在（API → 409）。
	ErrModelExists = errors.New("modelrepo: model already exists")
	// ErrModelHasActiveVersion 删除模型前置失败：仍有 active 版本（API → 409）。
	ErrModelHasActiveVersion = errors.New("modelrepo: model has active version")
	// ErrVersionNotFound 版本不存在（API → 404）。
	ErrVersionNotFound = errors.New("modelrepo: version not found")
	// ErrVersionExists 版本已存在（API → 409）。
	ErrVersionExists = errors.New("modelrepo: version already exists")
	// ErrVersionNotActive 归档/回滚前置失败：版本非 active（API → 409）。
	ErrVersionNotActive = errors.New("modelrepo: version is not active")
	// ErrVersionNotDraft 激活前置失败：版本非 draft（API → 409）。
	ErrVersionNotDraft = errors.New("modelrepo: version is not draft")
	// ErrVersionActive 删除版本前置失败：版本为 active（API → 409；
	// 设计 §2.4「delete 仅允许 draft/archived；active → 409」；WBS-1 哨兵清单
	// 之外的补充哨兵，实施登记）。
	ErrVersionActive = errors.New("modelrepo: version is active")
	// ErrReleaseNotFound 发布不存在（API → 404）。
	ErrReleaseNotFound = errors.New("modelrepo: release not found")
	// ErrReleaseConflict 同模型在途发布/在途发布指向的版本被归档（API → 409，
	// 响应含在途 releaseID）。
	ErrReleaseConflict = errors.New("modelrepo: release in progress")
	// ErrReleaseTerminal 发布状态不允许目标操作（cancel/rollback 已终态 → 409）。
	ErrReleaseTerminal = errors.New("modelrepo: release is terminal for this operation")
	// ErrNoPrevActive 回滚前置失败：无 PrevActive / PrevActive 版本不存在（API → 422）。
	ErrNoPrevActive = errors.New("modelrepo: no previous active version")
	// ErrVersionMismatch 回滚守卫：release.version ≠ 模型当前 active（已被更新版本
	// 接管，API → 409，文案引导显式 activate 或新发布，设计 L26）。
	ErrVersionMismatch = errors.New("modelrepo: release version is no longer current active")
	// ErrNoReadyNodes 百分比发布分母为 0（API → 422 no ready nodes）。
	ErrNoReadyNodes = errors.New("modelrepo: no ready nodes")
	// ErrUnknownNodes 白名单含未注册节点（API → 422，响应列 unknownNodes）。
	ErrUnknownNodes = errors.New("modelrepo: unknown nodes in target")
	// ErrConcurrentConflict CAS 冲突重试耗尽（API → 409）。
	ErrConcurrentConflict = errors.New("modelrepo: concurrent modification conflict")
)

// ReleaseConflictError 是发布并发冲突错误（设计 WBS-2：ErrReleaseConflict
// 带在途 ID——响应 409 含 releaseID）。错误哨兵 errors.Is 可命中 ErrReleaseConflict。
type ReleaseConflictError struct {
	// InFlight 是在途（guard 指向）的 releaseID。
	InFlight string
}

func (e *ReleaseConflictError) Error() string {
	return fmt.Sprintf("%v: release %s in progress", ErrReleaseConflict, e.InFlight)
}

// Unwrap 支持 errors.Is(err, ErrReleaseConflict)。
func (e *ReleaseConflictError) Unwrap() error { return ErrReleaseConflict }

// UnknownNodesError 是白名单含未知节点错误（API → 422，响应带 unknownNodes 列表）。
// 错误哨兵 errors.Is 可命中 ErrUnknownNodes。
type UnknownNodesError struct {
	// Nodes 是未注册的节点 ID 列表（响应字段 unknownNodes）。
	Nodes []string
}

func (e *UnknownNodesError) Error() string {
	return fmt.Sprintf("%v: %v", ErrUnknownNodes, e.Nodes)
}

// Unwrap 支持 errors.Is(err, ErrUnknownNodes)。
func (e *UnknownNodesError) Unwrap() error { return ErrUnknownNodes }

// ── 校验纯函数（设计 §8.1；全部 400 族）────────────────────────────────

// modelNameRe 同时约束 modelName/version/tag（禁 '/'，etcd 键安全；禁空白
// 与控制字符由字符集保证）。
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// nodeIDRe 复用既有 nodeID 硬约束（与 registry 同规则，设计 §8.1）。
var nodeIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// mirrorRe 是镜像 ref 校验正则（设计 §8.1，原样落地）：
//
//	^[a-z0-9]+((\.|_|-)[a-z0-9]+)*(\:[0-9]{1,5})?(\/[a-z0-9._-]+)+(\:[A-Za-z0-9._-]+)$
//
// 语义：小写 registry（可选 :port）+ 至少一段路径 + 必带 tag（最后一段 `:` 为
// tag 界定，与 Docker ref 惯例一致）。注意：该正则要求至少一个 '/'（registry
// 形态），单段 "repo:tag" 不满足——设计 §8.1 正则与 §10.1 测试列举的 "repo:tag"
// 例冲突时以 §8.1 为准（实施登记，API-SPEC §7 由设计稿 §8.1 转写）。
var mirrorRe = regexp.MustCompile(`^[a-z0-9]+((\.|_|-)[a-z0-9]+)*(\:[0-9]{1,5})?(\/[a-z0-9._-]+)+(\:[A-Za-z0-9._-]+)$`)

// mirrorPortRe 提取 registry 端口（仅出现在第一个 '/' 之前）。
var mirrorPortRe = regexp.MustCompile(`^[a-z0-9]+((\.|_|-)[a-z0-9]+)*:([0-9]{1,5})(/|$)`)

// sha256Re 是镜像摘要校验（大小写不敏感接受，存储统一小写）。
var sha256Re = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)

// metadataKeyRe 是扩展元数据键约束（^[A-Za-z0-9._-]{1,64}$）。
var metadataKeyRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// archs 白名单（设计 §8.1：元素 ∈ {amd64, arm64}）。
var validArchs = map[string]bool{"amd64": true, "arm64": true}

// ValidateModelName 校验模型名/版本 tag 字符集（^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$，
// 禁 '/'，etcd 键安全；设计 §8.1）。
func ValidateModelName(s string) error {
	if s == "" {
		return errors.New("model name is required")
	}
	if !modelNameRe.MatchString(s) {
		return fmt.Errorf("invalid model name %q: must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ (no '/', no whitespace/control chars)", s)
	}
	return nil
}

// ValidateVersionTag 校验版本 tag（字符集同 modelName；设计 §8.1）。
func ValidateVersionTag(s string) error {
	if s == "" {
		return errors.New("version is required")
	}
	if !modelNameRe.MatchString(s) {
		return fmt.Errorf("invalid version tag %q: must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ (no '/')", s)
	}
	return nil
}

// ValidateMirror 校验镜像 ref（设计 §8.1）：小写、必带 tag、禁 ".."、禁连续 '/'
// （正则已排除）、端口 1..65535 越界拒绝。tag 由最后一个 ':' 界定。
func ValidateMirror(s string) error {
	if s == "" {
		return errors.New("mirror is required")
	}
	if !mirrorRe.MatchString(s) {
		return fmt.Errorf("invalid mirror %q: must be a lowercase registry reference with at least one path segment and an explicit tag (e.g. registry.example.com/ns/model:v1.2.0)", s)
	}
	if m := mirrorPortRe.FindStringSubmatch(s); m != nil {
		port := 0
		for _, c := range m[2] {
			port = port*10 + int(c-'0')
		}
		if port > 65535 {
			return fmt.Errorf("invalid mirror %q: registry port out of range (%d > 65535)", s, port)
		}
	}
	return nil
}

// ValidateSha256 校验镜像摘要（^sha256:[0-9a-f]{64}$，大小写不敏感；存储统一小写
// 由 ModelVersion.Sanitize 归一）。
func ValidateSha256(s string) error {
	if s == "" {
		return errors.New("sha256 is required")
	}
	if !sha256Re.MatchString(s) {
		return fmt.Errorf("invalid sha256 %q: must match ^sha256:[0-9a-f]{64}$", s)
	}
	return nil
}

// ValidateArchs 校验架构白名单（元素 ∈ {amd64, arm64}；空 = 不限制）。
func ValidateArchs(a []string) error {
	for _, arch := range a {
		if !validArchs[arch] {
			return fmt.Errorf("invalid arch %q: must be one of [amd64 arm64]", arch)
		}
	}
	return nil
}

// ValidateMetadata 校验扩展元数据（键 ^[A-Za-z0-9._-]{1,64}$；值 ≤1024 字符）。
func ValidateMetadata(m map[string]string) error {
	for k, v := range m {
		if !metadataKeyRe.MatchString(k) {
			return fmt.Errorf("invalid metadata key %q: must match ^[A-Za-z0-9._-]{1,64}$", k)
		}
		if utf8.RuneCountInString(v) > 1024 {
			return fmt.Errorf("invalid metadata value for key %q: exceeds 1024 chars", k)
		}
	}
	return nil
}

// SanitizeModelName 归一模型名为下发命名段（设计 §6.1 命名约定）：转小写 +
// '.'→'-'。podName = cfgName = "edgeflow-model-" + sanitize(modelName)。
func SanitizeModelName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), ".", "-")
}

// Sanitize 归一版本台账的持久化形态（设计 WBS-1）：sha256 统一小写、archs 去重
// （保持原序）。创建版本时由 API 层调用。
func (v *ModelVersion) Sanitize() error {
	if v == nil {
		return errors.New("modelrepo: nil version")
	}
	if v.Sha256 != "" {
		v.Sha256 = strings.ToLower(v.Sha256)
	}
	seen := make(map[string]bool, len(v.Archs))
	out := v.Archs[:0]
	for _, a := range v.Archs {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	v.Archs = out
	return nil
}

// KeyNamePrefix 是 etcd 键空间总前缀（设计 §3.1；与 registry/devicestatus 完全隔离）。
const KeyNamePrefix = "/edgeflow/models/"

// 子空间前缀（meta/versions/guards/releases/deployments；构造见 etcd_store.go）。
const (
	// KeyMetaPrefix 是模型台账键前缀。
	KeyMetaPrefix = KeyNamePrefix + "meta/"
	// KeyVersionsPrefix 是版本台账键前缀。
	KeyVersionsPrefix = KeyNamePrefix + "versions/"
	// KeyGuardsPrefix 是在途发布守卫键前缀。
	KeyGuardsPrefix = KeyNamePrefix + "guards/"
	// KeyReleasesPrefix 是发布头/逐节点结果/领跑锁键前缀。
	KeyReleasesPrefix = KeyNamePrefix + "releases/"
	// KeyDeploymentsPrefix 是部署影子键前缀。
	KeyDeploymentsPrefix = KeyNamePrefix + "deployments/"
)

// ReleaseLockKey 构造发布领跑锁键（控制器经 LockKV 消费；设计 §3.1）。
func ReleaseLockKey(releaseID string) string {
	return KeyReleasesPrefix + releaseID + "/lock"
}

// ── 发布创建校验（设计 WBS-4；400 族，422 族由 API 层/存储层承担）──────

// Validate 校验发布目标格式（400 族：target.type 白名单、percentage 越界、
// nodeIDs 字符集；白名单元素合法性与去重由 API 层完成，节点存在性属 422 族）。
func (t ReleaseTarget) Validate() error {
	switch t.Type {
	case "nodeIDs":
		if len(t.NodeIDs) == 0 {
			return errors.New("target.nodeIDs must be non-empty when target.type=nodeIDs")
		}
		for _, id := range t.NodeIDs {
			if !nodeIDRe.MatchString(id) {
				return fmt.Errorf("invalid nodeID %q in target.nodeIDs: must match ^[A-Za-z0-9._-]+$", id)
			}
		}
	case "percentage":
		if t.Percentage < 1 || t.Percentage > 100 {
			return fmt.Errorf("target.percentage must be within 1..100, got %d", t.Percentage)
		}
	default:
		return fmt.Errorf("invalid target.type %q: must be \"nodeIDs\" or \"percentage\"", t.Type)
	}
	return nil
}

// ValidateCreate 校验发布创建的 400 族字段（batchSize/pauseBetween 缺省由
// API 层先落默认值再调用；设计 §4.2 字段表）。
func (r *ModelRelease) ValidateCreate() error {
	if r == nil {
		return errors.New("modelrepo: nil release")
	}
	if err := ValidateModelName(r.Model); err != nil {
		return err
	}
	if err := ValidateVersionTag(r.Version); err != nil {
		return err
	}
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if r.BatchSize < 1 {
		return fmt.Errorf("batchSize must be >= 1, got %d", r.BatchSize)
	}
	if r.PauseBetween < 0 {
		return fmt.Errorf("pauseBetween must be >= 0, got %d", r.PauseBetween)
	}
	return nil
}

// SortNodes 字典序排序（百分比选择确定性；设计 §5.3 跨副本可复现）。
func SortNodes(nodes []string) []string {
	out := append([]string(nil), nodes...)
	sort.Strings(out)
	return out
}
