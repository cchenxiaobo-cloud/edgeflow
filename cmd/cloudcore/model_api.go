// 模型仓库 API（v0.7.0，WBS-7）：17 个新 REST 端点
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
}

// Register 一次性注册 17 条模型 API 路由（WBS-7 装配面最小：main.go 在
// 既有 11 条之后一行调用）。
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
	mux.HandleFunc("GET /api/v1/models/{modelName}/releases/{releaseID}", a.getRelease)
	mux.HandleFunc("POST /api/v1/models/{modelName}/releases/{releaseID}/cancel", a.cancelRelease)
	mux.HandleFunc("POST /api/v1/models/{modelName}/releases/{releaseID}/rollback", a.rollbackRelease)
	mux.HandleFunc("GET /api/v1/models/{modelName}/deployments", a.listDeployments)
}

// ── 响应/错误辅助 ──────────────────────────────────────────────────────

const jsonContentType = "application/json; charset=utf-8"

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

// listModels 处理 GET /api/v1/models。
func (a *modelAPI) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := a.store.ListModels(r.Context())
	if err != nil {
		modelError(w, err)
		return
	}
	items := make([]modelrepo.Model, 0, len(models))
	items = append(items, models...)
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
	versions, err := a.store.ListVersions(r.Context(), modelName)
	if err != nil {
		modelError(w, err)
		return
	}
	items := make([]modelrepo.ModelVersion, 0, len(versions))
	items = append(items, versions...)
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
	// 1) 模型存在（404）
	if _, err := a.store.GetModel(r.Context(), modelName); err != nil {
		modelError(w, err)
		return
	}
	// 2) 内容校验（400 族：target 白名单/percentage 越界/nodeIDs 字符集/
	//    batchSize≥1/pauseBetween≥0），via ValidateCreate（设计 §5.2 step3）
	pre := &modelrepo.ModelRelease{Model: modelName, Version: req.Version,
		Target: req.Target, BatchSize: req.BatchSize, PauseBetween: req.PauseBetween}
	if err := pre.ValidateCreate(); err != nil {
		badRequest(w, "%v", err)
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
	// 5) PrevActive = 当前 active 版本（≠ 目标版本时记录；无则 ""）
	prevActive := ""
	if cur, err := a.store.ActiveVersion(r.Context(), modelName); err == nil && cur != nil && cur.Version != req.Version {
		prevActive = cur.Version
	}
	// 6) guard + release 头键 + perNode pending 预写（存储层；202 返回）
	release := &modelrepo.ModelRelease{
		ID: newReleaseID(), Model: modelName, Version: req.Version,
		Target: req.Target, TargetNodes: targetNodes,
		BatchSize: req.BatchSize, PauseBetween: req.PauseBetween,
		FailFast: *req.FailFast, PrevActive: prevActive,
	}
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
	releases, err := a.store.ListReleases(r.Context(), modelName)
	if err != nil {
		modelError(w, err)
		return
	}
	items := make([]releaseResponse, 0, len(releases))
	for i := range releases {
		items = append(items, releaseResponse{ModelRelease: releases[i]})
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

// cancelRelease 处理 POST /api/v1/models/{modelName}/releases/{releaseID}/cancel
// （pending/running → canceled；终态 → 409）。
func (a *modelAPI) cancelRelease(w http.ResponseWriter, r *http.Request) {
	releaseID := r.PathValue("releaseID")
	if err := a.store.CancelRelease(r.Context(), releaseID); err != nil {
		modelError(w, err)
		return
	}
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
func (a *modelAPI) listDeployments(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	if err := modelrepo.ValidateModelName(modelName); err != nil {
		badRequest(w, "%v", err)
		return
	}
	deployments, err := a.store.ListDeployments(r.Context(), modelName)
	if err != nil {
		modelError(w, err)
		return
	}
	items := make([]modelrepo.DeploymentState, 0, len(deployments))
	items = append(items, deployments...)
	writeJSON(w, http.StatusOK, deploymentList{Kind: "DeploymentList", APIVersion: "v1", Items: items})
}
