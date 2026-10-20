// 区域层与标签治理 API（v0.50.0，spec 0023，G03+G04）。
//
// 七端点（107→114）：
//
//	POST   /api/v1/regions            创建区域（write：operator+）
//	GET    /api/v1/regions            区域树（read：viewer+）
//	PUT    /api/v1/regions/{id}       更新（改名/描述/移动防环，write）
//	DELETE /api/v1/regions/{id}       删除（空区域约束，write）
//	POST   /api/v1/regions/{id}/nodes 节点批量归属（幂等换区，write）
//	POST   /api/v1/tags               打标/删标（三类锚点，write）
//	GET    /api/v1/governance/nodes   区域+标签组合查询（read）
//
// RBAC 由全局 middleware 统一管控（POST/PUT=write operator+ / DELETE=delete
// admin——对齐 v0.46 既有矩阵 / GET=read viewer+）；越权在 middleware 层 403，
// 本文件不重复判定。
// 组合查询为管理面 join（regionstore 归属 × tagstore 节点标签），
// 不动既有 /api/v1/nodes 契约（spec 裁决 7 只增不改）。
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"edgeflow/cloud/pkg/regionstore"
	"edgeflow/cloud/pkg/tagstore"
)

// mapRegionErr 把 regionstore 语义错误映射为 HTTP 状态码。
func mapRegionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, regionstore.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error(), nil)
	case errors.Is(err, regionstore.ErrCycle),
		errors.Is(err, regionstore.ErrHasChildren),
		errors.Is(err, regionstore.ErrHasNodes):
		writeErr(w, http.StatusConflict, err.Error(), nil)
	default:
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
	}
}

// registerGovernanceAPIs 注册治理 7 端点。
func registerGovernanceAPIs(mux *http.ServeMux, regions *regionstore.Store, tags *tagstore.Store) {
	// ─── US-1 区域树 ───
	mux.HandleFunc("POST /api/v1/regions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name        string `json:"name"`
			ParentID    string `json:"parentId"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), nil)
			return
		}
		reg, err := regions.Create(body.Name, body.ParentID, body.Description)
		if err != nil {
			mapRegionErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, reg)
	})

	mux.HandleFunc("GET /api/v1/regions", func(w http.ResponseWriter, r *http.Request) {
		tree := regions.Tree()
		writeJSON(w, http.StatusOK, map[string]any{"regions": tree, "total": len(tree)})
	})

	mux.HandleFunc("PUT /api/v1/regions/{id}", func(w http.ResponseWriter, r *http.Request) {
		// 指针字段：JSON 提供即改（缺失不改）；parentId 显式 "" = 移到根。
		var body struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			ParentID    *string `json:"parentId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), nil)
			return
		}
		reg, err := regions.Update(r.PathValue("id"), regionstore.UpdateOpts{
			Name: body.Name, Description: body.Description, ParentID: body.ParentID,
		})
		if err != nil {
			mapRegionErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, reg)
	})

	mux.HandleFunc("DELETE /api/v1/regions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := regions.Delete(r.PathValue("id")); err != nil {
			mapRegionErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	})

	// ─── US-2 节点归属 ───
	mux.HandleFunc("POST /api/v1/regions/{id}/nodes", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NodeIDs []string `json:"nodeIds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), nil)
			return
		}
		assigned, skipped, err := regions.AssignNodes(r.PathValue("id"), body.NodeIDs)
		if err != nil {
			mapRegionErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"assigned": assigned, "skipped": skipped})
	})

	// ─── US-3 打标 ───
	mux.HandleFunc("POST /api/v1/tags", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AnchorType string             `json:"anchorType"`
			AnchorID   string             `json:"anchorId"`
			Tags       map[string]*string `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), nil)
			return
		}
		changed, err := tags.SetTags(body.AnchorType, body.AnchorID, body.Tags)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
	})

	// ─── US-4 组合查询（region × tag join，只增不改）───
	mux.HandleFunc("GET /api/v1/governance/nodes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		regionID := q.Get("region")
		tagFilter := q.Get("tag") // 形态 key:value 或 key（存在性过滤）
		tagKey := q.Get("tagKey")

		candidates := map[string]struct{}{}
		if regionID != "" {
			if _, ok := regions.RegionByID(regionID); !ok {
				writeErr(w, http.StatusNotFound, "区域不存在", nil)
				return
			}
			for _, nid := range regions.NodesInRegion(regionID) {
				candidates[nid] = struct{}{}
			}
		}
		if tagFilter != "" || tagKey != "" {
			k, v := tagKey, ""
			if tagFilter != "" {
				k, v = tagFilter, ""
				if i := strings.Index(tagFilter, ":"); i >= 0 {
					k, v = tagFilter[:i], tagFilter[i+1:]
				}
			}
			// 复核 P3-④：空键静默退化为无过滤是语义陷阱，显式拒绝。
			if k == "" {
				writeErr(w, http.StatusBadRequest, "tag 过滤键不得为空（形态 key:value 或 key）", nil)
				return
			}
			filtered := map[string]struct{}{}
			for _, nid := range tags.AnchorsWithTag(tagstore.AnchorNode, k, v) {
				filtered[nid] = struct{}{}
			}
			if regionID != "" {
				// 交集（AND 语义，spec 裁决 7）
				for nid := range candidates {
					if _, ok := filtered[nid]; !ok {
						delete(candidates, nid)
					}
				}
			} else {
				candidates = filtered
			}
		}
		if regionID == "" && tagFilter == "" && tagKey == "" {
			// 无过滤：全量资产视图 = 归属节点 ∪ 有 node 标签的锚点
			for _, nid := range tags.AnchorsWithTag(tagstore.AnchorNode, "", "") {
				candidates[nid] = struct{}{}
			}
			for _, nid := range regions.AllAssignedNodes() {
				candidates[nid] = struct{}{}
			}
		}

		ids := make([]string, 0, len(candidates))
		for nid := range candidates {
			ids = append(ids, nid)
		}
		sort.Strings(ids)
		type nodeView struct {
			NodeID string              `json:"nodeId"`
			Region *regionstore.Region `json:"region"`
			Tags   map[string]string   `json:"tags"`
		}
		out := make([]nodeView, 0, len(ids))
		for _, nid := range ids {
			v := nodeView{NodeID: nid, Tags: tags.TagsOf(tagstore.AnchorNode, nid)}
			if reg, ok := regions.RegionOfNode(nid); ok {
				rg := reg
				v.Region = &rg
			}
			out = append(out, v)
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": out, "total": len(out)})
	})
}
