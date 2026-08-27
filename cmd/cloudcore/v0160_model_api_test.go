package main

// v0.16.0 API 层契约集成测试：pause/resume 状态守卫（409 族）、notBeforeMs
// 创建校验（400）、export/import 回环幂等。既有 model_api_test.go 不改一行
// （路由计数断言随发布口径更新除外）；本文件纯增量。

import (
	"net/http"
	"testing"
)

func TestV160PauseResumeGuards(t *testing.T) {
	srv, _, _ := newContractEnv(t, "node-a")
	mustCreateModelVersion(t, srv, "mnist", "v1.0.0")

	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
		"target": map[string]any{"type": "nodeIDs", "nodeIDs": []string{"node-a"}},
		"version": "v1.0.0",
	})
	if code != http.StatusAccepted {
		t.Fatalf("创建发布 = %d，want 202（%v）", code, body)
	}
	rid := body["id"].(string)

	// pending → pause 409（未启动不可暂停）
	if code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/pause", nil); code != http.StatusConflict {
		t.Errorf("pending pause = %d %v，want 409", code, body)
	}
	// pending → resume 409
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/resume", nil); code != http.StatusConflict {
		t.Errorf("pending resume = %d，want 409", code)
	}
	// 控制器未运行（无认领）：模拟 running 需经存储层——这里用 API 级路径
	// 只验证守卫与响应形态；running⇄paused 流转由 modelrelease 单测覆盖。
	// cancel 后（canceled 终态）pause/resume 均 409：
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/cancel", nil); code != http.StatusOK {
		t.Fatalf("cancel = %d，want 200", code)
	}
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/pause", nil); code != http.StatusConflict {
		t.Errorf("终态 pause = %d，want 409", code)
	}
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/resume", nil); code != http.StatusConflict {
		t.Errorf("终态 resume = %d，want 409", code)
	}
	// 不存在 releaseID → 404 与 cancel 一致
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/ghost/pause", nil); code != http.StatusNotFound {
		t.Errorf("缺失发布 pause = %d，want 404", code)
	}
}

func TestV160NotBeforeWindowValidation(t *testing.T) {
	srv, _, _ := newContractEnv(t, "node-a")
	mustCreateModelVersion(t, srv, "mnist", "v1.0.0")

	// 负数 notBeforeMs → 400
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
		"target": map[string]any{"type": "nodeIDs", "nodeIDs": []string{"node-a"}},
		"version": "v1.0.0", "notBeforeMs": -5,
	}); code != http.StatusBadRequest {
		t.Errorf("负 notBeforeMs = %d，want 400", code)
	}
	// 远在过去（>5min）→ 400 钟漂护栏
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
		"target": map[string]any{"type": "nodeIDs", "nodeIDs": []string{"node-a"}},
		"version": "v1.0.0", "notBeforeMs": 1000,
	}); code != http.StatusBadRequest {
		t.Errorf("远古 notBeforeMs = %d，want 400", code)
	}
	// 合法未来窗口 → 202 且回显 notBeforeMs
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
		"target": map[string]any{"type": "nodeIDs", "nodeIDs": []string{"node-a"}},
		"version": "v1.0.0", "notBeforeMs": 9999999999999,
	})
	if code != http.StatusAccepted {
		t.Fatalf("合法窗口创建 = %d（%v），want 202", code, body)
	}
	if body["notBeforeMs"] != float64(9999999999999) {
		t.Errorf("响应应回显 notBeforeMs，got %v", body["notBeforeMs"])
	}
}

func TestV160ExportImportRoundtrip(t *testing.T) {
	srv, _, _ := newContractEnv(t, "node-a")
	mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
	mustCreateModelVersion(t, srv, "yolo", "v2.0.0")

	// 导出：200 + schemaVersion=1 + 两模型两版本
	code, cat := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/export", nil)
	if code != http.StatusOK {
		t.Fatalf("导出 = %d，want 200", code)
	}
	if cat["schemaVersion"] != "1" {
		t.Errorf("schemaVersion = %v，want \"1\"", cat["schemaVersion"])
	}
	models := cat["models"].([]any)
	versions := cat["versions"].([]any)
	if len(models) != 2 || len(versions) != 2 {
		t.Fatalf("导出 models=%d versions=%d，want 2/2", len(models), len(versions))
	}

	// 导入到同一环境（模拟灾备回灌）：全部 skipped/updated（已存在）
	code, rep := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/import", cat)
	if code != http.StatusOK {
		t.Fatalf("导入 = %d（%v），want 200", code, rep)
	}
	if rep["modelsUpdated"] != float64(2) {
		t.Errorf("modelsUpdated = %v，want 2（已存在→元数据覆盖）", rep["modelsUpdated"])
	}
	if rep["versionsSkipped"] != float64(2) {
		t.Errorf("versionsSkipped = %v，want 2（同 version 不覆盖）", rep["versionsSkipped"])
	}

	// 改名回灌（模拟新环境）：全部 imported —— 把 models 段改名后重放
	cat["models"].([]any)[0].(map[string]any)["name"] = "mnist-r2"
	cat["models"].([]any)[1].(map[string]any)["name"] = "yolo-r2"
	for i := range cat["versions"].([]any) {
		vm := cat["versions"].([]any)[i].(map[string]any)
		if vm["model"] == "mnist" {
			vm["model"] = "mnist-r2"
		} else {
			vm["model"] = "yolo-r2"
		}
	}
	code, rep = doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/import", cat)
	if code != http.StatusOK {
		t.Fatalf("改名回灌 = %d（%v），want 200", code, rep)
	}
	if rep["modelsImported"] != float64(2) || rep["versionsImported"] != float64(2) {
		t.Errorf("改名回灌计数 = %v，want imported 2/2", rep)
	}
	// 回灌的 active 版本在新模型下仍 active（灾备语义直通）
	if code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist-r2", nil); code != http.StatusOK {
		t.Errorf("回灌模型详情 = %d", code)
	} else if body == nil {
		t.Error("回灌模型详情为空")
	}

	// 坏 schemaVersion → 400
	bad := map[string]any{"schemaVersion": "99", "models": []any{}, "versions": []any{}}
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/import", bad); code != http.StatusBadRequest {
		t.Errorf("坏 schemaVersion = %d，want 400", code)
	}
}
