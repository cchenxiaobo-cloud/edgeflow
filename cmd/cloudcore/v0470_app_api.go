// v0470_app_api.go —— v0.47.0（spec 0020）云端装配：应用目录 API（US-1，6 端点）、
// 应用灰度部署 API（US-2，6 端点）、应用包上传/下载（US-3，2 端点）。
//
// 装配口径：
//   - appstore：EDGEFLOW_CLOUDCORE_APPSTORE_FILE 配置 → 持久化；未配置 → 纯内存；
//   - 应用包目录：EDGEFLOW_CLOUDCORE_APPPKG_DIR（上传落盘 sha256 命名）；
//   - 灰度控制器：goroutine（扫描周期 EDGEFLOW_CLOUDCORE_APPDEPLOY_SCAN_SEC，
//     默认 2s）；交付执行器 appDeployer（PodSync/ConfigSync 组合，main.go 注入）；
//   - admin-only 面：全部沿 RBAC facet 判定（applications/app-deployments 非
//     roles 面，operator 可写、viewer 只读——与模型面同语义）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"edgeflow/cloud/pkg/appdeploy"
	"edgeflow/cloud/pkg/appstore"
	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// 应用包大小上限（spec 0020 §3 边界 ④）。
const appPkgMaxBytes = 32 << 20 // 32MB

// 全局装配句柄（main.go 赋值；v0470 API 注册使用）。
var (
	appStore     *appstore.Store
	appDeployCtl *appdeploy.Controller
	appPkgDir    string
)

// appPkgPath 返回应用包落盘路径（<pkgDir>/<app>/<version>.tar）。
// app/version 双重 Base 收敛（纵深防御：注册面字符集校验之外，路径拼接处
// 再收一道——目录逃逸即失效）。
func appPkgPath(app, version string) string {
	return filepath.Join(appPkgDir, filepath.Base(app), filepath.Base(version)+".tar")
}

// registerAppAPIs 挂 v0.47.0 应用面 API（13 端点）。
func registerAppAPIs(mux *http.ServeMux) {
	// ── US-1 应用目录（6）──
	mux.HandleFunc("POST /api/v1/applications", apiCreateApplication)
	mux.HandleFunc("GET /api/v1/applications", apiListApplications)
	mux.HandleFunc("GET /api/v1/applications/{name}", apiGetApplication)
	mux.HandleFunc("DELETE /api/v1/applications/{name}", apiDeleteApplication)
	mux.HandleFunc("POST /api/v1/applications/{name}/versions", apiAddAppVersion)
	mux.HandleFunc("GET /api/v1/applications/{name}/versions", apiListAppVersions)

	// ── US-3 应用包（2）──
	mux.HandleFunc("PUT /api/v1/applications/{name}/versions/{version}/package", apiPutAppPackage)
	mux.HandleFunc("GET /api/v1/applications/{name}/versions/{version}/package", apiGetAppPackage)

	// ── US-2 灰度部署（5 + pause/resume/cancel/rollback 4 = 6）──
	mux.HandleFunc("POST /api/v1/app-deployments", apiCreateAppDeployment)
	mux.HandleFunc("GET /api/v1/app-deployments", apiListAppDeployments)
	mux.HandleFunc("GET /api/v1/app-deployments/{planID}", apiGetAppDeployment)
	mux.HandleFunc("POST /api/v1/app-deployments/{planID}/pause", apiPauseAppDeployment)
	mux.HandleFunc("POST /api/v1/app-deployments/{planID}/resume", apiResumeAppDeployment)
	mux.HandleFunc("POST /api/v1/app-deployments/{planID}/cancel", apiCancelAppDeployment)
	mux.HandleFunc("POST /api/v1/app-deployments/{planID}/rollback", apiRollbackAppDeployment)
}

// ---- US-1 应用目录 ----

type appVersionReq struct {
	Version   string            `json:"version"`
	Image     string            `json:"image"`
	ScriptRef string            `json:"scriptRef"`
	Metadata  map[string]string `json:"metadata"`
}

func apiCreateApplication(w http.ResponseWriter, r *http.Request) {
	if appStore == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用目录未启用"})
		return
	}
	var body struct {
		Name         string         `json:"name"`
		DisplayName  string         `json:"displayName"`
		Description  string         `json:"description"`
		Kind         string         `json:"kind"`
		FirstVersion *appVersionReq `json:"firstVersion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeV0470JSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	var fv *appstore.Version
	if body.FirstVersion != nil {
		fv = &appstore.Version{Version: body.FirstVersion.Version, Image: body.FirstVersion.Image,
			ScriptRef: body.FirstVersion.ScriptRef, Metadata: body.FirstVersion.Metadata}
	}
	a, err := appStore.Create(body.Name, body.DisplayName, body.Description, body.Kind, time.Now().UnixMilli(), fv)
	if err == appstore.ErrExists {
		writeV0470JSON(w, http.StatusConflict, map[string]string{"error": "application already exists"})
		return
	}
	if err != nil {
		writeV0470JSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Infof("[appstore] 应用注册：%s（%s）", a.Name, a.Kind)
	writeV0470JSON(w, http.StatusOK, a)
}

func apiListApplications(w http.ResponseWriter, r *http.Request) {
	if appStore == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用目录未启用"})
		return
	}
	all := appStore.List()
	limit, offset := v0470Page(r, len(all))
	items := all[offset : offset+limit]
	w.Header().Set("X-Total-Count", strconv.Itoa(len(all)))
	writeV0470JSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// v0470Page 解析分页参数（limit 默认 50 上限 200；offset ≥0 裁剪）。
func v0470Page(r *http.Request, total int) (limit, offset int) {
	limit, offset = 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	if offset > total {
		offset = total
	}
	if offset+limit > total {
		limit = total - offset
	}
	return limit, offset
}

func apiGetApplication(w http.ResponseWriter, r *http.Request) {
	if appStore == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用目录未启用"})
		return
	}
	a, ok := appStore.Get(r.PathValue("name"))
	if !ok {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "application not found"})
		return
	}
	writeV0470JSON(w, http.StatusOK, a)
}

func apiDeleteApplication(w http.ResponseWriter, r *http.Request) {
	if appStore == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用目录未启用"})
		return
	}
	name := r.PathValue("name")
	// 有进行中部署 → 409（spec §2 US-1）
	for _, p := range appDeployCtl.List() {
		if strings.EqualFold(p.App, name) && !appdeploy.Terminal(p.State) {
			writeV0470JSON(w, http.StatusConflict, map[string]string{
				"error": fmt.Sprintf("存在进行中的部署计划 %s（%s）", p.ID, p.State)})
			return
		}
	}
	if err := appStore.Delete(name); err != nil {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "application not found"})
		return
	}
	writeV0470JSON(w, http.StatusOK, map[string]any{"deleted": name})
}

func apiAddAppVersion(w http.ResponseWriter, r *http.Request) {
	if appStore == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用目录未启用"})
		return
	}
	var body appVersionReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeV0470JSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	a, err := appStore.AddVersion(r.PathValue("name"), appstore.Version{
		Version: body.Version, Image: body.Image, ScriptRef: body.ScriptRef, Metadata: body.Metadata,
	})
	if err == appstore.ErrNotFound {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "application not found"})
		return
	}
	if err == appstore.ErrExists {
		writeV0470JSON(w, http.StatusConflict, map[string]string{"error": "version already exists"})
		return
	}
	if err != nil {
		writeV0470JSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeV0470JSON(w, http.StatusCreated, a)
}

func apiListAppVersions(w http.ResponseWriter, r *http.Request) {
	if appStore == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用目录未启用"})
		return
	}
	a, ok := appStore.Get(r.PathValue("name"))
	if !ok {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "application not found"})
		return
	}
	writeV0470JSON(w, http.StatusOK, map[string]any{"versions": a.Versions, "count": len(a.Versions)})
}

// ---- US-3 应用包 ----

func apiPutAppPackage(w http.ResponseWriter, r *http.Request) {
	if appStore == nil || appPkgDir == "" {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用包服务未启用（需配置 APPSTORE/APPKG）"})
		return
	}
	name := r.PathValue("name")
	version := r.PathValue("version")
	if _, ok := appStore.GetVersion(name, version); !ok {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "application/version not found"})
		return
	}
	// 大小硬上限（Content-Length 预检 + 读时截断双保险）
	if r.ContentLength > appPkgMaxBytes {
		writeV0470JSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "package exceeds 32MB"})
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, appPkgMaxBytes))
	if err != nil {
		writeV0470JSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "package exceeds 32MB or read error"})
		return
	}
	if len(data) == 0 {
		writeV0470JSON(w, http.StatusBadRequest, map[string]string{"error": "empty package"})
		return
	}
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	path := appPkgPath(name, version)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		writeV0470JSON(w, http.StatusInternalServerError, map[string]string{"error": "pkg dir create failed"})
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		writeV0470JSON(w, http.StatusInternalServerError, map[string]string{"error": "pkg write failed"})
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		writeV0470JSON(w, http.StatusInternalServerError, map[string]string{"error": "pkg rename failed"})
		return
	}
	_ = appStore.MarkPkg(name, version, true)
	log.Infof("[apppkg] 包已上传：app=%s version=%s bytes=%d sha256=%.12s", name, version, len(data), checksum)
	writeV0470JSON(w, http.StatusOK, map[string]any{
		"app": name, "version": version, "bytes": len(data), "sha256": checksum,
	})
}

func apiGetAppPackage(w http.ResponseWriter, r *http.Request) {
	if appStore == nil || appPkgDir == "" {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用包服务未启用"})
		return
	}
	name := r.PathValue("name")
	version := r.PathValue("version")
	if _, ok := appStore.GetVersion(name, version); !ok {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "application/version not found"})
		return
	}
	data, err := os.ReadFile(appPkgPath(name, version))
	if err != nil {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "package not uploaded"})
		return
	}
	sum := sha256.Sum256(data)
	w.Header().Set("X-Checksum-Sha256", hex.EncodeToString(sum[:]))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-%s.tar", name, version))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---- US-2 灰度部署 ----

func apiCreateAppDeployment(w http.ResponseWriter, r *http.Request) {
	if appDeployCtl == nil || appStore == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用部署未启用"})
		return
	}
	var body struct {
		App             string   `json:"app"`
		Version         string   `json:"version"`
		Nodes           []string `json:"nodes"`
		BatchSize       int      `json:"batchSize"`
		PauseBetweenSec int      `json:"pauseBetweenSec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeV0470JSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	a, ok := appStore.Get(body.App)
	if !ok {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "application not found"})
		return
	}
	ver, ok := appStore.GetVersion(body.App, body.Version)
	if !ok {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "version not found"})
		return
	}
	plan := &appdeploy.Plan{
		App: a.Name, Version: ver.Version, Kind: a.Kind,
		Image: ver.Image, ScriptRef: ver.ScriptRef,
		BatchSize: body.BatchSize, PauseBetweenSec: body.PauseBetweenSec,
	}
	created, err := appDeployCtl.Create(plan, body.Nodes)
	if err != nil {
		writeV0470JSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Infof("[appdeploy] 部署计划创建：%s app=%s@%s nodes=%d batch=%d", created.ID, created.App, created.Version, len(created.Nodes), created.BatchSize)
	writeV0470JSON(w, http.StatusCreated, created)
}

func apiListAppDeployments(w http.ResponseWriter, r *http.Request) {
	if appDeployCtl == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用部署未启用"})
		return
	}
	all := appDeployCtl.List()
	limit, offset := v0470Page(r, len(all))
	items := all[offset : offset+limit]
	w.Header().Set("X-Total-Count", strconv.Itoa(len(all)))
	writeV0470JSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

func apiGetAppDeployment(w http.ResponseWriter, r *http.Request) {
	if appDeployCtl == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用部署未启用"})
		return
	}
	p, ok := appDeployCtl.Get(r.PathValue("planID"))
	if !ok {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "deployment not found"})
		return
	}
	writeV0470JSON(w, http.StatusOK, p)
}

// writeV0470Ctl 执行状态控制动作（统一 404/409 映射）。
func writeV0470Ctl(w http.ResponseWriter, id string, fn func(string) error) {
	if err := fn(id); err == appdeploy.ErrNotFound {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "deployment not found"})
		return
	} else if err == appdeploy.ErrConflict {
		writeV0470JSON(w, http.StatusConflict, map[string]string{"error": "state conflict"})
		return
	} else if err != nil {
		writeV0470JSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	p, _ := appDeployCtl.Get(id)
	writeV0470JSON(w, http.StatusOK, p)
}

func apiPauseAppDeployment(w http.ResponseWriter, r *http.Request) {
	if appDeployCtl == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用部署未启用"})
		return
	}
	writeV0470Ctl(w, r.PathValue("planID"), appDeployCtl.Pause)
}

func apiResumeAppDeployment(w http.ResponseWriter, r *http.Request) {
	if appDeployCtl == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用部署未启用"})
		return
	}
	writeV0470Ctl(w, r.PathValue("planID"), appDeployCtl.Resume)
}

func apiCancelAppDeployment(w http.ResponseWriter, r *http.Request) {
	if appDeployCtl == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用部署未启用"})
		return
	}
	writeV0470Ctl(w, r.PathValue("planID"), appDeployCtl.Cancel)
}

func apiRollbackAppDeployment(w http.ResponseWriter, r *http.Request) {
	if appDeployCtl == nil || appStore == nil {
		writeV0470JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用部署未启用"})
		return
	}
	planID := r.PathValue("planID")
	orig, ok := appDeployCtl.Get(planID)
	if !ok {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "deployment not found"})
		return
	}
	// 目标版本：body.version 显式指定，缺省 = 该应用目录中早于当前版本的最大版本
	var body struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	target := body.Version
	if target == "" {
		// 缺省目标 = 严格早于当前版本的最新版（spec §3 边界 ⑤ 只恢复上一版本；
		// v0.47 复核 P2-1：原「排除当前版后取最大」可向前回滚到更新版本）
		app, _ := appStore.Get(orig.App)
		var candidates []string
		for _, v := range app.Versions {
			if v.Version < orig.Version {
				candidates = append(candidates, v.Version)
			}
		}
		if len(candidates) == 0 {
			writeV0470JSON(w, http.StatusConflict, map[string]string{"error": "无可回滚版本（目录中无早于当前版本者）"})
			return
		}
		target = candidates[len(candidates)-1] // 版本目录升序 → 取最大即最近上一版
	}
	ver, ok := appStore.GetVersion(orig.App, target)
	if !ok {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "target version not found"})
		return
	}
	rb, err := appDeployCtl.Rollback(planID, ver.Version, ver.Image)
	if err == appdeploy.ErrNotFound {
		writeV0470JSON(w, http.StatusNotFound, map[string]string{"error": "deployment not found"})
		return
	}
	if err != nil {
		writeV0470JSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	log.Infof("[appdeploy] 回滚计划创建：%s → %s@%s", rb.ID, orig.App, ver.Version)
	writeV0470JSON(w, http.StatusCreated, rb)
}

func writeV0470JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// appDeployScanInterval 是灰度控制器扫描周期（默认 2s；下限 500ms）。
func appDeployScanInterval() time.Duration {
	if v := os.Getenv("EDGEFLOW_CLOUDCORE_APPDEPLOY_SCAN_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return time.Duration(n) * time.Second
		}
	}
	return 2 * time.Second
}

// cloudAppDeployer 是 appdeploy.Deployer 的生产实现（PodSync/ConfigSync 组合）：
//   - container：PodSync add/update（pod 规格 name=edgeflow-app-<app>，
//     namespace=edgeflow，image=plan.Image，replicas=1）；
//   - script：ConfigSync add（data: scriptRef/sha256/生命周期声明——边缘
//     scriptrunner 消费；脚本包本体经应用包下载面拉取）。
//
// 交付错误原样返回（perNode.Reason 记录）。
type cloudAppDeployer struct {
	hub interface {
		ReliableSendContext(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error
	}
}

func (d *cloudAppDeployer) Deliver(ctx context.Context, plan *appdeploy.Plan, nodeID string) error {
	objName := "edgeflow-app-" + appstore.SanitizeAppName(plan.App)
	switch plan.Kind {
	case appstore.KindContainer:
		payload := map[string]any{
			"operation": "update", // upsert 语义：边缘 SavePod 幂等
			"pod": map[string]any{
				"name": objName, "namespace": "edgeflow",
				"image": plan.Image, "replicas": 1,
			},
		}
		msg, err := protocol.NewMessage(protocol.TypePodSync, "cloud", nodeID, payload)
		if err != nil {
			return err
		}
		return d.hub.ReliableSendContext(ctx, nodeID, msg, cloudhub.ReliableOptions{})
	case appstore.KindScript:
		// spec 0020 US-4：脚本包经 ConfigSync 内联（base64）+ sha256 硬校验。
		// 内联上限 8MB（base64 后 ~10.7MB）——超出拒绝交付并留痕（大包走
		// 离线预下发面，spec §3 边界 ⑤）；包未上传同样拒绝。
		pkgPath := appPkgPath(plan.App, plan.Version)
		blob, err := os.ReadFile(pkgPath)
		if err != nil {
			return fmt.Errorf("脚本包未上传（先 PUT .../package）: %w", err)
		}
		if len(blob) > 8<<20 {
			return fmt.Errorf("脚本包 %d 字节超内联上限 8MB", len(blob))
		}
		sum := sha256.Sum256(blob)
		data := map[string]string{
			"app":       plan.App,
			"version":   plan.Version,
			"scriptRef": plan.ScriptRef,
			"sha256":    hex.EncodeToString(sum[:]),
			"scriptTar": base64.StdEncoding.EncodeToString(blob),
		}
		payload := map[string]any{
			"operation": "add",
			"config": map[string]any{
				"name": objName, "namespace": "edgeflow", "kind": "ConfigMap", "data": data,
			},
		}
		msg, err := protocol.NewMessage(protocol.TypeConfigSync, "cloud", nodeID, payload)
		if err != nil {
			return err
		}
		return d.hub.ReliableSendContext(ctx, nodeID, msg, cloudhub.ReliableOptions{})
	default:
		return fmt.Errorf("未知应用形态: %s", plan.Kind)
	}
}

func (d *cloudAppDeployer) Remove(ctx context.Context, plan *appdeploy.Plan, nodeID string) error {
	objName := "edgeflow-app-" + appstore.SanitizeAppName(plan.App)
	if plan.Kind == appstore.KindContainer {
		payload := map[string]any{
			"operation": "delete",
			"pod":       map[string]any{"name": objName, "namespace": "edgeflow"},
		}
		msg, err := protocol.NewMessage(protocol.TypePodSync, "cloud", nodeID, payload)
		if err != nil {
			return err
		}
		return d.hub.ReliableSendContext(ctx, nodeID, msg, cloudhub.ReliableOptions{})
	}
	// script：ConfigSync delete（边缘 scriptrunner 卸载）
	payload := map[string]any{
		"operation": "delete",
		"config":    map[string]any{"name": objName, "namespace": "edgeflow", "kind": "ConfigMap"},
	}
	msg, err := protocol.NewMessage(protocol.TypeConfigSync, "cloud", nodeID, payload)
	if err != nil {
		return err
	}
	return d.hub.ReliableSendContext(ctx, nodeID, msg, cloudhub.ReliableOptions{})
}
