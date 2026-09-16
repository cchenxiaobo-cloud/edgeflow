package e2e

// v0.38.0 端到端：边缘时序存储（G17）。
//
// 链路：cloudcore（真实进程）+ edgecore（真实进程，TSDB 开启，采集周期
// 1s，内置 mock_sensor）→ 采集值经采样管道 sink 落时序库 → 优雅停机
// （刷盘关闭）→ 直接打开数据目录验证（序列/点数/值）→ 重启（同目录恢复）
// → 继续采集 → 点数增长（恢复 + 追加）。
import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/tsdb"
)

// v0380StartEdgecore 启动 edgecore（TSDB 用例专用）：在 edgeEnv 基础上把
// 设备上报/调谐周期替换为合法最小值 1s。说明：edgeEnv 默认的 300ms 低于
// 周期配置合法下限（1s，见 pkg/config/edgecore.go minEdgeCoreInterval），
// 会被区间校验回退为默认 30s（历史基建现象）——本用例需要真实多轮采集，
// 故显式替换（env 数组去重替换，避免重复键语义不确定）。
func v0380StartEdgecore(t *testing.T, root, nodeID string, hubPort int, tsdbDir string) *proc {
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
		"EDGEFLOW_EDGECORE_TSDB=on",
		"EDGEFLOW_EDGECORE_TSDB_DIR="+tsdbDir,
		"EDGEFLOW_EDGECORE_TSDB_RETENTION=24h",
	)
	return startProcess(t, "edgecore-"+nodeID, filepath.Join(binDir, "edgecore"), nil, env)
}

// TestV0380TSDBE2E 验证 采集→落盘→重启恢复 全链。
func TestV0380TSDBE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	// TSDB 数据目录固定为测试临时目录，便于停机后直接打开验证。
	tsdbDir := filepath.Join(t.TempDir(), "tsdb")
	nodeID := "e2e-v038-1"
	edge := v0380StartEdgecore(t, root, nodeID, hubPort, tsdbDir)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册（TSDB dir=%s）", nodeID, tsdbDir)

	// 1. 等待采集循环多轮（上报周期 1s；mock_sensor：sensor-01 温湿度）
	time.Sleep(6 * time.Second)

	// 2. 优雅停机（SIGINT → tsdb 刷盘关闭）
	edge.stop()
	t.Logf("edgecore 已停止（第一次）")

	// 3. 直接打开数据目录验证落盘
	db, err := tsdb.Open(tsdb.Options{Dir: tsdbDir})
	if err != nil {
		t.Fatalf("打开时序库失败: %v", err)
	}
	series := db.Series()
	if len(series) < 2 {
		db.Close()
		t.Fatalf("时序库序列不足（应含温湿度）: %v", series)
	}
	st := db.Stats()
	if st.Points < 4 {
		db.Close()
		t.Fatalf("时序库点数不足（应有 ≥2 轮采集）: %+v", st)
	}
	var tempSeries string
	for _, s := range series {
		if strings.HasSuffix(s, "/temperature") {
			tempSeries = s
			break
		}
	}
	if tempSeries == "" {
		db.Close()
		t.Fatalf("未找到温度序列: %v", series)
	}
	pts, err := db.Query(tempSeries, nil, 0, 0)
	if err != nil || len(pts) < 2 {
		db.Close()
		t.Fatalf("温度序列查询失败（应 ≥2 点）: n=%d err=%v", len(pts), err)
	}
	firstPoints := st.Points
	t.Logf("第一次验证：序列 %d 个，点 %d 个；温度序列 %s 共 %d 点（最新值 %v）",
		len(series), st.Points, tempSeries, len(pts), pts[len(pts)-1].Value)
	if err := db.Close(); err != nil {
		t.Fatalf("关闭验证实例失败: %v", err)
	}

	// 4. 重启（同 TSDB 目录恢复）：继续采集
	edge2 := v0380StartEdgecore(t, root, nodeID, hubPort, tsdbDir)
	waitNodeRegistered(t, base, nodeID)
	time.Sleep(3 * time.Second)
	edge2.stop()
	t.Logf("edgecore 已停止（第二次）")

	// 5. 再次打开验证：点数增长（恢复 + 追加）
	db2, err := tsdb.Open(tsdb.Options{Dir: tsdbDir})
	if err != nil {
		t.Fatalf("第二次打开失败: %v", err)
	}
	defer db2.Close()
	st2 := db2.Stats()
	if st2.Points <= firstPoints {
		t.Fatalf("重启后点数未增长（%d → %d，恢复或追加失败）", firstPoints, st2.Points)
	}
	pts2, err := db2.Query(tempSeries, nil, 0, 0)
	if err != nil || len(pts2) <= len(pts) {
		t.Fatalf("重启后温度序列未增长: n=%d err=%v", len(pts2), err)
	}
	t.Logf("第二次验证：点 %d → %d，温度序列 %d → %d 点（恢复+追加成功）",
		firstPoints, st2.Points, len(pts), len(pts2))
}
