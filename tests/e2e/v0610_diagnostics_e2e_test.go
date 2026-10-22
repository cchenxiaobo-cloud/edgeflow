package e2e

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
)

// TestV0610DiagnosticsE2E（v0.61.0，spec 0034）：诊断聚合面两段——
// US-1 summary 结构与计数（regions/mutes/forwards 字段齐 + 节点在线口径）/
// US-2 audit 非法参数显式 400（R2 P3-2 处置）。复用 v0520 云端环境（无 edgecore，
// 纯管理面）。
func TestV0610DiagnosticsE2E(t *testing.T) {
	buildBinaries(t)
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := v0520StartCloudcore(t, httpPort, hubPort, scenarioUsers(t),
		"EDGEFLOW_CLOUDCORE_MUTE_FILE="+filepath.Join(t.TempDir(), "mute.json"))
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)

	// 造一点可聚合状态：区域 + 静默窗口
	if code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "diag 区"}); code != http.StatusCreated {
		t.Fatalf("建区域 → %d body=%s", code, body)
	}
	if code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/alerts/mutes", opToken,
		map[string]any{"durationSec": 300, "reason": "diag"}); code != http.StatusCreated {
		t.Fatalf("建静默 → %d body=%s", code, body)
	}

	// ─── US-1 诊断聚合 ───
	t.Logf("US-1 诊断聚合结构")
	code, body := v0490TermDo(t, http.MethodGet, base+"/api/v1/diagnostics/summary", viewToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-1 summary → %d body=%s", code, body)
	}
	var diag struct {
		Now       int64  `json:"now"`
		StartedAt int64  `json:"startedAt"`
		UptimeSec int64  `json:"uptimeSec"`
		GoVersion string `json:"goVersion"`
		Nodes     struct {
			Total  int `json:"total"`
			Online int `json:"online"`
		} `json:"nodes"`
		Alarms struct {
			Total  int64 `json:"total"`
			Active int64 `json:"active"`
		} `json:"alarms"`
		Regions struct {
			Total int `json:"total"`
		} `json:"regions"`
		Mutes struct {
			Total  int `json:"total"`
			Active int `json:"active"`
		} `json:"mutes"`
		Forwards struct {
			Total   int `json:"total"`
			Enabled int `json:"enabled"`
		} `json:"forwards"`
		Etcd  string `json:"etcd"`
		Audit struct {
			Path  string `json:"path"`
			Bytes int64  `json:"bytes"`
		} `json:"audit"`
	}
	json.Unmarshal([]byte(body), &diag)
	if diag.UptimeSec < 0 || diag.GoVersion == "" || diag.Etcd == "" {
		t.Fatalf("US-1 基础字段不符: %s", body)
	}
	if diag.Regions.Total != 1 || diag.Mutes.Total != 1 || diag.Mutes.Active != 1 {
		t.Fatalf("US-1 聚合计数不符: %s", body)
	}
	if diag.StartedAt > diag.Now {
		t.Fatalf("US-1 startedAt 不应晚于 now")
	}

	// ─── US-2 audit 非法参数显式 400（R2 P3-2 处置）───
	t.Logf("US-2 audit 非法参数 400")
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/audit/entries?limit=-5", viewToken, nil); code != http.StatusBadRequest {
		t.Fatalf("US-2 limit=-5 → %d（want 400）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/audit/entries?offset=abc", viewToken, nil); code != http.StatusBadRequest {
		t.Fatalf("US-2 offset=abc → %d（want 400）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/audit/stats?status=-1", viewToken, nil); code != http.StatusBadRequest {
		t.Fatalf("US-2 status=-1 → %d（want 400）", code)
	}
	t.Logf("US-2 完成（三组非法参数显式 400）")
}
