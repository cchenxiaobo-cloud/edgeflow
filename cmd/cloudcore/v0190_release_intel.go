package main

// v0.19.0 发布面智能运维第二批：发布审计快照 + 全局发布查询。
//
// 设计要点（与 v0.17.0/v0.18.0 同源约束）：
//   - 存储零变化：两个端点都是只读读路径——snapshot 走既有
//     GetRelease/ListNodeResults（events 内嵌 release 头），全局查询走
//     ListReleases("")（Memory 全 map 扫描/Etcd 同一前缀读，接口已支持
//     model=""=全部），不新增任何存储接口方法；
//   - 端点只增不改：38→40 两条 GET，全部纯查询无副作用；
//   - 缺省行为逐字节不变：不改任何既有处理器。

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"edgeflow/cloud/pkg/modelrelease"

	"edgeflow/cloud/pkg/modelrepo"
)

// maxFailureBudget 是 PATCH failureBudget 的上限护栏（与创建校验同一量级：
// 预算语义上只需区分"0=关闸/小数字=尽早刹车"，万级预算失去刹车意义且
// 防御性拦截明显写错位的调用）。
const maxFailureBudget = 10000

// releaseSnapshot 是 GET .../releases/{releaseID}/snapshot 的响应体
// （v0.19.0 审计快照）：一次拉全发布全景——头（含 events 时间线）+ 逐节点
// 结果 + 现算 summary 六计数。**非承诺语义**：generatedAt 之后的写入不在
// 本快照内；节点结果分页上限与列表一致（snapshot 是审计锚点，超大发布
// 以 releases/{id}/nodes 列表为准——口径登记 KNOWN-ISSUES §19）。
type releaseSnapshot struct {
	Kind        string                        `json:"kind"` // "ReleaseSnapshot"
	APIVersion  string                        `json:"apiVersion"`
	GeneratedAt int64                         `json:"generatedAt"`
	Release     *modelrepo.ModelRelease       `json:"release"`
	Summary     modelrelease.NodeSummary      `json:"summary,omitempty"`
	Nodes       []modelrepo.NodeReleaseResult `json:"nodes"` // 恒非 nil（空发布 → []）
}

// handleReleaseSnapshot 处理 GET /api/v1/models/{modelName}/releases/{releaseID}/snapshot。
// 校验链：模型存在（404 先行，与 v0.17.0 C-4 同链序纪律）→ release 头存在
// （404；同时钉住 head.Model == modelName，跨模型引用一律 404 防目录穿越式
// 枚举）→ 节点结果装配失败 → 502 语义沿用 modelError 族（存储错误原样映射）。
func (a *modelAPI) handleReleaseSnapshot(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	if _, err := a.store.GetModel(r.Context(), modelName); err != nil {
		modelError(w, err)
		return
	}
	release, err := a.store.GetRelease(r.Context(), r.PathValue("releaseID"))
	if err != nil {
		modelError(w, err)
		return
	}
	if release.Model != modelName {
		modelError(w, modelrepo.ErrReleaseNotFound)
		return
	}
	nodes, err := a.store.ListNodeResults(r.Context(), release.ID)
	if err != nil {
		modelError(w, err)
		return
	}
	if nodes == nil {
		nodes = []modelrepo.NodeReleaseResult{}
	}
	writeJSON(w, http.StatusOK, releaseSnapshot{
		Kind:        "ReleaseSnapshot",
		APIVersion:  "v1",
		GeneratedAt: time.Now().UnixMilli(),
		Release:     release,
		Summary:     modelrelease.SummarizeNodes(nodes),
		Nodes:       nodes,
	})
}

// releaseListAll 是全局发布查询的响应形态（v0.23.0，CLD-08 契约口径
// 统一）：对齐模型域 K8s List 风格（kind/apiVersion/items）——v0.19.0
// 为裸 items map，与 GET /api/v1/models/{m}/releases（releaseList）及
// GET /api/v1/deployments 双口径。items 元素沿用 releaseResponse 形态
// （字段平铺 + summary 恒输出，CLD-07 同口径）。增量字段：顶层新增
// kind/apiVersion 两键，items 键名与元素既有字段不变，消费方兼容。
type releaseListAll struct {
	Kind       string            `json:"kind"`
	APIVersion string            `json:"apiVersion"`
	Items      []releaseResponse `json:"items"`
}

// handleListAllReleases 处理 GET /api/v1/releases（v0.19.0 全局发布查询，
// 与 v0.18.0 /api/v1/deployments 对偶）：跨模型聚合全部 release 头；
// status 逗号多值过滤复用 v0.17.0 parseStatusFilter（七态枚举）；
// limit 缺省 100 上限 500、offset≥0；X-Total-Count 报过滤后总数；
// CreatedAt 降序稳定 tie-break by ID（Memory 层升序返回后此处反转排序，
// etcd 前缀序同构）。
func (a *modelAPI) handleListAllReleases(w http.ResponseWriter, r *http.Request) {
	allow, err := parseStatusFilter(r.URL.Query().Get("status"))
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 1 || n > maxReleaseListLimit {
			badRequest(w, "limit must be in [1,%d]", maxReleaseListLimit)
			return
		}
		limit = n
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 {
			badRequest(w, "offset must be >= 0")
			return
		}
		offset = n
	}
	items, err := a.store.ListReleases(r.Context(), "") // "" = 全部模型
	if err != nil {
		modelError(w, err)
		return
	}
	items = filterReleasesByStatus(items, allow)
	sort.Slice(items, func(i, j int) bool { // 降序稳定 tie-break by ID
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		return items[i].ID > items[j].ID
	})
	total := len(items)
	if offset > total {
		offset = total
	}
	items = items[offset:]
	if len(items) > limit {
		items = items[:limit]
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	// v0.23.0（CLD-08 + CLD-07）：裸 items map → K8s List 风格包装
	// （kind/apiVersion 增量字段）；items 逐条现算 summary，与模型域
	// 发布列表/详情同口径。
	out := make([]releaseResponse, 0, len(items))
	for i := range items {
		out = append(out, a.summaryOfRelease(r.Context(), &items[i]))
	}
	writeJSON(w, http.StatusOK, releaseListAll{Kind: "ModelReleaseList", APIVersion: "v1", Items: out})
}

const maxReleaseListLimit = 500
