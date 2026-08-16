package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"edgeflow/apis/edge/v1alpha1"
)

// goTypes 是生成器应覆盖的全部 schema 类型（roots + 嵌套类型闭包）。
// 测试 ② 用它做 reflect 对照。
var goTypes = map[string]reflect.Type{
	"EdgeNode":           reflect.TypeOf(v1alpha1.EdgeNode{}),
	"EdgeNodeSpec":       reflect.TypeOf(v1alpha1.EdgeNodeSpec{}),
	"EdgeNodeStatus":     reflect.TypeOf(v1alpha1.EdgeNodeStatus{}),
	"NodeAddress":        reflect.TypeOf(v1alpha1.NodeAddress{}),
	"NodeCondition":      reflect.TypeOf(v1alpha1.NodeCondition{}),
	"DeviceModel":        reflect.TypeOf(v1alpha1.DeviceModel{}),
	"DeviceModelSpec":    reflect.TypeOf(v1alpha1.DeviceModelSpec{}),
	"DeviceProperty":     reflect.TypeOf(v1alpha1.DeviceProperty{}),
	"Device":             reflect.TypeOf(v1alpha1.Device{}),
	"DeviceSpec":         reflect.TypeOf(v1alpha1.DeviceSpec{}),
	"DeviceStatus":       reflect.TypeOf(v1alpha1.DeviceStatus{}),
	"ProtocolConfig":     reflect.TypeOf(v1alpha1.ProtocolConfig{}),
	"DevicePropertySpec": reflect.TypeOf(v1alpha1.DevicePropertySpec{}),
	"PropertyValue":      reflect.TypeOf(v1alpha1.PropertyValue{}),
	"TwinProperty":       reflect.TypeOf(v1alpha1.TwinProperty{}),
	"TypeMeta":           reflect.TypeOf(v1alpha1.TypeMeta{}),
	"ObjectMeta":         reflect.TypeOf(v1alpha1.ObjectMeta{}),
}

// TestDeterministicOutput ① 生成器输出确定性：多次生成结果逐字节一致。
func TestDeterministicOutput(t *testing.T) {
	var outputs []string
	for i := 0; i < 3; i++ {
		outputs = append(outputs, generate())
	}
	for i := 1; i < len(outputs); i++ {
		if outputs[i] != outputs[0] {
			t.Fatalf("generator output is not deterministic: run %d differs from run 0", i)
		}
	}

	// 落盘对比（两次写入文件，字节级 diff 为空）。
	dir := t.TempDir()
	p1 := filepath.Join(dir, "run1.yaml")
	p2 := filepath.Join(dir, "run2.yaml")
	if err := os.WriteFile(p1, []byte(outputs[0]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte(outputs[1]), 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("two generated files differ (byte diff not empty)")
	}
}

// TestSchemaPropertiesMatchGoJSONTags ② schema 的 properties 键集合
// 与 Go 字段 json tag 集合一致（reflect 对照，含 inline 展开规则）。
func TestSchemaPropertiesMatchGoJSONTags(t *testing.T) {
	set := buildSchemas()

	// 覆盖完整性：schema 集合与 goTypes 集合互为一一对应。
	if len(set.order) != len(goTypes) {
		t.Fatalf("schema count = %d, expected %d; schemas: %v", len(set.order), len(goTypes), set.order)
	}
	for _, name := range set.order {
		if _, ok := goTypes[name]; !ok {
			t.Errorf("schema %q has no corresponding Go type", name)
		}
		if set.byName[name] == nil {
			t.Errorf("schema %q not built", name)
		}
	}
	for name := range goTypes {
		if set.byType[name] == nil {
			t.Errorf("Go type %q missing from generated schemas", name)
		}
	}

	for name, typ := range goTypes {
		node := set.byName[name]
		// 每个 schema 顶层必须是 type: object。
		typVal := mapValue(node, "type")
		if typVal == nil || typVal.scalar != "object" {
			t.Errorf("schema %s: top-level type = %v, want object", name, typVal)
		}

		got := schemaPropertyKeys(node)
		want := expectedPropertyKeys(t, typ)
		if len(got) != len(want) {
			t.Errorf("schema %s: properties keys = %v, json tag keys = %v", name, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("schema %s: properties keys = %v, json tag keys = %v", name, got, want)
				break
			}
		}
		// 键集合无重复。
		seen := map[string]bool{}
		for _, k := range got {
			if seen[k] {
				t.Errorf("schema %s: duplicate property key %q", name, k)
			}
			seen[k] = true
		}
	}

	// $ref 完整性：所有引用目标都存在于 schemas 中。
	var refs []string
	collectRefs(set.byName["EdgeNode"], &refs)
	collectRefs(set.byName["DeviceModel"], &refs)
	collectRefs(set.byName["Device"], &refs)
	for _, r := range refs {
		target := strings.TrimPrefix(r, "#/components/schemas/")
		if _, ok := set.byName[target]; !ok {
			t.Errorf("dangling $ref %q: target schema %q not generated", r, target)
		}
	}
}

// TestArtifactUpToDate ③ docs/openapi/edgeflow-openapi.yaml 与最新生成输出一致，
// 防止产物漂移。
func TestArtifactUpToDate(t *testing.T) {
	got := generate()
	path := filepath.Join("..", "..", "docs", "openapi", "edgeflow-openapi.yaml")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(want) != got {
		t.Fatalf("docs/openapi/edgeflow-openapi.yaml 与最新生成输出不一致（产物漂移）。请运行: bash hack/gen-openapi.sh")
	}
}

// ---------------------------------------------------------------------------
// 测试辅助（独立的 reflect 对照实现，避免与被测代码共享逻辑）
// ---------------------------------------------------------------------------

// expectedPropertyKeys 独立地从 Go 结构体 json tag 推导 properties 键
// （含 inline 展开），与生成器行为对照。
func expectedPropertyKeys(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		isInline := false
		for _, p := range parts[1:] {
			if p == "inline" {
				isInline = true
			}
		}
		et := f.Type
		for et.Kind() == reflect.Ptr {
			et = et.Elem()
		}
		if isInline || (f.Anonymous && name == "") {
			if et.Kind() == reflect.Struct {
				out = append(out, expectedPropertyKeys(t, et)...)
			}
			continue
		}
		out = append(out, name)
	}
	return out
}

// schemaPropertyKeys 从生成器的 schema 节点中提取 properties 键（保持顺序）。
func schemaPropertyKeys(n *yamlNode) []string {
	props := mapValue(n, "properties")
	if props == nil {
		return nil
	}
	var keys []string
	for _, p := range props.pairs {
		keys = append(keys, p.key)
	}
	return keys
}

func mapValue(n *yamlNode, key string) *yamlNode {
	if n == nil || n.kind != kindMap {
		return nil
	}
	for _, p := range n.pairs {
		if p.key == key {
			return p.val
		}
	}
	return nil
}

func collectRefs(n *yamlNode, out *[]string) {
	if n == nil {
		return
	}
	switch n.kind {
	case kindMap:
		for _, p := range n.pairs {
			if p.key == "$ref" && p.val != nil && p.val.kind == kindScalar {
				*out = append(*out, p.val.scalar)
			}
			collectRefs(p.val, out)
		}
	case kindSeq:
		for _, item := range n.items {
			collectRefs(item, out)
		}
	}
}
