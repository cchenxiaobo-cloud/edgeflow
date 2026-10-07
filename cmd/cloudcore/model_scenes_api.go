package main

// v0.45.0（spec 0018 US-2，G23）：模型-业务场景关联查询端点。
//
// GET /api/v1/models/{modelName}/scenes
//   → 200 {"modelName":"...","modality":"...","bindings":[{"kind":"camera","name":"cam-01"}],"count":n}
//   → 404 模型不存在
//
// scene.bindings 是 Model.Metadata 约定键（JSON 数组字符串；spec 0018 US-2）。
// 本端点是其结构化视图（非独立资源，无 CRUD）；未配置/解析失败 → 空列表
// （解析失败记 Warn——bindings 键被外部改坏时不阻塞读取面）。

import (
	"encoding/json"
	"net/http"

	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/pkg/log"
)

// MetaKeySceneBindings 是场景绑定约定键（与 modelrepo 侧注释同步）。
const MetaKeySceneBindings = "scene.bindings"

// sceneBinding 是一条绑定（kind: camera|device；name: 对象名）。
type sceneBinding struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// scenesResponse 是 GET .../scenes 响应形态。
type scenesResponse struct {
	ModelName string         `json:"modelName"`
	Modality  string         `json:"modality,omitempty"`
	Bindings  []sceneBinding `json:"bindings"`
	Count     int            `json:"count"`
}

// listModelScenes 处理 GET /api/v1/models/{modelName}/scenes。
func (a *modelAPI) listModelScenes(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	model, err := a.store.GetModel(r.Context(), modelName)
	if err != nil {
		modelError(w, err)
		return
	}
	resp := scenesResponse{
		ModelName: model.Name,
		Modality:  model.Metadata[modelrepo.MetaKeyModality],
		Bindings:  []sceneBinding{},
	}
	if raw := model.Metadata[MetaKeySceneBindings]; raw != "" {
		var bindings []sceneBinding
		if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
			log.Warnf("[model-scenes] bindings 键解析失败（model=%s，返回空列表）: %v", modelName, err)
		} else {
			resp.Bindings = bindings
		}
	}
	resp.Count = len(resp.Bindings)
	writeJSON(w, http.StatusOK, resp)
}
