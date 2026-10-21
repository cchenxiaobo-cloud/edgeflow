package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestV0520DRE2E（v0.52.0，spec 0025）：灾备备份管理面五段——
// US-1 未启用语义（DR_DIR 空：GET enabled=false / POST·详情 503）/
// US-2 创建备份（region 快照落盘 → manifest 文件+sha256+自检位）/ US-3 清单与
// 详情（?verify=true 复核；篡改副本 → verified=false 回写）/ US-4 RBAC（viewer
// POST 403 / 匿名 401 / operator POST 200 / viewer GET 200）/ US-5 轮转
// （DR_KEEP=2 三次创建 → 最旧被删）。
// 复用 v0490 云端环境形态（RBAC on + 三凭证）；drstore 单测深覆盖轮转/并发/
// 缺失源/路径穿越；本 e2e 聚焦 API 面、启用语义与 RBAC。
func TestV0520DRE2E(t *testing.T) {
	buildBinaries(t)
	usersFile := filepath.Join(t.TempDir(), "users.json")
	preUsers := `[{"id":"boot-admin","hash":"` + v0460Sha256("admin-pass-1") + `","role":"admin"},` +
		`{"id":"op-1","hash":"` + v0460Sha256("op-pass-1") + `","role":"operator"},` +
		`{"id":"view-1","hash":"` + v0460Sha256("view-pass-1") + `","role":"viewer"}]`
	if err := os.WriteFile(usersFile, []byte(preUsers), 0o600); err != nil {
		t.Fatal(err)
	}
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"

	// ─── US-1 未启用语义（DR_DIR 未配置）───
	t.Logf("US-1 未启用语义：GET enabled=false / POST·详情 503")
	httpPort1, hubPort1 := reservePort(t), reservePort(t)
	p1 := v0520StartCloudcore(t, httpPort1, hubPort1, usersFile)
	base1 := "http://127.0.0.1:" + strconv.Itoa(httpPort1)

	code, body := v0490TermDo(t, http.MethodGet, base1+"/api/v1/dr/backups", viewToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-1 清单（未启用）→ %d body=%s", code, body)
	}
	var listResp struct {
		Enabled bool `json:"enabled"`
	}
	json.Unmarshal([]byte(body), &listResp)
	if listResp.Enabled {
		t.Fatalf("US-1 未启用时应 enabled=false: %s", body)
	}
	if code, body = v0490TermDo(t, http.MethodPost, base1+"/api/v1/dr/backups", opToken, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("US-1 POST（未启用）→ %d（want 503）body=%s", code, body)
	}
	if code, body = v0490TermDo(t, http.MethodGet, base1+"/api/v1/dr/backups/bk-nope", viewToken, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("US-1 详情（未启用）→ %d（want 503）body=%s", code, body)
	}
	p1.stop()

	// ─── US-2/3/4/5 启用语义（DR_DIR + REGION_FILE 配置）───
	t.Logf("US-2 创建备份：region 快照落盘 → manifest+sha256+自检位")
	root := t.TempDir()
	regionFile := filepath.Join(root, "region.json")
	drDir := filepath.Join(root, "dr")
	httpPort2, hubPort2 := reservePort(t), reservePort(t)
	p2 := v0520StartCloudcore(t, httpPort2, hubPort2, usersFile,
		"EDGEFLOW_CLOUDCORE_REGION_FILE="+regionFile,
		"EDGEFLOW_CLOUDCORE_DR_DIR="+drDir,
		"EDGEFLOW_CLOUDCORE_DR_KEEP=2",
	)
	defer p2.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort2)

	// 建区域 → regionstore 快照落盘（轮询等文件出现）
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "华东"}); code != http.StatusCreated {
		t.Fatalf("US-2 建区域 → %d body=%s", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(regionFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("US-2 regionstore 快照 5s 未落盘: %s", regionFile)
		}
		time.Sleep(100 * time.Millisecond)
	}

	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/dr/backups", opToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-2 创建备份 → %d body=%s", code, body)
	}
	var bk struct {
		ID    string `json:"id"`
		Files []struct {
			Name   string `json:"name"`
			Sha256 string `json:"sha256"`
			Bytes  int64  `json:"bytes"`
		} `json:"files"`
		TotalBytes int64 `json:"totalBytes"`
		Verified   bool  `json:"verified"`
	}
	json.Unmarshal([]byte(body), &bk)
	if bk.ID == "" || len(bk.Files) != 1 || bk.Files[0].Name != "region.snapshot.json" ||
		len(bk.Files[0].Sha256) != 64 || !bk.Verified {
		t.Fatalf("US-2 备份记录不符: %s", body)
	}
	if bk.TotalBytes != bk.Files[0].Bytes {
		t.Fatalf("US-2 总字节不符: %s", body)
	}
	copyPath := filepath.Join(drDir, bk.ID, bk.Files[0].Name)
	if data, err := os.ReadFile(copyPath); err != nil || len(data) == 0 {
		t.Fatalf("US-2 副本文件缺失: %v", err)
	}

	// ─── US-3 清单与详情（?verify=true 复核 + 篡改检测）───
	t.Logf("US-3 清单与详情：verify=true 复核；篡改 → verified=false 回写")
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/dr/backups", viewToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-3 清单 → %d", code)
	}
	var list2 struct {
		Enabled bool `json:"enabled"`
		Backups []struct {
			ID string `json:"id"`
		} `json:"backups"`
	}
	json.Unmarshal([]byte(body), &list2)
	if !list2.Enabled || len(list2.Backups) != 1 || list2.Backups[0].ID != bk.ID {
		t.Fatalf("US-3 清单不符: %s", body)
	}
	if code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/dr/backups/"+bk.ID, viewToken, nil); code != http.StatusOK {
		t.Fatalf("US-3 详情 → %d body=%s", code, body)
	}
	// 篡改副本 → verify=false（结果回写 manifest）
	if err := os.WriteFile(copyPath, []byte(`{"tampered":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/dr/backups/"+bk.ID+"?verify=true", viewToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-3 复核 → %d body=%s", code, body)
	}
	var got struct {
		Verified bool `json:"verified"`
	}
	json.Unmarshal([]byte(body), &got)
	if got.Verified {
		t.Fatalf("US-3 篡改后应 verified=false: %s", body)
	}
	// 未触发复核的详情也读到回写结果
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/dr/backups/"+bk.ID, viewToken, nil)
	json.Unmarshal([]byte(body), &got)
	if code != http.StatusOK || got.Verified {
		t.Fatalf("US-3 复核结果应回写 manifest: %d %s", code, body)
	}
	// 未知 id → 404
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/dr/backups/bk-nope", viewToken, nil); code != http.StatusNotFound {
		t.Fatalf("US-3 未知 id → %d（want 404）", code)
	}

	// ─── US-4 RBAC：viewer POST 403 / 匿名 401 / viewer GET 200 / operator POST 200 ───
	t.Logf("US-4 RBAC 三态")
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/dr/backups", viewToken, nil); code != http.StatusForbidden {
		t.Fatalf("US-4 viewer POST → %d（want 403）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/dr/backups", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("US-4 匿名 POST → %d（want 401）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/dr/backups", viewToken, nil); code != http.StatusOK {
		t.Fatalf("US-4 viewer GET → %d（want 200）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/dr/backups", opToken, nil); code != http.StatusOK {
		t.Fatalf("US-4 operator POST → %d（want 200）", code)
	}

	// ─── US-5 轮转：DR_KEEP=2，共创建 4 份 → 清单只剩最新 2 份，最旧被删 ───
	t.Logf("US-5 轮转：keep=2 → 最旧被删")
	for i := 0; i < 2; i++ {
		time.Sleep(15 * time.Millisecond) // 错开 CreatedAt 毫秒
		if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/dr/backups", opToken, nil); code != http.StatusOK {
			t.Fatalf("US-5 追加创建 #%d → %d body=%s", i, code, body)
		}
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		var l struct {
			Backups []struct {
				ID string `json:"id"`
			} `json:"backups"`
		}
		code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/dr/backups", viewToken, nil)
		json.Unmarshal([]byte(body), &l)
		if code == http.StatusOK && len(l.Backups) == 2 {
			found := false
			for _, b := range l.Backups {
				if b.ID == bk.ID {
					found = true
				}
			}
			if !found {
				break // 最旧（US-2 那份，且已被篡改）被轮转删除
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("US-5 轮转未生效（5s）：清单=%s", body)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("US-5 完成（最旧备份已轮转，保留 2 份）")
}

// v0520StartCloudcore：RBAC on + 调用方附加 env（DR_DIR/DR_KEEP/REGION_FILE 等）。
func v0520StartCloudcore(t *testing.T, httpPort, hubPort int, usersFile string, extra ...string) *proc {
	t.Helper()
	env := append(v0400CloudEnv(httpPort, hubPort),
		"EDGEFLOW_CLOUDCORE_RBAC=on",
		"EDGEFLOW_CLOUDCORE_RBAC_FILE="+usersFile,
	)
	env = append(env, extra...)
	p := startProcess(t, "cloudcore-v052", filepath.Join(binDir, "cloudcore"), nil, env)
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	// 就绪等待（嵌入式 etcd 启动 ~10s；对齐 v0490 教训）
	if !waitHTTP(t, 30*time.Second, base+"/healthz", nil) {
		p.stop()
		t.Fatalf("cloudcore-v052 未就绪（端口 %d/%d）", httpPort, hubPort)
	}
	return p
}
