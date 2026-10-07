package e2e

// v0.45.0 e2e（spec 0018）：模型面扩展 + 困难样本回传 + 加速卡上报（G23/G25/G24）。
//
// 场景复用 v0430 视频链形态（合成帧源 + mediaSinkHolder）+ v0400 告警链：
//   - US-5：EDGEFLOW_EDGECORE_ACCELS env → Register 上报 → 云端节点快照含 accels；
//   - US-2：创建时序模型（modality=time-series + scene.bindings）→ scenes 查询；
//   - US-3/US-4：HARDSAMPLE=on + 告警规则触发 → 采集器从 mediaSinkHolder 捕获
//     最新快照帧 → EnqueueHardSample（kind=hard-sample）→ 补传队列 → 云端
//     mediastore（objects/hardsample/ 前缀，不挂 videostream 索引）→
//     GET /api/v1/hardsamples 按 alarmId 过滤可查 → 内容回读 sha256 一致。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// v0450GetJSON GET JSON 并断言状态码。
func v0450GetJSON(t *testing.T, url string, out any, wantCode int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != wantCode {
		t.Fatalf("GET %s = %d, want %d", url, resp.StatusCode, wantCode)
	}
	if out == nil {
		return
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// TestV0450ModelSampleInferenceE2E 主用例。
func TestV0450ModelSampleInferenceE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)
	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	mediaDir := filepath.Join(t.TempDir(), "media")
	t.Setenv("EDGEFLOW_CLOUDCORE_MEDIA_DIR", mediaDir) // 云端媒资目录（困难样本对象落此）
	httpPort, hubPort := reservePort(t), reservePort(t)
	cloud := v0400StartCloudcoreOnPorts(t, root, httpPort, hubPort)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID := "e2e-v045-1"

	// 视频配置（复用 v0430 形态）：合成源 + 本地推理 stub + media 采集。
	// 困难样本从 mediaSinkHolder 环形缓冲取最新帧——合成源 5fps 保证告警
	// 触发时环形非空。推理 stub 检出恒真（驱动媒资采集触发；困难样本链
	// 只依赖帧，不依赖推理结果语义）。
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"detections":[{"label":"motion","score":0.9,"bbox":[1,1,4,4]}]}`))
	}))
	defer stub.Close()
	stubURL := stub.URL
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "video.json")
	cfgJSON := fmt.Sprintf(`{"deviceName":"cam-v045","source":{"type":"synthetic","synthetic":{"fps":5,"width":320,"height":240}},"inference":{"url":%q,"timeoutMs":2000},"media":{"enabled":true,"segmentFrames":6,"minIntervalMs":800}}`, stubURL)
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("写视频配置: %v", err)
	}
	spoolDir := filepath.Join(dir, "spool")

	dbPath := filepath.Join(t.TempDir(), "edgecore.db")
	cloudAddr := "ws://127.0.0.1:" + strconv.Itoa(hubPort)
	env := append(edgeEnv(nodeID, cloudAddr, dbPath),
		"EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=1s",
		"EDGEFLOW_EDGECORE_RECONCILE_INTERVAL=1s",
		"EDGEFLOW_EDGECORE_UPLINK=on",
		"EDGEFLOW_EDGECORE_UPLINK_REPORT_SEC=1",
		"EDGEFLOW_EDGECORE_UPLINK_BATCH=16",
		"EDGEFLOW_EDGECORE_UPLINK_RATE=50",
		"EDGEFLOW_EDGECORE_ALARM_REANNOUNCE_SEC=8",
		"EDGEFLOW_VIDEO_MAPPER_CONFIG="+cfgPath,
		"EDGEFLOW_MEDIA_SPOOL_DIR="+spoolDir,
		"EDGEFLOW_EDGECORE_HARDSAMPLE=on",                          // US-3 opt-in
		"EDGEFLOW_EDGECORE_ACCELS=gpu:cuda-12.4,npu:rockchip-9996", // US-5
	)
	_ = startProcess(t, "edgecore-"+nodeID, filepath.Join(binDir, "edgecore"), nil, env)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册（视频媒资 + 困难样本 opt-in + 加速卡清单）", nodeID)

	// 1. US-5：云端节点快照含 accels（Register 透传链铁证）。
	type nodeView struct {
		NodeID string   `json:"nodeID"`
		Accels []string `json:"accels"`
	}
	var nv nodeView
	v0450GetJSON(t, base+"/api/v1/nodes/"+nodeID, &nv, http.StatusOK)
	if len(nv.Accels) != 2 || nv.Accels[0] != "gpu:cuda-12.4" || nv.Accels[1] != "npu:rockchip-9996" {
		t.Fatalf("节点 accels = %v, want [gpu:cuda-12.4 npu:rockchip-9996]", nv.Accels)
	}
	t.Logf("US-5 加速卡上报链路 OK：%v", nv.Accels)

	// 2. US-2：创建时序模型（modality + scene.bindings）→ scenes 结构化查询。
	createBody := map[string]any{
		"name":        "ts-anomaly-v1",
		"description": "时序异常检测（e2e）",
		"type":        "anomaly-detection",
		"metadata": map[string]string{
			"modality":       "time-series",
			"input.features": "temperature,vibration",
			"input.window":   "64",
			"input.rate":     "10",
			"scene.bindings": `[{"kind":"device","name":"sensor-01"}]`,
		},
	}
	v0370Do(t, "POST", base+"/api/v1/models", createBody, http.StatusOK)
	type scenesView struct {
		ModelName string `json:"modelName"`
		Modality  string `json:"modality"`
		Bindings  []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"bindings"`
		Count int `json:"count"`
	}
	var sc scenesView
	v0450GetJSON(t, base+"/api/v1/models/ts-anomaly-v1/scenes", &sc, http.StatusOK)
	if sc.Count != 1 || sc.Bindings[0].Kind != "device" || sc.Bindings[0].Name != "sensor-01" {
		t.Fatalf("scenes = %+v", sc)
	}
	if sc.Modality != "time-series" {
		t.Fatalf("modality = %q", sc.Modality)
	}
	t.Logf("US-2 模型场景关联 OK：%s（%s）绑定 %d 条", sc.ModelName, sc.Modality, sc.Count)

	// 3. US-4：检索面空态语义 + 未知 404。
	var emptyList struct {
		Items []map[string]any `json:"items"`
		Count int              `json:"count"`
	}
	v0450GetJSON(t, base+"/api/v1/hardsamples", &emptyList, http.StatusOK)
	if emptyList.Count != 0 {
		t.Fatalf("初始困难样本应为空，got %d", emptyList.Count)
	}
	v0450GetJSON(t, base+"/api/v1/hardsamples/none/content", nil, http.StatusNotFound)
	t.Logf("US-4 检索面空态语义 OK")

	// 4. US-3 全链铁证：告警触发 → 采集器捕获（合成帧源）→ hard-sample 上云。
	v0400createRule(t, base, "e2e-v045-alm", "gt", 28, "critical")
	v0370Do(t, "POST", base+"/api/v1/nodes/"+nodeID+"/rules/sync", nil, http.StatusOK)
	online := v0400waitAlarms(t, base, func(l v0400AlarmList) bool {
		for _, a := range l.Alarms {
			if a.NodeID == nodeID && a.Severity == "critical" {
				return true
			}
		}
		return false
	}, 90*time.Second)
	var alarmID string
	for _, a := range online.Alarms {
		if a.NodeID == nodeID && a.Severity == "critical" {
			alarmID = a.AlarmID
		}
	}
	if alarmID == "" {
		t.Fatal("告警未触发")
	}
	t.Logf("告警已触发（alarmID=%s）——困难样本应经采集器入队上云", alarmID)

	// 5. US-4：按 alarmId 过滤检索 → 内容回读 sha256 一致（回放铁证）。
	var found map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var list struct {
			Items []map[string]any `json:"items"`
			Count int              `json:"count"`
		}
		v0450GetJSON(t, base+"/api/v1/hardsamples?alarmId="+alarmID, &list, http.StatusOK)
		if list.Count >= 1 {
			found = list.Items[0]
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("困难样本未出现在检索列表（采集/入队/补传链断裂）")
	}
	mediaID, _ := found["mediaId"].(string)
	shaHex, _ := found["sha256"].(string)
	if mediaID == "" || len(shaHex) != 64 {
		t.Fatalf("列表项异常: %v", found)
	}
	resp, err := http.Get(base + "/api/v1/hardsamples/" + mediaID + "/content")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, rerr := resp.Body.Read(tmp)
		body = append(body, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("内容回读 = %d", resp.StatusCode)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != shaHex {
		t.Fatalf("内容 sha256 不一致（got %s want %s）", hex.EncodeToString(sum[:])[:16], shaHex[:16])
	}
	if len(body) < 4 || body[0] != 0xff || body[1] != 0xd8 {
		t.Fatalf("内容非 JPEG（%d 字节）", len(body))
	}
	t.Logf("US-3/US-4 困难样本全链铁证 OK：mediaId=%s JPEG %d 字节 sha256=%s…", mediaID, len(body), shaHex[:16])
}
