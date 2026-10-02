// v0.43.0 端到端：视频管理面（G21）——媒资（快照/片段）断网补传与回放。
//
// 链路：cloudcore（真实进程，媒资目录） + edgecore（真实进程：视频 media
// 采集开启 + 上行补传开启 + 本地推理 stub + 合成源）→ 在线采集上云（快照/
// 片段经 API 可回放、字节校验）→ 停云断网（edge 持续采集 → spool + 补传
// 队列积压）→ 同端口/同数据目录/同媒资目录重启云 → 补传重放（断网窗口内
// capturedAt 的新片段到达并可回放——重放铁证）。
//
// 验收对齐（发展规划 v0.43）：契约测试更新全绿（新增端点登记，见
// tests/contract）；快照/片段端到端（断网补传后的历史片段可回放）。
package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// v0430Stream 是 /api/v1/videostreams/{name} 响应子集。
type v0430Stream struct {
	Name            string     `json:"name"`
	SnapshotMediaID string     `json:"snapshotMediaId"`
	Segments        []v0430Seg `json:"segments"`
}

// v0430Seg 是片段索引元素子集。
type v0430Seg struct {
	MediaID    string `json:"mediaId"`
	CapturedAt int64  `json:"capturedAt"`
	FrameCount int    `json:"frameCount"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
}

// v0430StartCloud 启动 cloudcore（媒资目录注入；复用 cloudDataDir 支持重启恢复）。
func v0430StartCloud(t *testing.T, httpPort, hubPort int, mediaDir string) *proc {
	t.Helper()
	env := append(cloudEnv(httpPort, hubPort), "EDGEFLOW_CLOUDCORE_MEDIA_DIR="+mediaDir)
	p := startProcess(t, "cloudcore", filepath.Join(binDir, "cloudcore"),
		[]string{"--port", strconv.Itoa(httpPort)}, env)
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	if !waitHTTP(t, 15*time.Second, base+"/healthz", nil) {
		p.stop()
		t.Fatalf("cloudcore 未就绪（端口 %d/%d）", httpPort, hubPort)
	}
	return p
}

// v0430StartEdge 启动 edgecore（视频 media 采集 + 补传 + spool）。
func v0430StartEdge(t *testing.T, nodeID string, hubPort int, cfgPath, spoolDir string) *proc {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "edgecore.db")
	cloudAddr := "ws://127.0.0.1:" + strconv.Itoa(hubPort)
	env := edgeEnv(nodeID, cloudAddr, dbPath)
	env = append(env,
		"EDGEFLOW_EDGECORE_UPLINK=on",
		"EDGEFLOW_EDGECORE_UPLINK_REPORT_SEC=1",
		"EDGEFLOW_EDGECORE_UPLINK_BATCH=16",
		"EDGEFLOW_EDGECORE_UPLINK_RATE=50",
		"EDGEFLOW_VIDEO_MAPPER_CONFIG="+cfgPath,
		"EDGEFLOW_MEDIA_SPOOL_DIR="+spoolDir,
	)
	return startProcess(t, "edgecore-"+nodeID, filepath.Join(binDir, "edgecore"), nil, env)
}

// v0430GetStream 读取流详情（404 = 未创建）。
func v0430GetStream(t *testing.T, base, name string) (v0430Stream, int) {
	t.Helper()
	resp, err := http.Get(base + "/api/v1/videostreams/" + name)
	if err != nil {
		t.Fatalf("GET 流详情失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var st v0430Stream
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatalf("解析流详情失败: %v（体: %s）", err, string(b))
		}
		// 收集上传侧 sha256（供回放校验比对；复核 P2-5）。
		for _, s := range st.Segments {
			if s.SHA256 != "" {
				wantSHA[s.MediaID] = s.SHA256
			}
		}
	}
	return st, resp.StatusCode
}

// v0430WaitStream 轮询流详情直到谓词满足。
func v0430WaitStream(t *testing.T, base, name string, want func(v0430Stream) bool, timeout time.Duration) v0430Stream {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last v0430Stream
	for time.Now().Before(deadline) {
		last, _ = v0430GetStream(t, base, name)
		if want(last) {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待流条件超时（name=%s，当前快照=%q 片段=%d）", name, last.SnapshotMediaID, len(last.Segments))
	return last
}

// v0430Fetch 取媒体字节（附带头）。
func v0430Fetch(t *testing.T, url string) ([]byte, http.Header, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.Header, resp.StatusCode
}

// countJPEGSOI 统计 JPEG 起始符（FF D8）数量（合成帧内容确定，无转义歧义）。
func countJPEGSOI(b []byte) int {
	n := 0
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0xFF && b[i+1] == 0xD8 {
			n++
		}
	}
	return n
}

// v0430CheckSnapshot 校验快照回放（JPEG 完整性 + 帧数头）。
func v0430CheckSnapshot(t *testing.T, base, name string) {
	t.Helper()
	b, hdr, code := v0430Fetch(t, base+"/api/v1/videostreams/"+name+"/snapshot")
	if code != http.StatusOK {
		t.Fatalf("快照回放应 200: %d", code)
	}
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 || b[len(b)-2] != 0xFF || b[len(b)-1] != 0xD9 {
		t.Fatalf("快照应为完整 JPEG（SOI…EOI，%d 字节）", len(b))
	}
	if hdr.Get("X-Frame-Count") != "1" {
		t.Fatalf("快照 X-Frame-Count = %q", hdr.Get("X-Frame-Count"))
	}
}

// v0430CheckSegment 校验片段回放（JPEG 序列帧数 == 索引/头）。
func v0430CheckSegment(t *testing.T, base, name string, seg v0430Seg) {
	t.Helper()
	b, hdr, code := v0430Fetch(t, base+"/api/v1/videostreams/"+name+"/segments/"+seg.MediaID)
	if code != http.StatusOK {
		t.Fatalf("片段回放应 200: %d（%s）", code, seg.MediaID)
	}
	if len(b) == 0 || b[0] != 0xFF || b[1] != 0xD8 {
		t.Fatalf("片段应为 JPEG 序列: %s", seg.MediaID)
	}
	soi := countJPEGSOI(b)
	if soi != seg.FrameCount || strconv.Itoa(soi) != hdr.Get("X-Frame-Count") {
		t.Fatalf("片段帧数不一致（索引=%d，SOI=%d，头=%s）: %s",
			seg.FrameCount, soi, hdr.Get("X-Frame-Count"), seg.MediaID)
	}
	if int64(len(b)) != seg.Bytes {
		t.Fatalf("片段字节数不一致（索引=%d，实际=%d）: %s", seg.Bytes, len(b), seg.MediaID)
	}
	// sha256 显式比对（复核 P2-5 补强：与上传元数据全链闭环）。
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != wantSHA[seg.MediaID] {
		t.Fatalf("片段 sha256 与上传元数据不符: %s", seg.MediaID)
	}
}

// wantSHA 收集上传元数据中的 sha256（按 mediaId；v0430GetStream 时填充）。
var wantSHA = map[string]string{}

// TestV0430VideoManageE2E 验证 媒资采集上云 + 断网补传 + 快照/片段回放 全链。
func TestV0430VideoManageE2E(t *testing.T) {
	buildBinaries(t)

	// 推理 stub：检出恒真（驱动媒资采集触发）。
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"detections":[{"label":"motion","score":0.9,"bbox":[1,1,4,4]}]}`))
	}))
	defer stub.Close()

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	mediaDir := filepath.Join(t.TempDir(), "media")
	httpPort, hubPort := reservePort(t), reservePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	cloud := v0430StartCloud(t, httpPort, hubPort, mediaDir)

	// 视频配置：合成源 + 本地推理 stub + media 采集（节流 800ms、片段 6 帧）。
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "video.json")
	cfgJSON := fmt.Sprintf(`{"deviceName":"cam-e2e","source":{"type":"synthetic","synthetic":{"fps":5,"width":320,"height":240}},"inference":{"url":%q,"timeoutMs":2000},"media":{"enabled":true,"segmentFrames":6,"minIntervalMs":800}}`, stub.URL)
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("写视频配置: %v", err)
	}
	spoolDir := filepath.Join(dir, "spool")

	nodeID := "e2e-v043-1"
	v0430StartEdge(t, nodeID, hubPort, cfgPath, spoolDir)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册（视频媒资采集 + 补传开启）", nodeID)

	// 1. 在线：等首段媒资上云（快照 + ≥2 片段——片段多段确保索引与回放）。
	first := v0430WaitStream(t, base, "cam-e2e", func(st v0430Stream) bool {
		return st.SnapshotMediaID != "" && len(st.Segments) >= 2
	}, 120*time.Second)
	t.Logf("在线媒资已上云：快照=%s，片段=%d", first.SnapshotMediaID, len(first.Segments))

	// 回放校验：快照 JPEG + 每段片段帧数/字节（在线阶段的完整性证据）。
	v0430CheckSnapshot(t, base, "cam-e2e")
	for _, s := range first.Segments {
		v0430CheckSegment(t, base, "cam-e2e", s)
	}
	segCountBefore := len(first.Segments)

	// 2. 断网：停云 20s（edge 持续采集 → spool + 补传队列确定性积压）。
	tDown := time.Now().UnixMilli()
	cloud.stop()
	t.Logf("cloudcore 已停（断网模拟 tDown=%d），边侧持续采集 20s...", tDown)
	time.Sleep(20 * time.Second)
	tUp := time.Now().UnixMilli()

	// 3. 恢复：同端口/同数据目录/同媒资目录重启（索引经 Load 恢复）。
	cloud = v0430StartCloud(t, httpPort, hubPort, mediaDir)
	t.Logf("cloudcore 已重启（tUp=%d），等待补传重放...", tUp)

	// 4. 补传铁证：新片段到达，且至少一段 capturedAt < tUp（采集于云端离线
	//    窗口——只能经补传队列重放才可能出现于云端索引）。
	replayed := v0430WaitStream(t, base, "cam-e2e", func(st v0430Stream) bool {
		if len(st.Segments) <= segCountBefore {
			return false
		}
		for _, s := range st.Segments[segCountBefore:] {
			if s.CapturedAt >= tDown && s.CapturedAt < tUp {
				return true
			}
		}
		return false
	}, 150*time.Second)
	newSegs := replayed.Segments[segCountBefore:]
	inWindow := 0
	for _, s := range newSegs {
		if s.CapturedAt >= tDown && s.CapturedAt < tUp {
			inWindow++
		}
	}
	t.Logf("断网窗口媒资已补传：片段 %d → %d（窗内 %d 段）", segCountBefore, len(replayed.Segments), inWindow)
	if inWindow < 3 {
		t.Fatalf("断网窗口片段补传不足（窗内 %d < 3，窗 20s × 节流 800ms 应远多于 3）", inWindow)
	}

	// 5. 补传片段回放校验（窗内全部 + 每个都完整）。
	for _, s := range newSegs {
		if s.CapturedAt >= tDown && s.CapturedAt < tUp {
			v0430CheckSegment(t, base, "cam-e2e", s)
		}
	}
	// 快照仍可回放（离线窗内被替换为更新的帧）。
	v0430CheckSnapshot(t, base, "cam-e2e")

	// 6. 终态复核：索引自洽（快照 ID 形态 + 片段数一致）。
	again, code := v0430GetStream(t, base, "cam-e2e")
	if code != http.StatusOK || len(again.Segments) != len(replayed.Segments) {
		t.Fatalf("终态索引不一致: %d / %d（code=%d）", len(again.Segments), len(replayed.Segments), code)
	}
	if !strings.HasPrefix(again.SnapshotMediaID, "m-snapshot") {
		t.Fatalf("快照媒资 ID 形态异常: %q", again.SnapshotMediaID)
	}
	t.Logf("用例完成：在线媒资 %d 段 + 断网补传 %d 段（窗内 %d）全部回放校验通过",
		segCountBefore, len(newSegs), inWindow)
}
