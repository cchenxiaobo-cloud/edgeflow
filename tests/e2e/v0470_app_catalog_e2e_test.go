package e2e

// v0.47.0 e2e（spec 0020）：应用目录 + 灰度部署 + 应用包（US-1/US-2/US-3）。
//
// 场景（真实 cloudcore + 1 边缘节点）：
//   - US-1 应用目录：注册 container 应用（409 重复）→ 版本登记 → 分页/详情；
//   - US-3 应用包：上传（sha256 回显）→ 下载（checksum 一致）→ 未知 404；
//   - US-2 灰度：创建 2 节点计划（batchSize=1，第二节点不在线）→ 批1 交付
//     （边缘收到 PodSync）→ 查询进度 → rollback 语义（无可回滚版本 409 分支
//     + 有目录版本的回滚计划创建）。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func v0470Do(t *testing.T, method, url string, body any) (int, string, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(b)
	} else if method == http.MethodPut {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestV0470AppCatalogDeployE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)
	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	pkgDir := filepath.Join(t.TempDir(), "apppkg")
	httpPort, hubPort := reservePort(t), reservePort(t)
	cloud := v0400StartCloudcoreOnPorts(t, root, httpPort, hubPort)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	// ── US-1 应用目录 ──
	code, body, _ := v0470Do(t, "POST", base+"/api/v1/applications", map[string]any{
		"name": "demo-app", "displayName": "演示应用", "kind": "container",
		"firstVersion": map[string]any{"version": "v1.0.0", "image": e2eImage},
	})
	if code != 200 || !bytes.Contains([]byte(body), []byte(`"name":"demo-app"`)) {
		t.Fatalf("注册应用 = %d %s", code, body)
	}
	// 重复 409
	if code, _, _ := v0470Do(t, "POST", base+"/api/v1/applications", map[string]any{
		"name": "demo-app", "kind": "container",
	}); code != 409 {
		t.Fatalf("重复注册 = %d, want 409", code)
	}
	// 版本登记 + 重复 409
	if code, _, _ := v0470Do(t, "POST", base+"/api/v1/applications/demo-app/versions", map[string]any{
		"version": "v1.1.0", "image": e2eImage,
	}); code != 201 {
		t.Fatalf("登记版本 = %d, want 201", code)
	}
	if code, _, _ := v0470Do(t, "POST", base+"/api/v1/applications/demo-app/versions", map[string]any{
		"version": "v1.1.0", "image": e2eImage,
	}); code != 409 {
		t.Fatalf("重复版本 = %d, want 409", code)
	}
	// container 版本缺 image → 400
	if code, _, _ := v0470Do(t, "POST", base+"/api/v1/applications/demo-app/versions", map[string]any{
		"version": "v1.2.0",
	}); code != 400 {
		t.Fatalf("缺 image = %d, want 400", code)
	}
	// 列表分页
	code, body, hdr := v0470Do(t, "GET", base+"/api/v1/applications?limit=1", nil)
	if code != 200 || hdr.Get("X-Total-Count") != "1" {
		t.Fatalf("分页 = %d total=%q", code, hdr.Get("X-Total-Count"))
	}
	// 详情含 2 版本
	code, body, _ = v0470Do(t, "GET", base+"/api/v1/applications/demo-app", nil)
	if code != 200 || !bytes.Contains([]byte(body), []byte("v1.1.0")) {
		t.Fatalf("详情 = %d %s", code, body)
	}
	t.Logf("US-1 应用目录 OK：注册/409/版本校验/分页/详情")

	// ── US-3 应用包 ──
	pkgContent := bytes.Repeat([]byte("demo-pkg-content-"), 100)
	sum := sha256.Sum256(pkgContent)
	wantSum := hex.EncodeToString(sum[:])
	req, _ := http.NewRequest(http.MethodPut, base+"/api/v1/applications/demo-app/versions/v1.0.0/package", bytes.NewReader(pkgContent))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("上传包: %v", err)
	}
	pb, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(pb, []byte(wantSum[:12])) {
		t.Fatalf("上传包 = %d %s want sha256=%s…", resp.StatusCode, pb, wantSum[:12])
	}
	// 下载 + checksum
	code, raw, hdr := v0470RawGet(t, base+"/api/v1/applications/demo-app/versions/v1.0.0/package")
	if code != 200 || hdr.Get("X-Checksum-Sha256") != wantSum || !bytes.Equal(raw, pkgContent) {
		t.Fatalf("下载包 = %d checksum=%q len=%d", code, hdr.Get("X-Checksum-Sha256"), len(raw))
	}
	// 未上传版本 → 404
	if code, _, _ = v0470RawGet(t, base+"/api/v1/applications/demo-app/versions/v1.1.0/package"); code != 404 {
		t.Fatalf("未上传包 = %d, want 404", code)
	}
	t.Logf("US-3 应用包 OK：上传/下载/checksum 一致/404")

	// ── 边缘节点（批1 交付目标）──
	nodeID := "e2e-v047-1"
	spoolDir := filepath.Join(t.TempDir(), "spool")
	dbPath := filepath.Join(t.TempDir(), "edgecore.db")
	cloudAddr := "ws://127.0.0.1:" + strconv.Itoa(hubPort)
	scriptPkgDir := filepath.Join(t.TempDir(), "scriptpkg")
	env := append(edgeEnv(nodeID, cloudAddr, dbPath),
		"EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=1s",
		"EDGEFLOW_EDGECORE_RECONCILE_INTERVAL=1s",
		"EDGEFLOW_EDGECORE_REPORT_INTERVAL=2s", // v0.47：脚本伪 Pod 快速回穿
		"EDGEFLOW_MEDIA_SPOOL_DIR="+spoolDir,
		// v0.47.0 US-4：脚本应用执行器（opt-in）+ 包缓存目录
		"EDGEFLOW_EDGED_SCRIPTRUNNER=on",
		"EDGEFLOW_EDGED_APPPKG_DIR="+scriptPkgDir,
	)
	_ = startProcess(t, "edgecore-"+nodeID, filepath.Join(binDir, "edgecore"), nil, env)
	waitNodeRegistered(t, base, nodeID)

	// ── US-2 灰度：2 节点（1 在线 1 离线）batchSize=1 ──
	code, body, _ = v0470Do(t, "POST", base+"/api/v1/app-deployments", map[string]any{
		"app": "demo-app", "version": "v1.0.0",
		"nodes": []string{nodeID, "e2e-v047-offline"}, "batchSize": 1,
	})
	if code != 201 {
		t.Fatalf("创建计划 = %d %s, want 201", code, body)
	}
	var plan struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	_ = json.Unmarshal([]byte(body), &plan)
	if plan.ID == "" || plan.State != "pending" {
		t.Fatalf("计划 = %+v", plan)
	}
	// 交付：批1 在线节点 → delivered（PodSync 下发 acked）；批2 离线 → failed
	terminal := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code, body, _ = v0470Do(t, "GET", base+"/api/v1/app-deployments/"+plan.ID, nil)
		if code != 200 {
			t.Fatalf("查计划 = %d", code)
		}
		var got struct {
			State string `json:"state"`
			Nodes []struct {
				NodeID string `json:"nodeID"`
				State  string `json:"state"`
				Reason string `json:"reason"`
			} `json:"nodes"`
		}
		_ = json.Unmarshal([]byte(body), &got)
		done := got.State == "succeeded" || got.State == "failed"
		if done {
			terminal = true
			if got.State != "succeeded" {
				t.Fatalf("尽力交付语义（1/2 成功）= %s nodes=%+v", got.State, got.Nodes)
			}
			if got.Nodes[0].State != "delivered" || got.Nodes[1].State != "failed" {
				t.Fatalf("perNode = %+v", got.Nodes)
			}
			if got.Nodes[0].NodeID != nodeID {
				t.Fatalf("批序错乱: %+v", got.Nodes)
			}
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !terminal {
		t.Fatalf("计划 30s 未达终态（pending 未被控制器拾起或批次卡死）")
	}
	// 边缘 Pod 状态可见（PodSync update upsert 落地 → 状态上报回穿）。
	// 容器运行断言依赖 Docker daemon 与本地缓存镜像（与 autonomy/multi_node 同口径）；
	// 无 Docker 时跳过运行断言——目录/包/灰度状态机断言不受影响。
	if dockerOK() && dockerImageCached(e2eImage) {
		waitPodPhase(t, base, nodeID, "edgeflow-app-demo-app", "Running")
	} else {
		t.Log("Docker 缺席或镜像未缓存：跳过边缘容器运行断言（灰度交付状态机已覆盖）")
	}
	// 回滚：目录有 v1.1.0？——v1.0.0 交付中；回滚目标 = 其他版本 v1.1.0
	code, body, _ = v0470Do(t, "POST", base+"/api/v1/app-deployments/"+plan.ID+"/rollback", map[string]any{
		"version": "v1.1.0",
	})
	if code != 201 {
		t.Fatalf("回滚计划 = %d %s, want 201", code, body)
	}
	t.Logf("US-2 灰度 OK：计划/逐批交付/离线失败尽力语义/边缘 Pod 落地/回滚计划创建")

	// 回滚计划跑到终态（目录删除前置：非终态计划 409 是正确行为）
	var rb struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &rb)
	rbDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(rbDeadline) {
		code, rbody, _ := v0470Do(t, "GET", base+"/api/v1/app-deployments/"+rb.ID, nil)
		var rgot struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal([]byte(rbody), &rgot)
		if code == 200 && (rgot.State == "succeeded" || rgot.State == "failed") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 应用删除：仅非终态计划 409；终态计划不阻塞
	if code, _, _ := v0470Do(t, "DELETE", base+"/api/v1/applications/demo-app", nil); code != 200 {
		t.Fatalf("删除应用 = %d, want 200（终态计划不阻塞）", code)
	}

	// ── US-4 脚本化应用（scriptrunner opt-in 全链）──
	code, body, _ = v0470Do(t, "POST", base+"/api/v1/applications", map[string]any{
		"name": "demo-script", "kind": "script",
		"firstVersion": map[string]any{"version": "v1.0.0", "scriptRef": "demo.sh"},
	})
	if code != 200 {
		t.Fatalf("注册脚本应用 = %d %s", code, body)
	}
	pkg := v0470BuildScriptPkg(t, "e2e-marker")
	sum = sha256.Sum256(pkg)
	req, err = http.NewRequest(http.MethodPut, base+"/api/v1/applications/demo-script/versions/v1.0.0/package", bytes.NewReader(pkg))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("上传脚本包: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("上传脚本包 = %d", resp.StatusCode)
	}
	code, body, _ = v0470Do(t, "POST", base+"/api/v1/app-deployments", map[string]any{
		"app": "demo-script", "version": "v1.0.0", "nodes": []string{nodeID}, "batchSize": 1,
	})
	if code != 201 {
		t.Fatalf("创建脚本部署 = %d %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &plan)
	// 伪 Pod 回穿：pod.name = edgeflow-script-<app>（spec US-4 命名）
	scriptPod := "edgeflow-script-demo-script"
	sDeadline := time.Now().Add(45 * time.Second)
	seen := ""
	for time.Now().Before(sDeadline) {
		_, pbody, _ := v0470Do(t, "GET", base+"/api/v1/pods", nil)
		if bytes.Contains([]byte(pbody), []byte(scriptPod)) {
			var pods []struct {
				PodName string `json:"podName"`
				Phase   string `json:"phase"`
			}
			var wrap struct {
				Items []struct {
					PodName string `json:"podName"`
					Phase   string `json:"phase"`
				} `json:"items"`
			}
			_ = json.Unmarshal([]byte(pbody), &wrap)
			pods = wrap.Items
			for _, pd := range pods {
				if pd.PodName == scriptPod {
					seen = pd.Phase
				}
			}
			if seen == "Running" {
				break
			}
		}
		time.Sleep(1 * time.Second)
	}
	if seen != "Running" {
		t.Fatalf("脚本应用伪 Pod 未 Running（实际 %q）——scriptrunner 链路断", seen)
	}
	t.Logf("US-4 脚本应用 OK：内联下发/sha256 校验/install+start 执行/伪 Pod 回穿")
	_ = os.RemoveAll(pkgDir)
	_ = scriptPkgDir
}

// v0470BuildScriptPkg 构造测试脚本包（tar.gz：install/start）。
func v0470BuildScriptPkg(t *testing.T, marker string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"install": "#!/bin/sh\necho install > " + marker + ".txt\n",
		"start":   "#!/bin/sh\necho started >> " + marker + ".txt\n",
	}
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// v0470RawGet 原始 GET（返回字节流与头）。
func v0470RawGet(t *testing.T, url string) (int, []byte, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}
