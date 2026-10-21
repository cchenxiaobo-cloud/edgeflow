// v0520_test.go：drstore 备份管理面单测（spec 0025）。
package drstore

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// writeSource 落一个源快照文件。
func writeSource(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// US-1 创建备份：manifest 字段/副本内容/sha256/自检位；缺失源跳过；Get 回读一致。
func TestV0520CreateAndManifest(t *testing.T) {
	root := t.TempDir()
	src1 := writeSource(t, root, "region.json", `{"regions":[]}`)
	_ = writeSource(t, root, "tag.json", `{"tags":[]}`) // 存在但未注入（不在 sources）
	s := New(filepath.Join(root, "dr"), []Source{
		{Name: "region", Path: src1},
		{Name: "missing", Path: filepath.Join(root, "not-exist.json")}, // 缺失：跳过
	}, 7)

	b, err := s.Create("manual", 1700000000000)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(b.ID) < 4 || b.ID[:3] != "bk-" {
		t.Fatalf("备份 id 形态不符: %s", b.ID)
	}
	if b.CreatedAt != 1700000000000 || b.Trigger != "manual" {
		t.Fatalf("manifest 元数据不符: %+v", b)
	}
	if len(b.Files) != 1 {
		t.Fatalf("files=%d（缺失源应跳过）", len(b.Files))
	}
	f := b.Files[0]
	if f.Name != "region.snapshot.json" || f.Bytes != int64(len(`{"regions":[]}`)) || len(f.Sha256) != 64 {
		t.Fatalf("文件记录不符: %+v", f)
	}
	if !b.Verified || b.TotalBytes != f.Bytes {
		t.Fatalf("自检位/总字节不符: verified=%v total=%d", b.Verified, b.TotalBytes)
	}
	data, err := os.ReadFile(filepath.Join(root, "dr", b.ID, f.Name))
	if err != nil || string(data) != `{"regions":[]}` {
		t.Fatalf("副本内容不符: %v %q", err, data)
	}
	got, err := s.Get(b.ID)
	if err != nil || got.ID != b.ID || len(got.Files) != 1 || !got.Verified {
		t.Fatalf("Get 回读不符: %v %+v", err, got)
	}
}

// US-2 轮转：keep=2 创建 3 份 → 最旧整目录被删，清单按创建时间倒序保留 2 份。
func TestV0520Rotation(t *testing.T) {
	root := t.TempDir()
	src := writeSource(t, root, "region.json", `{"a":1}`)
	s := New(filepath.Join(root, "dr"), []Source{{Name: "region", Path: src}}, 2)
	ids := make([]string, 0, 3)
	for i, ts := range []int64{1000, 2000, 3000} {
		b, err := s.Create("manual", ts)
		if err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
		ids = append(ids, b.ID)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("轮转后备份=%d（want 2）", len(list))
	}
	if list[0].CreatedAt != 3000 || list[1].CreatedAt != 2000 {
		t.Fatalf("清单排序不符: %d,%d", list[0].CreatedAt, list[1].CreatedAt)
	}
	for _, b := range list {
		if b.ID == ids[0] {
			t.Fatalf("最旧备份 %s 应被轮转删除", ids[0])
		}
	}
	if _, err := os.Stat(filepath.Join(root, "dr", ids[0])); !os.IsNotExist(err) {
		t.Fatalf("最旧备份目录应已删除: %v", err)
	}
}

// US-3 完整性复核：副本被篡改 → verified=false；恢复内容 → true（结果回写 manifest）。
func TestV0520VerifyTamper(t *testing.T) {
	root := t.TempDir()
	src := writeSource(t, root, "region.json", `{"a":1}`)
	s := New(filepath.Join(root, "dr"), []Source{{Name: "region", Path: src}}, 7)
	b, err := s.Create("manual", 1000)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(root, "dr", b.ID, b.Files[0].Name)
	if err := os.WriteFile(copyPath, []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Verify(b.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Verified {
		t.Fatalf("篡改后应 verified=false")
	}
	// 回写持久化：直接 Get（不走 Verify）也应读到 false
	g2, err := s.Get(b.ID)
	if err != nil || g2.Verified {
		t.Fatalf("复核结果应回写 manifest: %v %+v", err, g2)
	}
	_ = os.WriteFile(copyPath, []byte(`{"a":1}`), 0o600)
	got, err = s.Verify(b.ID)
	if err != nil || !got.Verified {
		t.Fatalf("恢复后应 verified=true: %v %+v", err, got)
	}
}

// US-4 未启用语义与防御：dir 空 = 全路径 ErrDisabled；未知/穿越 id = ErrNotFound；
// 空目录清单 = 空切片非 nil。
func TestV0520DisabledAndGuard(t *testing.T) {
	s := New("", nil, 0)
	if s.Enabled() {
		t.Fatalf("空目录应未启用")
	}
	if _, err := s.Create("manual", 1); err != ErrDisabled {
		t.Fatalf("Create 应 ErrDisabled: %v", err)
	}
	if _, err := s.List(); err != ErrDisabled {
		t.Fatalf("List 应 ErrDisabled: %v", err)
	}
	if _, err := s.Get("bk-x"); err != ErrDisabled {
		t.Fatalf("Get 应 ErrDisabled: %v", err)
	}
	s2 := New(filepath.Join(t.TempDir(), "dr"), nil, 7)
	list, err := s2.List()
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("空目录清单不符: %v %+v", err, list)
	}
	if _, err := s2.Get("bk-nope"); err != ErrNotFound {
		t.Fatalf("未知 id 应 ErrNotFound: %v", err)
	}
	if _, err := s2.Get("../escape"); err != ErrNotFound {
		t.Fatalf("穿越 id 应 ErrNotFound: %v", err)
	}
	if _, err := s2.Verify("../escape"); err != ErrNotFound {
		t.Fatalf("Verify 穿越 id 应 ErrNotFound: %v", err)
	}
}

// US-5 并发创建：同刻多 goroutine 创建互不干扰（-race 下验证；keep=10 不触发轮转）。
func TestV0520ConcurrentCreate(t *testing.T) {
	root := t.TempDir()
	src := writeSource(t, root, "region.json", `{"a":1}`)
	s := New(filepath.Join(root, "dr"), []Source{{Name: "region", Path: src}}, 10)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Create("drill", int64(1000+i)); err != nil {
				t.Errorf("Create #%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	list, err := s.List()
	if err != nil || len(list) != 4 {
		t.Fatalf("并发创建后清单=%d err=%v（want 4）", len(list), err)
	}
}
