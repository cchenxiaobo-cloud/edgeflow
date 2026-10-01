package e2e

// v0.41.0 端到端：采集扩展（REST 采集器 + 波形特征通道，G19/G13）。
//
// 链路：cloudcore（真实进程）+ edgecore（真实进程，REST 轮询 + 波形开启）
// → REST 采集器轮询测试 HTTP 服务端（注入固定值 restValue=42.5）→
//   rest-01 设备影子 → DeviceReport 上云 → /api/v1/nodes/{id}/devices 可见；
// → 波形模拟源 10kHz 出块 → 特征前置（rms/peak/domFreq…）→
//   vibration-01 影子 → 上云可见（domFreq ≈ 50Hz——注入基频的特征提取演示）。
//
// 验收对齐（发展规划 v0.41）：10kHz 振动模拟源连续采集+特征提取演示；
// REST 采集器轮询形态全链；现有协议路径零回归（本用例不触碰既有路径）。
import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/waveform"
)

// v0410StartEdgecore 启动 edgecore（采集扩展用例专用）：
// REST 轮询 + 波形开启（10kHz / 块 4096 / 500ms 出块）+ 1s 上报周期
// （同 v0390：REPORT_INTERVAL/RECONCILE_INTERVAL 替换为合法最小值 1s）。
func v0410StartEdgecore(t *testing.T, root, nodeID string, hubPort int, restURL string) *proc {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "edgecore.db")
	cloudAddr := "ws://127.0.0.1:" + strconv.Itoa(hubPort)
	env := edgeEnv(nodeID, cloudAddr, dbPath)
	for i, kv := range env {
		switch {
		case strings.HasPrefix(kv, "EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL="):
			env[i] = "EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=1s"
		case strings.HasPrefix(kv, "EDGEFLOW_EDGECORE_RECONCILE_INTERVAL="):
			env[i] = "EDGEFLOW_EDGECORE_RECONCILE_INTERVAL=1s"
		}
	}
	env = append(env,
		"EDGEFLOW_REST_URL="+restURL,
		"EDGEFLOW_EDGECORE_WAVEFORM=on",
		"EDGEFLOW_EDGECORE_WAVEFORM_RATE_HZ=10000",
		"EDGEFLOW_EDGECORE_WAVEFORM_INTERVAL_MS=500",
	)
	return startProcess(t, "edgecore-"+nodeID, filepath.Join(binDir, "edgecore"), nil, env)
}

// v0410DeviceProps 轮询设备面直到谓词满足（GET /api/v1/nodes/{id}/devices）。
func v0410DeviceProps(t *testing.T, base, nodeID string, want func(map[string]map[string]float64) bool, timeout time.Duration) map[string]map[string]float64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got struct {
		Items []struct {
			DeviceName string             `json:"deviceName"`
			Namespace  string             `json:"namespace"`
			Properties map[string]float64 `json:"properties"`
		} `json:"items"`
	}
	for time.Now().Before(deadline) {
		getJSON(t, base+"/api/v1/nodes/"+nodeID+"/devices", &got)
		byDevice := map[string]map[string]float64{}
		for _, it := range got.Items {
			byDevice[it.DeviceName] = it.Properties
		}
		if want(byDevice) {
			return byDevice
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待设备属性条件超时（当前 %d 台设备）", len(got.Items))
	return nil
}

// TestV0410CollectE2E 验证 REST 采集器 + 波形特征通道全链。
func TestV0410CollectE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	// REST 测试服务端：注入固定值 restValue=42.5（确定性断言）。
	restSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"values":{"restValue":42.5}}`))
	}))
	defer restSrv.Close()

	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID := "e2e-v041-1"
	edge := v0410StartEdgecore(t, root, nodeID, hubPort, restSrv.URL)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册（REST 轮询 + 波形开启）", nodeID)

	// 1. REST 采集器：rest-01 出现且 restValue = 42.5（轮询→影子→上云全链）。
	// 2. 波形通道：vibration-01 出现且特征合理（rms > 0.3、domFreq ≈ 50Hz、
	//    peak > 1——注入 50Hz 基波的 FFT 特征提取演示）。
	devices := v0410DeviceProps(t, base, nodeID, func(byDevice map[string]map[string]float64) bool {
		rest, okRest := byDevice["rest-01"]
		vib, okVib := byDevice["vibration-01"]
		if !okRest || !okVib {
			return false
		}
		return rest["restValue"] == 42.5 && vib["rms"] > 0.3
	}, 90*time.Second)
	restProps := devices["rest-01"]
	vibProps := devices["vibration-01"]
	t.Logf("REST 属性: %v", restProps)
	t.Logf("波形特征: %v", vibProps)

	if restProps["restValue"] != 42.5 {
		t.Fatalf("REST 注入值不符: %v", restProps)
	}
	if df := vibProps["domFreq"]; df < 47 || df > 53 {
		t.Fatalf("波形主频 = %v, want ≈50（±3，注入基频 50Hz 的 FFT 特征提取演示）", df)
	}
	if rms := vibProps["rms"]; rms <= 0.3 {
		t.Fatalf("rms = %v, want > 0.3（振动信号有效值）", rms)
	}

	// 3. 波形特征键完整性（六个特征键全部上云）。
	for _, k := range waveform.FeatureNames() {
		if _, ok := vibProps[k]; !ok {
			t.Fatalf("波形特征缺键 %s: %v", k, vibProps)
		}
	}

	// 4. 边侧日志锚点（诊断记录，不断言——tail 窗口可能被刷出）。
	if strings.Contains(edge.logTail(), "波形采集循环已启用") {
		t.Logf("边侧日志含波形启用锚点")
	}
	t.Logf("用例完成：REST 采集（restValue=42.5）+ 波形特征提取（domFreq≈50Hz）全链")
}
