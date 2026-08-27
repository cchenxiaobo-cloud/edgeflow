// 模型仓库 API（v0.7.0，WBS-7）：18 个新 REST 端点
// （模型 5 + 版本 6 + 发布 5 + 部署影子 1，设计定稿 §4.1 表 / 主线裁决 D1
// 端点数 17 统一；F-1 修正稿）。全部注册在既有 apiMux 上，自动挂
// auth.Middleware + ledger.Middleware（401 审计、action/operator 留痕
// 零额外代码）。
//
// 端点清单（subagent_01.md §4.1 表 17 行，逐一落地，不增不减）：
//
//	GET    /api/v1/models                                  模型列表（K8s List）
//	POST   /api/v1/models                                  创建模型
//	GET    /api/v1/models/{modelName}                      模型详情
//	PUT    /api/v1/models/{modelName}                      更新模型
//	DELETE /api/v1/models/{modelName}                      删除模型（级联）
//	GET    /api/v1/models/{modelName}/versions             版本列表
//	POST   /api/v1/models/{modelName}/versions             创建版本（draft）
//	GET    /api/v1/models/{modelName}/versions/{version}   版本详情
//	DELETE /api/v1/models/{modelName}/versions/{version}   删除版本（draft/archived）
//	POST   /api/v1/models/{modelName}/versions/{version}/activate  激活
//	POST   /api/v1/models/{modelName}/versions/{version}/archive   归档
//	POST   /api/v1/models/{modelName}/releases             创建发布（202）
//	GET    /api/v1/models/{modelName}/releases             发布列表
//	GET    /api/v1/models/{modelName}/releases/{releaseID} 发布详情（含 summary）
//	GET    /api/v1/models/{modelName}/releases/{releaseID}/digest   发布 digest 复核（v0.12.0）
//	POST   /api/v1/models/{modelName}/releases/{releaseID}/cancel    取消
//	POST   /api/v1/models/{modelName}/releases/{releaseID}/rollback  回滚（202）
//	GET    /api/v1/models/{modelName}/deployments          部署影子
//
// 错误语义（设计 §4.3）：400 非法请求/校验失败/413 请求体超 1MiB（复用
// decodeWriteBody 的 MaxBytesError 分支）；404 模型/版本/发布/影子不存在
// （模型不存在时其子资源一律 404）；409 冲突（已存在/状态机非法/在途发布/
// CAS 耗尽/回滚守卫）；422 语义不可执行（目标非 active/无 Ready 节点/
// 白名单未知节点/无 PrevActive）；202 异步受理（发布创建/回滚置位）。
// 全部错误响应统一 {"error":"<机器可读原因>", ...上下文字段}。
//
// 发布创建的 422/400 族前置校验（设计 §5.2）：内容校验（validate* 纯函数）
// → 400；模型/版本存在 → 404；目标版本 active / 白名单节点全部注册 /
// 百分比分母非 0 → 422；TargetNodes 创建时物化（白名单保序去重；百分比 =
// Ready 快照字典序前 n，含 archs 过滤）。
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"edgeflow/cloud/pkg/modelrelease"
	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/log"
)

// routeRegistrar 是管理 API 路由注册的目标面（*http.ServeMux 满足；
// 契约测试注入计数实现断言 17 条路由与 14+17=31 口径）。
type routeRegistrar interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// modelAPI 是模型仓库 API 的处理器集合（v0.7.0，WBS-7）。独立 struct，
// 不塞 nodeAPI——main.go 装配时平级注入。
type modelAPI struct {
	// store 是模型仓库存储（内存 / embed / 外部 etcd 三模式装配的
	// modelrepo.ModelStore 同实例，与控制器共享）。
	store modelrepo.ModelStore
	// reg 是节点注册表（发布创建的 Ready 快照 / 白名单存在性校验用）。
	reg registry.Store
	// mirrorCheck 是发布前镜像探活配置（v0.9.0，R-1）；nil = 不检查
	// （默认 off，零行为变化）。
	mirrorCheck *mirrorCheckConfig
	// digestOf 返回节点当前上报镜像 digest（v0.12.0，D-1 发布 digest 复核）；
	// 与控制器 DigestLookup 同一闭包实例（podstatus.NodeDigestOf），nil 安全
	// （nil → 全节点 currentImageDigest 空 → unknown）。
	digestOf func(nodeID string) string
}

// mirrorCheckConfig 是装配层注入的探活配置（env 解析见 etcd.go）。
type mirrorCheckConfig struct {
	mode    modelrelease.MirrorCheckMode
	timeout time.Duration
	token   string
}

// check 执行一次镜像探活；返回 (digest, 是否阻断, 错误)。off 模式恒不
// 检查（digest 空）。v0.11.0 R-1+：探活成功时固化 manifest digest 至
// 发布头（mirrorDigest），供终态 digest 校验。
func (c *mirrorCheckConfig) check(ctx context.Context, mirror string) (digest string, block bool, err error) {
	if c == nil || c.mode == modelrelease.MirrorCheckOff {
		return "", false, nil
	}
	digest, err = modelrelease.CheckMirror(ctx, mirror, modelrelease.MirrorCheckOptions{
		Mode:    c.mode,
		Timeout: c.timeout,
		Token:   c.token,
	})
	if err == nil {
		return digest, false, nil
	}
	return "", c.mode == modelrelease.MirrorCheckFail, err
}

// Register 一次性注册 18 条模型 API 路由（WBS-7 装配面最小：main.go 在
// 既有 11 条之后一行调用；v0.12.0 新增 digest 复核端点）。
func (a *modelAPI) Register(mux routeRegistrar) {
	mux.HandleFunc("GET /api/v1/models", a.listModels)
	mux.HandleFunc("POST /api/v1/models", a.createModel)
	mux.HandleFunc("GET /api/v1/models/{modelName}", a.getModel)
	mux.HandleFunc("PUT /api/v1/models/{modelName}", a.updateModel)
	mux.HandleFunc("DELETE /api/v1/models/{modelName}", a.deleteModel)
	mux.HandleFunc("GET /api/v1/models/{modelName}/versions", a.listVersions)
	mux.HandleFunc("POST /api/v1/models/{modelName}/versions", a.createVersion)
	mux.HandleFunc("GET /api/v1/models/{modelName}/versions/{version}", a.getVersion)
	mux.HandleFunc("DELETE /api/v1/models/{modelName}/versions/{version}", a.deleteVersion)
	mux.HandleFunc("POST /api/v1/models/{modelName}/versions/{version}/activate", a.activateVersion)
	mux.HandleFunc("POST /api/v1/models/{modelName}/versions/{version}/archive", a.archiveVersion)
	mux.HandleFunc("POST /api/v1/models/{modelName}/releases", a.createRelease)
	mux.HandleFunc("GET /api/v1/models/{modelName}/releases", a.listReleases)
	// v0.17.0：运行中可调参数（PATCH 部分更新语义；非终态可改执行参数）
	mux.HandleFunc("PATCH /api/v1/models/{modelName}/releases/{releaseID}", a.updateRelease)
	mux.HandleFunc("GET /api/v1/models/{modelName}/releases/{releaseID}", a.getRelease)
	mux.HandleFunc("GET /api/v1/models/{modelName}/releases/{releaseID}/snapshot", a.handleReleaseSnapshot) // v0.19.0
	mux.HandleFunc("GET /api/v1/models/{modelName}/releases/{releaseID}/digest", a.getReleaseDigest)
	mux.HandleFunc("POST /api/v1/models/{modelName}/releases/{releaseID}/cancel", a.cancelRelease)
	mux.HandleFunc("POST /api/v1/models/{modelName}/releases/{releaseID}/pause", a.pauseRelease)
	mux.HandleFunc("POST /api/v1/models/{modelName}/releases/{releaseID}/resume", a.resumeRelease)
	mux.HandleFunc("GET /api/v1/models/export", a.exportCatalog)
	mux.HandleFunc("POST /api/v1/models/import", a.importCatalog)
	mux.HandleFunc("POST /api/v1/models/{modelName}/releases/{releaseID}/rollback", a.rollbackRelease)
	mux.HandleFunc("GET /api/v1/models/{modelName}/deployments", a.listDeployments)
	// v0.18.0：全局部署影子查询（跨模型聚合；model/nodeID 过滤 query 可选）
	mux.HandleFunc("GET /api/v1/deployments", a.listAllDeployments)
	mux.HandleFunc("GET /api/v1/releases", a.handleListAllReleases) // v0.19.0 全局发布查询
}

// ── 响应/错误辅助 ──────────────────────────────────────────────────────

const jsonContentType = "application/json; charset=utf-8"

// pageParams 是列表分页参数（v0.8.0，L28）：limit=0 表示全量（默认），
// 合法范围 limit ∈ [1,1000]、offset ≥ 0。不传任何参数 = 旧行为（全量）。
type pageParams struct {
	limit  int
	offset int
}

// parsePageParams 解析 limit/offset 查询参数：非法 → error（handler 回 400）；
// 未设置 → 零值（limit=0 = 全量，offset=0）。
func parsePageParams(r *http.Request) (pageParams, error) {
	var p pageParams
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return p, fmt.Errorf("limit 必须为 1-1000 的整数（缺省 = 全量返回）")
		}
		p.limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return p, fmt.Errorf("offset 必须为非负整数（缺省 = 0）")
		}
		p.offset = n
	}
	return p, nil
}

// slicePage 应用分页切片：全量列表 → [offset, offset+limit)；
// limit=0 = 全量；offset 超界 → 空切片。返回切片与总数（X-Total-Count 用）。
func slicePage[T any](all []T, p pageParams) ([]T, int) {
	total := len(all)
	if p.limit == 0 {
		if p.offset >= total {
			return []T{}, total
		}
		return all[p.offset:], total
	}
	if p.offset >= total {
		return []T{}, total
	}
	hi := p.offset + p.limit
	if hi > total {
		hi = total
	}
	return all[p.offset:hi], total
}

// writePageHeaders 写分页响应头（X-Total-Count；v0.8.0，L28）。
func writePageHeaders(w http.ResponseWriter, total int) {
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
}

// writeJSON 写 JSON 响应（状态码 + Content-Type + 编码失败仅记日志）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logEncodeError("modelAPI", err)
	}
}

// writeErr 写统一错误形态 {"error": "...", ...extra}。
func writeErr(w http.ResponseWriter, status int, msg string, extra map[string]any) {
	body := map[string]any{"error": msg}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

// modelError 把存储/业务错误映射为状态码（设计 §4.3 错误表）：
//
//	404: 模型/版本/发布/影子不存在（子资源随模型 404）
//	409: 已存在 / active 拒删 / 状态机非法 / 在途发布（含 releaseID）/
//	     CAS 冲突耗尽 / 回滚守卫（L26 版本被接管）
//	422: 无 PrevActive / 无 Ready 节点 / 白名单未知节点（含 unknownNodes）
//	500: 兜底（存储/序列化异常）
func modelError(w http.ResponseWriter, err error) {
	var rce *modelrepo.ReleaseConflictError
	if errors.As(err, &rce) {
		writeErr(w, http.StatusConflict, err.Error(), map[string]any{"releaseID": rce.InFlight})
		return
	}
	var une *modelrepo.UnknownNodesError
	if errors.As(err, &une) {
		writeErr(w, http.StatusUnprocessableEntity, err.Error(), map[string]any{"unknownNodes": une.Nodes})
		return
	}
	switch {
	case errors.Is(err, modelrepo.ErrModelNotFound),
		errors.Is(err, modelrepo.ErrVersionNotFound),
		errors.Is(err, modelrepo.ErrReleaseNotFound):
		writeErr(w, http.StatusNotFound, err.Error(), nil)
	case errors.Is(err, modelrepo.ErrNoPrevActive),
		errors.Is(err, modelrepo.ErrNoReadyNodes),
		errors.Is(err, modelrepo.ErrUnknownNodes):
		writeErr(w, http.StatusUnprocessableEntity, err.Error(), nil)
	case errors.Is(err, modelrepo.ErrModelExists),
		errors.Is(err, modelrepo.ErrModelHasActiveVersion),
		errors.Is(err, modelrepo.ErrVersionExists),
		errors.Is(err, modelrepo.ErrVersionActive),
		errors.Is(err, modelrepo.ErrVersionNotActive),
		errors.Is(err, modelrepo.ErrVersionNotDraft),
		errors.Is(err, modelrepo.ErrReleaseConflict),
		errors.Is(err, modelrepo.ErrReleaseTerminal),
		errors.Is(err, modelrepo.ErrVersionMismatch),
		errors.Is(err, modelrepo.ErrConcurrentConflict):
		writeErr(w, http.StatusConflict, err.Error(), nil)
	default:
		log.Errorf("[modelAPI] 内部错误: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error", nil)
	}
}

// badRequest 是 400 族的快捷写（校验失败/非法 JSON 已由调用方分支处理）。
func badRequest(w http.ResponseWriter, format string, args ...any) {
	writeErr(w, http.StatusBadRequest, fmt.Sprintf(format, args...), nil)
}

// ── 校验纯函数（设计 §8.1；全部 400 族）──────────────────────────────

const (
	maxDescriptionRunes = 256
	maxTypeRunes        = 64
)

// validateDescription 校验人类可读说明（≤256 字符）。
func validateDescription(s string) error {
	if utf8.RuneCountInString(s) > maxDescriptionRunes {
		return fmt.Errorf("description 超过 %d 字符", maxDescriptionRunes)
	}
	return nil
}

// validateType 校验推理类型（开放字符串 ≤64，字符集同 modelName）。
func validateType(s string) error {
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > maxTypeRunes || !modelNameReLike(s) {
		return fmt.Errorf("invalid type %q: 字符集同模型名（^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$），长度 ≤64", s)
	}
	return nil
}

// modelNameReLike 是模型名/版本/tag 字符集的镜像判定（handler 前置校验用；
// 权威校验在 modelrepo.ValidateModelName——此处仅避免把路径段透传给存储）。
func modelNameReLike(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	c := s[0]
	if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// createModelRequest 是 POST /api/v1/models 请求体（设计 §4.2 字段表）。
type createModelRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Type        string            `json:"type"`
	Metadata    map[string]string `json:"metadata"`
}

// validate 校验创建模型请求（400 族）。
func (r *createModelRequest) validate() error {
	if err := modelrepo.ValidateModelName(r.Name); err != nil {
		return err
	}
	if err := validateDescription(r.Description); err != nil {
		return err
	}
	if err := validateType(r.Type); err != nil {
		return err
	}
	return modelrepo.ValidateMetadata(r.Metadata)
}

// updateModelRequest 是 PUT /api/v1/models/{modelName} 请求体：可编辑字段
// 全量替换语义（description/type/metadata；name/createdAt 不可变——设计
// §2.1 更新语义：metadata 整表替换，此处 description/type 同语义）。
type updateModelRequest struct {
	Description string            `json:"description"`
	Type        string            `json:"type"`
	Metadata    map[string]string `json:"metadata"`
}

func (r *updateModelRequest) validate() error {
	if err := validateDescription(r.Description); err != nil {
		return err
	}
	if err := validateType(r.Type); err != nil {
		return err
	}
	return modelrepo.ValidateMetadata(r.Metadata)
}

// createVersionRequest 是 POST /api/v1/models/{modelName}/versions 请求体。
type createVersionRequest struct {
	Version   string            `json:"version"`
	Mirror    string            `json:"mirror"`
	Sha256    string            `json:"sha256"`
	SizeBytes int64             `json:"sizeBytes"`
	Archs     []string          `json:"archs"`
	Metadata  map[string]string `json:"metadata"`
}

// validate 校验创建版本请求（400 族；sha256 大小写统一与 archs 去重由
// ModelVersion.Sanitize 在 handler 内完成）。
func (r *createVersionRequest) validate() error {
	if err := modelrepo.ValidateVersionTag(r.Version); err != nil {
		return err
	}
	if err := modelrepo.ValidateMirror(r.Mirror); err != nil {
		return err
	}
	if err := modelrepo.ValidateSha256(r.Sha256); err != nil {
		return err
	}
	if r.SizeBytes < 0 {
		return fmt.Errorf("sizeBytes 必须 >= 0，got %d", r.SizeBytes)
	}
	if err := modelrepo.ValidateArchs(r.Archs); err != nil {
		return err
	}
	return modelrepo.ValidateMetadata(r.Metadata)
}

// createReleaseRequest 是 POST /api/v1/models/{modelName}/releases 请求体
// （设计 §4.2 字段表；failFast 用指针三态区分"缺省 true"与显式 false）。
type createReleaseRequest struct {
	Version      string                  `json:"version"`
	Target       modelrepo.ReleaseTarget `json:"target"`
	BatchSize    int                     `json:"batchSize"`
	PauseBetween int64                   `json:"pauseBetween"`
	FailFast     *bool                   `json:"failFast"`
	// NotBeforeMs 定时维护窗口起点（v0.16.0 opt-in；Unix 毫秒；0 = 立即）。
	NotBeforeMs int64 `json:"notBeforeMs,omitempty"`
	// DryRun 预检模式（v0.17.0 opt-in）：true 时全量执行创建校验链但绝不
	// 落盘/不占 guard/不预写 perNode；响应 200 + wouldCreate 摘要（非承诺语义）。
	DryRun bool `json:"dryRun,omitempty"`
	// FailureBudget 失败预算（v0.18.0 opt-in）：≥1 启用——批后 failed 计数
	// 达预算自动 pause；0=缺省=禁用（行为与 v0.17.0 逐字节一致）。
	FailureBudget int `json:"failureBudget,omitempty"`
}

// applyDefaults 落设计缺省值（batchSize 缺省 1；failFast 缺省 true）。
func (r *createReleaseRequest) applyDefaults() {
	if r.BatchSize == 0 {
		r.BatchSize = 1
	}
	if r.FailFast == nil {
		t := true
		r.FailFast = &t
	}
}

// ── 列表响应形态（K8s List 风格，设计 §4.2；items 恒非 nil）───────────

type modelList struct {
	Kind       string            `json:"kind"`
	APIVersion string            `json:"apiVersion"`
	Items      []modelrepo.Model `json:"items"`
}

type versionList struct {
	Kind       string                   `json:"kind"`
	APIVersion string                   `json:"apiVersion"`
	Items      []modelrepo.ModelVersion `json:"items"`
}

type releaseList struct {
	Kind       string            `json:"kind"`
	APIVersion string            `json:"apiVersion"`
	Items      []releaseResponse `json:"items"`
}

type deploymentList struct {
	Kind       string                      `json:"kind"`
	APIVersion string                      `json:"apiVersion"`
	Items      []modelrepo.DeploymentState `json:"items"`
}

// releaseResponse 是发布对象响应形态：内嵌 ModelRelease（字段平铺）+
// summary 派生汇总（现算，非冗余存储，设计 §4.2）。列表场景不现算
// summary（omitempty：零值省略；详情/创建/取消/回滚响应必带——创建时
// total≥1 恒非零）。
type releaseResponse struct {
	modelrepo.ModelRelease
	Summary modelrelease.NodeSummary `json:"summary,omitempty"`
}

// summaryOf 现算发布 perNode 汇总（getRelease/cancel/rollback/create 响应共用）。
func (a *modelAPI) summaryOf(ctx context.Context, id string) modelrelease.NodeSummary {
	results, err := a.store.ListNodeResults(ctx, id)
	if err != nil {
		return modelrelease.NodeSummary{}
	}
	return modelrelease.SummarizeNodes(results)
}

// newReleaseID 生成发布 ID（32 位十六进制，等价 128 位随机数；生成方式与
// protocol.newID 同源——crypto/rand 首选，熵源失败回退时间戳+进程内计数，
// 设计 §2.3：id 生成方式与 protocol.NewMessage 同源）。
func newReleaseID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	var fb [16]byte
	binary.BigEndian.PutUint64(fb[0:8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint64(fb[8:16], releaseIDFallback.Add(1))
	return hex.EncodeToString(fb[:])
}

// releaseIDFallback 是 crypto/rand 失败时的进程内兜底计数器。
var releaseIDFallback atomic.Uint64

// ── 模型端点（5）──────────────────────────────────────────────────────

// listModels 处理 GET /api/v1/models（v0.8.0 起支持 limit/offset 分页）。
func (a *modelAPI) listModels(w http.ResponseWriter, r *http.Request) {
	pp, err := parsePageParams(r)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	models, err := a.store.ListModels(r.Context())
	if err != nil {
		modelError(w, err)
		return
	}
	page, total := slicePage(models, pp)
	writePageHeaders(w, total)
	items := make([]modelrepo.Model, 0, len(page))
	items = append(items, page...)
	writeJSON(w, http.StatusOK, modelList{Kind: "ModelList", APIVersion: "v1", Items: items})
}

// createModel 处理 POST /api/v1/models。
func (a *modelAPI) createModel(w http.ResponseWriter, r *http.Request) {
	var req createModelRequest
	if err := decodeWriteBody(w, r, &req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large (limit 1MiB)", nil)
			return
		}
		badRequest(w, "invalid json body")
		return
	}
	if err := req.validate(); err != nil {
		badRequest(w, "%v", err)
		return
	}
	model := &modelrepo.Model{Name: req.Name, Description: req.Description, Type: req.Type, Metadata: req.Metadata}
	if err := a.store.CreateModel(r.Context(), model); err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model)
}

// getModel 处理 GET /api/v1/models/{modelName}。
func (a *modelAPI) getModel(w http.ResponseWriter, r *http.Request) {
	model, err := a.store.GetModel(r.Context(), r.PathValue("modelName"))
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model)
}

// updateModel 处理 PUT /api/v1/models/{modelName}（可编辑字段全量替换；
// name/createdAt 不可变）。
func (a *modelAPI) updateModel(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	var req updateModelRequest
	if err := decodeWriteBody(w, r, &req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large (limit 1MiB)", nil)
			return
		}
		badRequest(w, "invalid json body")
		return
	}
	if err := req.validate(); err != nil {
		badRequest(w, "%v", err)
		return
	}
	err := a.store.UpdateModel(r.Context(), modelName, func(m *modelrepo.Model) error {
		m.Description = req.Description
		m.Type = req.Type
		m.Metadata = req.Metadata
		return nil
	})
	if err != nil {
		modelError(w, err)
		return
	}
	updated, err := a.store.GetModel(r.Context(), modelName)
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// deleteModel 处理 DELETE /api/v1/models/{modelName}（级联删除 versions +
// deployments + meta；前置：无 active 版本、无在途发布）。
func (a *modelAPI) deleteModel(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	if err := a.store.DeleteModel(r.Context(), modelName); err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "model": modelName})
}

// ── 版本端点（6）──────────────────────────────────────────────────────

// listVersions 处理 GET /api/v1/models/{modelName}/versions。
func (a *modelAPI) listVersions(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	pp, err := parsePageParams(r)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	versions, err := a.store.ListVersions(r.Context(), modelName)
	if err != nil {
		modelError(w, err)
		return
	}
	page, total := slicePage(versions, pp)
	writePageHeaders(w, total)
	items := make([]modelrepo.ModelVersion, 0, len(page))
	items = append(items, page...)
	writeJSON(w, http.StatusOK, versionList{Kind: "ModelVersionList", APIVersion: "v1", Items: items})
}

// createVersion 处理 POST /api/v1/models/{modelName}/versions（初始 draft，
// POST 不激活——激活是显式动作）。
func (a *modelAPI) createVersion(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	var req createVersionRequest
	if err := decodeWriteBody(w, r, &req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large (limit 1MiB)", nil)
			return
		}
		badRequest(w, "invalid json body")
		return
	}
	if err := req.validate(); err != nil {
		badRequest(w, "%v", err)
		return
	}
	version := &modelrepo.ModelVersion{
		Model: modelName, Version: req.Version, Mirror: req.Mirror, Sha256: req.Sha256,
		SizeBytes: req.SizeBytes, Archs: req.Archs, Metadata: req.Metadata,
	}
	if err := version.Sanitize(); err != nil {
		badRequest(w, "%v", err)
		return
	}
	if err := a.store.CreateVersion(r.Context(), version); err != nil {
		modelError(w, err)
		return
	}
	// 读回存储对象（status 由实现填充为 draft，响应完整字段）
	created, err := a.store.GetVersion(r.Context(), modelName, req.Version)
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

// getVersion 处理 GET /api/v1/models/{modelName}/versions/{version}。
func (a *modelAPI) getVersion(w http.ResponseWriter, r *http.Request) {
	modelName, version := r.PathValue("modelName"), r.PathValue("version")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	if err := modelrepo.ValidateVersionTag(version); err != nil {
		badRequest(w, "%v", err)
		return
	}
	v, err := a.store.GetVersion(r.Context(), modelName, version)
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// deleteVersion 处理 DELETE /api/v1/models/{modelName}/versions/{version}
// （仅 draft/archived；active → 409）。
func (a *modelAPI) deleteVersion(w http.ResponseWriter, r *http.Request) {
	modelName, version := r.PathValue("modelName"), r.PathValue("version")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	if err := modelrepo.ValidateVersionTag(version); err != nil {
		badRequest(w, "%v", err)
		return
	}
	if err := a.store.DeleteVersion(r.Context(), modelName, version); err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "model": modelName, "version": version})
}

// activateVersion 处理 POST /api/v1/models/{modelName}/versions/{version}/activate
// （draft→active，自动降级旧 active；archived 不可再激活）。
func (a *modelAPI) activateVersion(w http.ResponseWriter, r *http.Request) {
	modelName, version := r.PathValue("modelName"), r.PathValue("version")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	if err := modelrepo.ValidateVersionTag(version); err != nil {
		badRequest(w, "%v", err)
		return
	}
	if err := a.store.ActivateVersion(r.Context(), modelName, version); err != nil {
		modelError(w, err)
		return
	}
	v, err := a.store.GetVersion(r.Context(), modelName, version)
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// archiveVersion 处理 POST /api/v1/models/{modelName}/versions/{version}/archive
// （active→archived；指向该版本的 pending/running 发布 → 409）。
func (a *modelAPI) archiveVersion(w http.ResponseWriter, r *http.Request) {
	modelName, version := r.PathValue("modelName"), r.PathValue("version")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	if err := modelrepo.ValidateVersionTag(version); err != nil {
		badRequest(w, "%v", err)
		return
	}
	if err := a.store.ArchiveVersion(r.Context(), modelName, version); err != nil {
		modelError(w, err)
		return
	}
	v, err := a.store.GetVersion(r.Context(), modelName, version)
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ── 发布端点（5）──────────────────────────────────────────────────────

// createRelease 处理 POST /api/v1/models/{modelName}/releases（202 异步
// 执行；设计 §5.2 前置校验依次执行 + §5.3 物化语义）。
func (a *modelAPI) createRelease(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	var req createReleaseRequest
	if err := decodeWriteBody(w, r, &req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large (limit 1MiB)", nil)
			return
		}
		badRequest(w, "invalid json body")
		return
	}
	req.applyDefaults()
	// v0.17.0：dryRun 预检分支——全量走真实创建校验链（模型 404/内容 400/
	// 版本 active 422/探活/节点物化 422/guard 等价只读判定），但绝不落盘、
	// 不占 guard、不预写 perNode；响应 200 + wouldCreate 摘要（**非承诺语义**，
	// TOCTOU：预检通过后资源状态仍可变化，真实创建以 CreateRelease CAS 兑现）。
	if req.DryRun {
		a.dryRunCreateRelease(w, r, modelName, &req)
		return
	}
	// 1) 模型存在（404）
	if _, err := a.store.GetModel(r.Context(), modelName); err != nil {
		modelError(w, err)
		return
	}
	// 2) 内容校验（400 族：target 白名单/percentage 越界/nodeIDs 字符集/
	//    batchSize≥1/pauseBetween≥0/notBeforeMs≥0），via ValidateCreate（设计 §5.2 step3）
	pre := &modelrepo.ModelRelease{Model: modelName, Version: req.Version,
		Target: req.Target, BatchSize: req.BatchSize, PauseBetween: req.PauseBetween,
		NotBeforeMs: req.NotBeforeMs, FailureBudget: req.FailureBudget}
	if err := pre.ValidateCreate(); err != nil {
		badRequest(w, "%v", err)
		return
	}
	// v0.16.0：窗口不允许早于 now-5min（防钟漂误触即时启动；>now 正常预约）
	if req.NotBeforeMs > 0 && req.NotBeforeMs < time.Now().UnixMilli()-5*60*1000 {
		badRequest(w, "notBeforeMs is too far in the past (clock drift guard; use 0 for immediate)")
		return
	}
	// 3) 目标版本存在且须 active（否则 422）
	version, err := a.store.GetVersion(r.Context(), modelName, req.Version)
	if err != nil {
		modelError(w, err)
		return
	}
	if version.Status != modelrepo.VersionStatusActive {
		writeErr(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("release target version %s/%s must be active (status=%s)", modelName, req.Version, version.Status), nil)
		return
	}
	// R-1（v0.9.0）：发布前镜像存在性探活（默认 off；warn 仅告警/fail 阻断 422）
	digest, block, err := a.mirrorCheck.check(r.Context(), version.Mirror)
	if err != nil {
		if block {
			writeErr(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("release mirror check failed: %v", err),
				map[string]any{"mirror": version.Mirror})
			return
		}
		log.Warnf("[modelrelease] 镜像探活失败（warn 模式，不阻断发布，mirror=%s）: %v", version.Mirror, err)
	}
	// 4) 物化 TargetNodes（设计 §5.2 step4/§5.3；未知节点/无 Ready → 422）
	targetNodes, err := a.materializeTargetNodes(req.Target, version)
	if err != nil {
		var une *modelrepo.UnknownNodesError
		if errors.As(err, &une) || errors.Is(err, modelrepo.ErrNoReadyNodes) {
			modelError(w, err) // 422 带 unknownNodes/no ready nodes
			return
		}
		badRequest(w, "%v", err)
		return
	}
	// 5) PrevActive = 目标版本激活时被降级的版本（ActivateVersion 写入的
	//    version.PrevActive，API 链路"先激活后发布"下回滚链正确的来源）；
	//    为空（旧数据/直接 store 调用）回退旧公式：当前 active ≠ 目标时用
	//    active 版本。
	prevActive := version.PrevActive
	if prevActive == "" {
		if cur, err := a.store.ActiveVersion(r.Context(), modelName); err == nil && cur != nil && cur.Version != req.Version {
			prevActive = cur.Version
		}
	}
	// 6) guard + release 头键 + perNode pending 预写（存储层；202 返回）
	nowCreated := time.Now().UnixMilli()
	release := &modelrepo.ModelRelease{
		MirrorDigest: digest,
		ID:           newReleaseID(), Model: modelName, Version: req.Version,
		Target: req.Target, TargetNodes: targetNodes,
		BatchSize: req.BatchSize, PauseBetween: req.PauseBetween,
		FailFast: *req.FailFast, PrevActive: prevActive,
		NotBeforeMs:   req.NotBeforeMs,
		FailureBudget: req.FailureBudget,
	}
	// v0.18.0：时间线首事件 created（后续流转点由控制器/存储层追加）
	release.AppendEvent(modelrepo.EventCreated, "created via API", nowCreated)
	if err := a.store.CreateRelease(r.Context(), release); err != nil {
		modelError(w, err)
		return
	}
	batchSize := release.BatchSize
	for i, nodeID := range release.TargetNodes {
		if err := a.store.SetNodeResult(r.Context(), release.ID, nodeID, &modelrepo.NodeReleaseResult{
			NodeID: nodeID, Status: modelrepo.NodeRelPending, Batch: modelrelease.BatchNumber(i, batchSize),
		}); err != nil {
			// perNode 预写失败：release 头已持久化（写穿），控制器对缺失
			// perNode 按 pending 处理（firstUnexecutedBatch），仅记日志
			log.Warnf("[modelAPI] 发布 %s perNode 预写失败（node=%s，控制器按 pending 收敛）: %v",
				release.ID, nodeID, err)
		}
	}
	log.Infof("[modelAPI] 创建发布 %s（model=%s version=%s target=%s nodes=%d batchSize=%d prevActive=%q）",
		release.ID, modelName, req.Version, req.Target.Type, len(targetNodes), release.BatchSize, prevActive)
	writeJSON(w, http.StatusAccepted, releaseResponse{
		ModelRelease: *release,
		Summary:      modelrelease.NodeSummary{Total: len(targetNodes), Pending: len(targetNodes)},
	})
}

// materializeTargetNodes 物化目标节点（设计 §5.2 step4）：
//   - nodeIDs：保持用户给定顺序（去重后）；全部经 reg.Get 校验存在
//     （含离线节点——运行时按离线处理）；未知 → *UnknownNodesError（422）；
//   - percentage：Ready 快照 = 进程内注册表 List 过滤 Status==Ready；
//     version.Archs 非空 → 先按 arch 过滤（NodeRef 本地小结构体，不
//     import registry 于 plan 层）；再 SelectPercentageNodesByArch
//     （字典序前 n，上取整；0 台 Ready / 过滤后 0 台 → ErrNoReadyNodes 422）。
func (a *modelAPI) materializeTargetNodes(target modelrepo.ReleaseTarget, version *modelrepo.ModelVersion) ([]string, error) {
	switch target.Type {
	case "nodeIDs":
		seen := make(map[string]bool, len(target.NodeIDs))
		ordered := make([]string, 0, len(target.NodeIDs))
		var unknown []string
		for _, id := range target.NodeIDs {
			if seen[id] {
				continue // 去重保持原序
			}
			seen[id] = true
			if _, ok := a.reg.Get(id); !ok {
				unknown = append(unknown, id)
				continue
			}
			ordered = append(ordered, id)
		}
		if len(unknown) > 0 {
			return nil, &modelrepo.UnknownNodesError{Nodes: unknown}
		}
		if len(ordered) == 0 {
			return nil, fmt.Errorf("target.nodeIDs 去重后为空")
		}
		return ordered, nil
	case "percentage":
		ready := make([]modelrelease.NodeRef, 0)
		for _, info := range a.reg.List() {
			if info.Status == registry.StatusReady {
				ready = append(ready, modelrelease.NodeRef{NodeID: info.NodeID, Arch: info.Arch})
			}
		}
		return modelrelease.SelectPercentageNodesByArch(ready, target.Percentage, version.Archs)
	}
	return nil, fmt.Errorf("invalid target.type %q", target.Type)
}

// listReleases 处理 GET /api/v1/models/{modelName}/releases（模型不存在
// → 404；按 createdAt 升序）。
func (a *modelAPI) listReleases(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	if _, err := a.store.GetModel(r.Context(), modelName); err != nil {
		modelError(w, err) // 子资源随模型 404
		return
	}
	pp, err := parsePageParams(r)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	// v0.17.0：status 过滤（逗号多值；过滤后再分页，X-Total-Count 报过滤后总数）
	statusAllow, err := parseStatusFilter(r.URL.Query().Get("status"))
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	releases, err := a.store.ListReleases(r.Context(), modelName)
	if err != nil {
		modelError(w, err)
		return
	}
	releases = filterReleasesByStatus(releases, statusAllow)
	page, total := slicePage(releases, pp)
	writePageHeaders(w, total)
	items := make([]releaseResponse, 0, len(page))
	for i := range page {
		items = append(items, releaseResponse{ModelRelease: page[i]})
	}
	writeJSON(w, http.StatusOK, releaseList{Kind: "ModelReleaseList", APIVersion: "v1", Items: items})
}

// getRelease 处理 GET /api/v1/models/{modelName}/releases/{releaseID}
// （含 summary 派生汇总）。
func (a *modelAPI) getRelease(w http.ResponseWriter, r *http.Request) {
	release, err := a.store.GetRelease(r.Context(), r.PathValue("releaseID"))
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, releaseResponse{
		ModelRelease: *release,
		Summary:      a.summaryOf(r.Context(), release.ID),
	})
}

// releaseDigestReport 是 GET .../releases/{releaseID}/digest 的响应
// （v0.12.0，D-1 发布 digest 复核）：发布期望 digest + 逐节点当前 digest +
// 一致结论。目的：KNOWN-ISSUES §11 残余②「终态后晚到 mismatch 不回写」的
// 运维通道——一键对比 release.mirrorDigest 与各节点当前上报 imageDigest，
// 免人工双端点比对（处置仍为人工回滚/重发，审计稳定不变）。
type releaseDigestReport struct {
	Kind         string                  `json:"kind"`
	APIVersion   string                  `json:"apiVersion"`
	ReleaseID    string                  `json:"releaseID"`
	Model        string                  `json:"model"`
	Version      string                  `json:"version"`
	Status       modelrepo.ReleaseStatus `json:"status"`
	MirrorDigest string                  `json:"mirrorDigest,omitempty"`
	Consistency  string                  `json:"consistency"` // skipped|consistent|inconsistent|unknown
	GeneratedAt  int64                   `json:"generatedAt"`
	Nodes        []releaseDigestNode     `json:"nodes"` // 恒非 nil（空发布 → []）
}

type releaseDigestNode struct {
	NodeID             string                  `json:"nodeID"`
	ReleaseStatus      modelrepo.NodeRelStatus `json:"releaseStatus"`
	CurrentImageDigest string                  `json:"currentImageDigest,omitempty"`
	Consistency        string                  `json:"consistency"` // skipped|consistent|inconsistent|unknown
}

// consistencyOf 逐节点 digest 一致性判定（纯函数，可单测）：
// expected 空 → "skipped"（发布级未启用 digest 校验）；current 空 →
// "unknown"（节点侧缺失：未上报/无 Pod/无运行时采集）；相等 → "consistent"；
// 否则 → "inconsistent"。
func consistencyOf(expected, current string) string {
	if expected == "" {
		return "skipped"
	}
	if current == "" {
		return "unknown"
	}
	if current == expected {
		return "consistent"
	}
	return "inconsistent"
}

// getReleaseDigest 处理 GET /api/v1/models/{modelName}/releases/{releaseID}/digest
// （v0.12.0，D-1 发布 digest 复核）。发布不存在 → 404（modelError 既有语义）；
// 任意状态可查（pending/running 为进行中视图，status 字段明示）。
// head 聚合：mirrorDigest 空 → skipped；任一节点 inconsistent → inconsistent；
// 任一节点 unknown → unknown；否则 consistent。
func (a *modelAPI) getReleaseDigest(w http.ResponseWriter, r *http.Request) {
	release, err := a.store.GetRelease(r.Context(), r.PathValue("releaseID"))
	if err != nil {
		modelError(w, err)
		return
	}
	results, err := a.store.ListNodeResults(r.Context(), release.ID)
	if err != nil {
		modelError(w, err)
		return
	}
	byNode := make(map[string]modelrepo.NodeReleaseResult, len(results))
	for _, nr := range results {
		byNode[nr.NodeID] = nr
	}
	nodes := make([]releaseDigestNode, 0, len(release.TargetNodes))
	anyInconsistent, anyUnknown := false, false
	for _, nodeID := range release.TargetNodes {
		cur := ""
		if a.digestOf != nil {
			cur = a.digestOf(nodeID)
		}
		cons := consistencyOf(release.MirrorDigest, cur)
		st := modelrepo.NodeRelPending
		if nr, ok := byNode[nodeID]; ok {
			st = nr.Status
		}
		nodes = append(nodes, releaseDigestNode{
			NodeID:             nodeID,
			ReleaseStatus:      st,
			CurrentImageDigest: cur,
			Consistency:        cons,
		})
		switch cons {
		case "inconsistent":
			anyInconsistent = true
		case "unknown":
			anyUnknown = true
		}
	}
	head := "consistent"
	switch {
	case release.MirrorDigest == "":
		head = "skipped"
	case anyInconsistent:
		head = "inconsistent"
	case anyUnknown:
		head = "unknown"
	}
	writeJSON(w, http.StatusOK, releaseDigestReport{
		Kind:         "ReleaseDigestReport",
		APIVersion:   "v1",
		ReleaseID:    release.ID,
		Model:        release.Model,
		Version:      release.Version,
		Status:       release.Status,
		MirrorDigest: release.MirrorDigest,
		Consistency:  head,
		GeneratedAt:  time.Now().UnixMilli(),
		Nodes:        nodes,
	})
}

// cancelRelease 处理 POST /api/v1/models/{modelName}/releases/{releaseID}/cancel
// （pending/running → canceled；终态 → 409）。
func (a *modelAPI) cancelRelease(w http.ResponseWriter, r *http.Request) {
	releaseID := r.PathValue("releaseID")
	if err := a.store.CancelRelease(r.Context(), releaseID); err != nil {
		modelError(w, err)
		return
	}
	a.appendEvent(r, releaseID, modelrepo.EventCancelled, "cancelled via API")
	release, err := a.store.GetRelease(r.Context(), releaseID)
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, releaseResponse{
		ModelRelease: *release,
		Summary:      a.summaryOf(r.Context(), release.ID),
	})
}

// pauseRelease 处理 POST /api/v1/models/{modelName}/releases/{releaseID}/pause
// （v0.16.0）：running → paused（存储层守卫：仅 running 可暂停，重复幂等；
// pending/终态 → 409 族）。200 返回最新 release + summary。
func (a *modelAPI) pauseRelease(w http.ResponseWriter, r *http.Request) {
	releaseID := r.PathValue("releaseID")
	if err := a.store.PauseRelease(r.Context(), releaseID); err != nil {
		modelError(w, err)
		return
	}
	a.appendEvent(r, releaseID, modelrepo.EventPaused, "paused via API")
	release, err := a.store.GetRelease(r.Context(), releaseID)
	if err != nil {
		modelError(w, err)
		return
	}
	log.Infof("[modelAPI] 发布 %s 已暂停（model=%s version=%s）", release.ID, release.Model, release.Version)
	writeJSON(w, http.StatusOK, releaseResponse{
		ModelRelease: *release,
		Summary:      a.summaryOf(r.Context(), release.ID),
	})
}

// resumeRelease 处理 POST /api/v1/models/{modelName}/releases/{releaseID}/resume
// （v0.16.0）：paused → running（非 paused → 409）；NextBatchAt 保持原值。
func (a *modelAPI) resumeRelease(w http.ResponseWriter, r *http.Request) {
	releaseID := r.PathValue("releaseID")
	if err := a.store.ResumeRelease(r.Context(), releaseID); err != nil {
		modelError(w, err)
		return
	}
	a.appendEvent(r, releaseID, modelrepo.EventResumed, "resumed via API")
	release, err := a.store.GetRelease(r.Context(), releaseID)
	if err != nil {
		modelError(w, err)
		return
	}
	log.Infof("[modelAPI] 发布 %s 已恢复（model=%s version=%s）", release.ID, release.Model, release.Version)
	writeJSON(w, http.StatusOK, releaseResponse{
		ModelRelease: *release,
		Summary:      a.summaryOf(r.Context(), release.ID),
	})
}

// ── 模型目录导出/导入（v0.16.0，2 端点）──────────────────────────

// ModelCatalog 是导出/导入同构的目录快照（纯云端台账；不含 releases/
// deployments/guards——发布任务与环境绑定、影子是派生台账，均不可迁移）。
type ModelCatalog struct {
	SchemaVersion string                   `json:"schemaVersion"` // 恒 "1"
	ExportedAt    int64                    `json:"exportedAt"`    // Unix 毫秒
	Models        []modelrepo.Model        `json:"models"`
	Versions      []modelrepo.ModelVersion `json:"versions"`
}

// exportCatalog 处理 GET /api/v1/models/export：全量模型+版本 JSON 快照下载。
// 恒 200（空目录 → models/versions = []）。Content-Disposition 附文件名提示。
func (a *modelAPI) exportCatalog(w http.ResponseWriter, r *http.Request) {
	models, err := a.store.ListModels(r.Context())
	if err != nil {
		modelError(w, err)
		return
	}
	cat := ModelCatalog{
		SchemaVersion: "1",
		ExportedAt:    time.Now().UnixMilli(),
		Models:        []modelrepo.Model{},
		Versions:      []modelrepo.ModelVersion{},
	}
	for i := range models {
		m := models[i]
		cat.Models = append(cat.Models, m)
		vs, verr := a.store.ListVersions(r.Context(), m.Name)
		if verr != nil {
			continue // 导出尽力而为：单模型版本读失败不中断整体（记日志）
		}
		cat.Versions = append(cat.Versions, vs...)
	}
	w.Header().Set("Content-Disposition", `attachment; filename="edgeflow-model-catalog.json"`)
	writeJSON(w, http.StatusOK, cat)
}

// importReport 是 POST /api/v1/models/import 的响应计数。
type importReport struct {
	Kind             string `json:"kind"`
	APIVersion       string `json:"apiVersion"`
	ModelsImported   int    `json:"modelsImported"`   // 新建模型数
	ModelsUpdated    int    `json:"modelsUpdated"`    // 元数据覆盖更新数
	VersionsImported int    `json:"versionsImported"` // 新建版本数（含 active 补齐）
	VersionsSkipped  int    `json:"versionsSkipped"`  // 同 (model,version) 已存在而跳过
}

// importCatalog 处理 POST /api/v1/models/import：目录快照幂等 upsert。
// 幂等律：模型存在 → 元数据整表覆盖；版本已存在 → 跳过计数（不覆盖本地
// 状态机演进）；active 版本经 CreateVersion(draft)+ActivateVersion 直通。
func (a *modelAPI) importCatalog(w http.ResponseWriter, r *http.Request) {
	var cat ModelCatalog
	if err := decodeWriteBody(w, r, &cat); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large (limit 1MiB)", nil)
			return
		}
		badRequest(w, "invalid json body")
		return
	}
	if cat.SchemaVersion == "" {
		cat.SchemaVersion = "1"
	}
	if cat.SchemaVersion != "1" {
		badRequest(w, "unsupported catalog schemaVersion %q (only \"1\")", cat.SchemaVersion)
		return
	}
	rep := importReport{Kind: "importReport", APIVersion: "v1alpha1"}
	for i := range cat.Models {
		m := cat.Models[i]
		if err := modelrepo.ValidateModelName(m.Name); err != nil {
			badRequest(w, "invalid model name in catalog: %v", err)
			return
		}
		_, gerr := a.store.GetModel(r.Context(), m.Name)
		if gerr == nil {
			err := a.store.UpdateModel(r.Context(), m.Name, func(cur *modelrepo.Model) error {
				cur.Description = m.Description
				cur.Type = m.Type
				if len(m.Metadata) > 0 {
					cur.Metadata = m.Metadata
				}
				return nil
			})
			if err != nil {
				modelError(w, err)
				return
			}
			rep.ModelsUpdated++
			continue
		}
		if !errors.Is(gerr, modelrepo.ErrModelNotFound) {
			modelError(w, gerr)
			return
		}
		created := m
		now := time.Now().UnixMilli()
		if created.CreatedAt == 0 {
			created.CreatedAt = now
		}
		if created.UpdatedAt == 0 {
			created.UpdatedAt = now
		}
		if err := a.store.CreateModel(r.Context(), &created); err != nil {
			modelError(w, err)
			return
		}
		rep.ModelsImported++
	}
	for i := range cat.Versions {
		v := cat.Versions[i]
		if err := modelrepo.ValidateModelName(v.Model); err != nil {
			badRequest(w, "invalid model name in catalog versions: %v", err)
			return
		}
		if err := modelrepo.ValidateVersionTag(v.Version); err != nil {
			badRequest(w, "invalid version tag in catalog: %v", err)
			return
		}
		if !v.Status.IsValid() {
			badRequest(w, "invalid version status %q", v.Status)
			return
		}
		// 模型缺失时自动补建空壳（versions 段自洽保障）
		if _, gerr := a.store.GetModel(r.Context(), v.Model); errors.Is(gerr, modelrepo.ErrModelNotFound) {
			now := time.Now().UnixMilli()
			shell := modelrepo.Model{Name: v.Model, Description: "(imported shell for orphan version)", CreatedAt: now, UpdatedAt: now}
			if cerr := a.store.CreateModel(r.Context(), &shell); cerr != nil && !errors.Is(cerr, modelrepo.ErrModelExists) {
				modelError(w, cerr)
				return
			}
		} else if gerr != nil {
			modelError(w, gerr)
			return
		}
		if _, gerr := a.store.GetVersion(r.Context(), v.Model, v.Version); gerr == nil {
			rep.VersionsSkipped++ // 幂等：不覆盖本地状态机演进
			continue
		} else if !errors.Is(gerr, modelrepo.ErrVersionNotFound) {
			modelError(w, gerr)
			return
		}
		ins := v
		now := time.Now().UnixMilli()
		if ins.CreatedAt == 0 {
			ins.CreatedAt = now
		}
		if ins.UpdatedAt == 0 {
			ins.UpdatedAt = now
		}
		status := ins.Status
		ins.Status = modelrepo.VersionStatusDraft // CreateVersion 强制初始态
		if err := a.store.CreateVersion(r.Context(), &ins); err != nil {
			if errors.Is(err, modelrepo.ErrVersionExists) {
				rep.VersionsSkipped++
				continue
			}
			modelError(w, err)
			return
		}
		rep.VersionsImported++
		if status == modelrepo.VersionStatusActive {
			if aerr := a.store.ActivateVersion(r.Context(), v.Model, v.Version); aerr != nil {
				modelError(w, aerr)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, rep)
}

// rollbackRelease 处理 POST /api/v1/models/{modelName}/releases/{releaseID}/rollback
// （设计 §5.5 前置守卫全量在存储层 RequestRollback：422 无 PrevActive /
// 409 pending 或已回滚或版本被新发布接管；通过 → 202 + rollbackRequested=true，
// 控制器下一轮异步逆序执行）。
func (a *modelAPI) rollbackRelease(w http.ResponseWriter, r *http.Request) {
	releaseID := r.PathValue("releaseID")
	if err := a.store.RequestRollback(r.Context(), releaseID); err != nil {
		modelError(w, err)
		return
	}
	release, err := a.store.GetRelease(r.Context(), releaseID)
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, releaseResponse{
		ModelRelease: *release,
		Summary:      a.summaryOf(r.Context(), release.ID),
	})
}

// ── 部署影子端点（1）──────────────────────────────────────────────────

// listDeployments 处理 GET /api/v1/models/{modelName}/deployments
// （版本—节点—时间追踪台账；模型不存在 → 404）。
// v0.13.0（A′）：分页与 releases/versions/models 同构——limit(1-1000)/
// offset(≥0) query 参数 + X-Total-Count 头；缺省全量（零破坏）。
func (a *modelAPI) listDeployments(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	pp, err := parsePageParams(r)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	deployments, err := a.store.ListDeployments(r.Context(), modelName)
	if err != nil {
		modelError(w, err)
		return
	}
	page, total := slicePage(deployments, pp)
	writePageHeaders(w, total)
	items := make([]modelrepo.DeploymentState, 0, len(page))
	items = append(items, page...)
	writeJSON(w, http.StatusOK, deploymentList{Kind: "DeploymentList", APIVersion: "v1", Items: items})
}
