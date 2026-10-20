package tagstore

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func strPtr(s string) *string { return &s }

// TestV0500TagUpsert US-3：打标幂等 + 变更计数。
func TestV0500TagUpsert(t *testing.T) {
	s := New("")
	changed, err := s.SetTags(AnchorNode, "n1", map[string]*string{
		"line": strPtr("A1"), "floor": strPtr("3"),
	})
	if err != nil || changed != 2 {
		t.Fatalf("打标不符: changed=%d err=%v", changed, err)
	}
	// 同值重打 → 0 变更（幂等）
	changed, err = s.SetTags(AnchorNode, "n1", map[string]*string{"line": strPtr("A1")})
	if err != nil || changed != 0 {
		t.Fatalf("幂等重打应 0 变更: changed=%d err=%v", changed, err)
	}
	// 值变更 → 1
	changed, _ = s.SetTags(AnchorNode, "n1", map[string]*string{"line": strPtr("B2")})
	if changed != 1 {
		t.Fatalf("值变更应计 1: %d", changed)
	}
	tags := s.TagsOf(AnchorNode, "n1")
	if tags["line"] != "B2" || tags["floor"] != "3" || len(tags) != 2 {
		t.Fatalf("标签视图不符: %+v", tags)
	}
}

// TestV0500TagDelete US-3：null 删标 + 空壳回收。
func TestV0500TagDelete(t *testing.T) {
	s := New("")
	if _, err := s.SetTags(AnchorNode, "n1", map[string]*string{"k": strPtr("v")}); err != nil {
		t.Fatal(err)
	}
	changed, err := s.SetTags(AnchorNode, "n1", map[string]*string{"k": nil, "ghost": nil})
	if err != nil || changed != 1 {
		t.Fatalf("删标不符: changed=%d err=%v", changed, err)
	}
	if tags := s.TagsOf(AnchorNode, "n1"); len(tags) != 0 {
		t.Fatalf("应空: %+v", tags)
	}
	if s.Count() != 0 {
		t.Fatalf("空壳应回收: %d", s.Count())
	}
}

// TestV0500TagAnchorIsolation 裁决 5：三类锚点同 ID 互不干扰。
func TestV0500TagAnchorIsolation(t *testing.T) {
	s := New("")
	if _, err := s.SetTags(AnchorNode, "x1", map[string]*string{"role": strPtr("edge")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTags(AnchorModel, "x1", map[string]*string{"role": strPtr("detector")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTags(AnchorDevice, "n1/temp/传感器", map[string]*string{"vendor": strPtr("acme")}); err != nil {
		t.Fatalf("device 复合锚点应可用: %v", err)
	}
	if tags := s.TagsOf(AnchorNode, "x1"); tags["role"] != "edge" || len(tags) != 1 {
		t.Fatalf("node 锚点被污染: %+v", tags)
	}
	if tags := s.TagsOf(AnchorModel, "x1"); tags["role"] != "detector" || len(tags) != 1 {
		t.Fatalf("model 锚点被污染: %+v", tags)
	}
	if got := s.AnchorsWithTag(AnchorNode, "role", "edge"); len(got) != 1 || got[0] != "x1" {
		t.Fatalf("过滤不符: %v", got)
	}
	// 复核 P2-2：tagKey 存在性过滤（key 命中、value 不限）——spec 裁决 7 广告语义。
	if got := s.AnchorsWithTag(AnchorNode, "role", ""); len(got) != 1 || got[0] != "x1" {
		t.Fatalf("存在性过滤不符: %v", got)
	}
	if got := s.AnchorsWithTag(AnchorNode, "absent", ""); len(got) != 0 {
		t.Fatalf("不存在 key 应空: %v", got)
	}
	if got := s.AnchorsWithTag(AnchorDevice, "", ""); len(got) != 1 {
		t.Fatalf("device 全量不符: %v", got)
	}
}

// TestV0500TagValidation 裁决 6：校验面。
func TestV0500TagValidation(t *testing.T) {
	s := New("")
	cases := []struct {
		name       string
		anchorType string
		anchorID   string
		kvs        map[string]*string
		want       error
	}{
		{"坏锚点类型", "pod", "x", map[string]*string{"k": strPtr("v")}, ErrBadAnchorType},
		{"空锚点 ID", AnchorNode, "", map[string]*string{"k": strPtr("v")}, ErrBadAnchorID},
		{"超长锚点 ID", AnchorNode, strings.Repeat("x", MaxAnchorID+1), map[string]*string{"k": strPtr("v")}, ErrBadAnchorID},
		{"坏 key 字符", AnchorNode, "n1", map[string]*string{"a b": strPtr("v")}, ErrBadTagKey},
		{"空 key", AnchorNode, "n1", map[string]*string{"": strPtr("v")}, ErrBadTagKey},
		{"超长 key", AnchorNode, "n1", map[string]*string{strings.Repeat("k", MaxKeyBytes+1): strPtr("v")}, ErrBadTagKey},
		{"超长 value", AnchorNode, "n1", map[string]*string{"k": strPtr(strings.Repeat("v", MaxValBytes+1))}, ErrTagValTooLong},
		{"空批量", AnchorNode, "n1", map[string]*string{}, ErrTooManyKeys},
	}
	for _, tc := range cases {
		if _, err := s.SetTags(tc.anchorType, tc.anchorID, tc.kvs); !errors.Is(err, tc.want) {
			t.Fatalf("%s 应拒绝: got=%v want=%v", tc.name, err, tc.want)
		}
	}
	// 超 32 键
	big := map[string]*string{}
	for i := 0; i < MaxKeysBatch+1; i++ {
		big["k"+strings.Repeat("x", i)] = strPtr("v")
	}
	if _, err := s.SetTags(AnchorNode, "n1", big); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("超批量应拒绝: %v", err)
	}
	// 32 键边界内通过
	ok := map[string]*string{}
	for i := 0; i < MaxKeysBatch; i++ {
		ok["k"+strings.Repeat("x", i)] = strPtr("v")
	}
	if _, err := s.SetTags(AnchorNode, "n1", ok); err != nil {
		t.Fatalf("边界内应通过: %v", err)
	}
}

// TestV0500TagPersistRoundtrip US-6：快照写读重建。
func TestV0500TagPersistRoundtrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tags.json")
	s := New(file)
	if _, err := s.SetTags(AnchorNode, "n1", map[string]*string{"line": strPtr("A1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTags(AnchorModel, "m1", map[string]*string{"ver": strPtr("v2")}); err != nil {
		t.Fatal(err)
	}
	// 删标后落盘（快照应反映删除）
	if _, err := s.SetTags(AnchorNode, "n1", map[string]*string{"line": nil}); err != nil {
		t.Fatal(err)
	}
	s2 := New(file)
	if tags := s2.TagsOf(AnchorNode, "n1"); len(tags) != 0 {
		t.Fatalf("重建后 node 标签应为空: %+v", tags)
	}
	if tags := s2.TagsOf(AnchorModel, "m1"); tags["ver"] != "v2" {
		t.Fatalf("重建后 model 标签不符: %+v", tags)
	}
}

// TestV0500TagConcurrent 并发读写（-race 面）。
func TestV0500TagConcurrent(t *testing.T) {
	s := New("")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.SetTags(AnchorNode, "shared", map[string]*string{"k" + string(rune('a'+i)): strPtr("v")})
			_ = s.TagsOf(AnchorNode, "shared")
			_ = s.AnchorsWithTag(AnchorNode, "", "")
		}(i)
	}
	wg.Wait()
	if got := len(s.TagsOf(AnchorNode, "shared")); got != 8 {
		t.Fatalf("并发打标应全部生效: %d", got)
	}
}
