package main

// 本文件：OpenAPI 生成器类型语义独立对照测试（复核变更集 2 中低项修复）。
//
// 设计原则：
//   - 期望值推导（deriveExpectedStruct / deriveExpectedSchema）为测试内独立实现，
//     不调用生成器的 parseJSONTag / fieldProps / schemaOfType / structSchema 等
//     推导函数，防止"共享逻辑同错"（同一 bug 同时存在于实现与断言而无法发现）；
//   - 第三对照：encoding/json 零值序列化（zeroValueMarshalKeys）。直接以标准库
//     真实行为验证 required 推导与键顺序，与生成器实现完全独立；
//   - 已知偏差（被测试明示允许）：非指针 struct 字段带 omitempty 时
//     encoding/json 恒输出该字段（struct 永不为空），生成器按 omitempty 标为
//     optional（方向安全：不拒绝 API 实际产出）。对照测试要求所有"零值序列化
//     存在但非 required"的键必须命中该偏差，否则视为 required 推导错误。

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fixtureInner 用作匿名内嵌（无 tag → 展开）与命名内联扩展（json:",inline"）的载体。
type fixtureInner struct {
	X string `json:"x"`
	Y int    `json:"y,omitempty"`
}

// fixtureSemantics 覆盖 schemaOfType 全部分支：基本类型、整型各宽度与 format、
// bool、float、time.Time、指针链、切片/嵌套切片、map、interface{}、匿名结构体、
// 无 tag / 空名 tag / "-"、匿名内嵌 struct（展开）与匿名内嵌非 struct
// （encoding/json 用类型名作键，如 "Duration"）。
type fixtureSemantics struct {
	Str     string `json:"str"`
	Opt     string `json:"opt,omitempty"`
	NoTag   string
	Empty   string         `json:",omitempty"`
	Skipped string         `json:"-"`
	Int     int            `json:"int"`
	I8      int8           `json:"i8"`
	I16     int16          `json:"i16"`
	I32     int32          `json:"i32"`
	I64     int64          `json:"i64"`
	U8      uint8          `json:"u8"`
	U16     uint16         `json:"u16"`
	U32     uint32         `json:"u32"`
	U64     uint64         `json:"u64"`
	Uint    uint           `json:"uint"`
	Uptr    uintptr        `json:"uptr"`
	Bool    bool           `json:"bool"`
	F32     float32        `json:"f32"`
	F64     float64        `json:"f64"`
	When    time.Time      `json:"when"`
	PStr    *string        `json:"pstr"`
	PPInt   **int          `json:"ppint"`
	Ints    []int          `json:"ints"`
	Matrix  [][]string     `json:"matrix"`
	Lookup  map[string]int `json:"lookup"`
	Any     interface{}    `json:"any"`
	Anon    struct {
		A string `json:"a"`
		B int    `json:"b,omitempty"`
	} `json:"anon"`
	fixtureInner
	time.Duration
}

// fixtureInlineExt 覆盖生成器对"命名字段 + json:\",inline\"" 的文档化扩展。
// 注意：encoding/json 不支持 inline 选项（会按 Go 字段名 "Inner" 输出），
// 该差异是生成器有意的扩展，因此本用例不做零值序列化强对照。
type fixtureInlineExt struct {
	Inner fixtureInner `json:",inline"`
	Own   string       `json:"own"`
}

// fixtureUnknown 覆盖未知类型（chan）的保守映射；encoding/json 无法序列化 chan，
// 因此本用例不做零值序列化对照。
type fixtureUnknown struct {
	Ch chan int `json:"ch"`
}

// TestSchemaSemanticsMatchReflect 遍历 17 个 Go 类型，对每个导出字段用独立推导
// 断言生成器产物的 type/format/required/items/$ref 语义，并做 encoding/json
// 零值序列化第三对照。
func TestSchemaSemanticsMatchReflect(t *testing.T) {
	if len(goTypes) != 17 {
		t.Fatalf("goTypes 数量 = %d，预期 17（新增/删除类型需同步更新本测试与 goTypes）", len(goTypes))
	}
	set := buildSchemas()

	for name, typ := range goTypes {
		node := set.byName[name]
		if node == nil {
			t.Errorf("schema %q 未生成", name)
			continue
		}
		req, fields := deriveExpectedStruct(t, typ)
		want := &schemaExpect{typ: "object", required: req, properties: fields}
		assertSchemaNodeMatches(t, name, node, want)
		crossCheckEncodingJSON(t, name, typ, node, req)
	}
}

// TestEmptyJSONTagFallsBackToGoFieldName 空 json tag 名（无 tag 或 `json:",omitempty"`）
// 回落 Go 字段名，与 encoding/json 行为一致。修复前生成器产出空字符串属性名。
func TestEmptyJSONTagFallsBackToGoFieldName(t *testing.T) {
	type fixture struct {
		Alpha string
		Beta  string `json:",omitempty"`
		Gamma string `json:"gamma"`
	}
	typ := reflect.TypeOf(fixture{})
	req, fields := deriveExpectedStruct(t, typ)
	if len(fields) != 3 || fields[0].name != "Alpha" || fields[1].name != "Beta" || fields[2].name != "gamma" {
		t.Fatalf("属性名推导错误（应为 Go 字段名回落）: %+v", fields)
	}
	want := &schemaExpect{typ: "object", required: req, properties: fields}
	assertSchemaNodeMatches(t, "fixture", (&schemaSet{}).structSchema(typ), want)

	// 强对照：零值序列化键（encoding/json 真实行为）== required。
	if got := zeroValueMarshalKeys(t, typ); !sameStringSet(got, req) {
		t.Errorf("required = %v，encoding/json 零值序列化键 = %v（应一致）", req, got)
	}
}

// TestTypeMappingAllBranches 用合成类型锁定 schemaOfType 全部分支的映射语义。
func TestTypeMappingAllBranches(t *testing.T) {
	t.Run("fixtureSemantics", func(t *testing.T) {
		typ := reflect.TypeOf(fixtureSemantics{})
		req, fields := deriveExpectedStruct(t, typ)
		want := &schemaExpect{typ: "object", required: req, properties: fields}
		assertSchemaNodeMatches(t, "fixtureSemantics", (&schemaSet{}).structSchema(typ), want)

		// 本 fixture 无 struct+omitempty 字段 → 强对照：required == 零值序列化键。
		if got := zeroValueMarshalKeys(t, typ); !sameStringSet(got, req) {
			t.Errorf("required 与 encoding/json 零值序列化键不一致:\nrequired = %v\nmarshal  = %v", req, got)
		}
	})

	t.Run("inlineExtension", func(t *testing.T) {
		typ := reflect.TypeOf(fixtureInlineExt{})
		req, fields := deriveExpectedStruct(t, typ)
		want := &schemaExpect{typ: "object", required: req, properties: fields}
		assertSchemaNodeMatches(t, "fixtureInlineExt", (&schemaSet{}).structSchema(typ), want)
		if len(fields) != 3 || fields[0].name != "x" || fields[1].name != "y" || fields[2].name != "own" {
			t.Fatalf("命名字段 inline 展开错误: %+v", fields)
		}
		if len(req) != 2 || req[0] != "x" || req[1] != "own" {
			t.Fatalf("命名字段 inline 展开 required 错误: %v", req)
		}
	})

	t.Run("unknownKind", func(t *testing.T) {
		typ := reflect.TypeOf(fixtureUnknown{})
		req, fields := deriveExpectedStruct(t, typ)
		want := &schemaExpect{typ: "object", required: req, properties: fields}
		assertSchemaNodeMatches(t, "fixtureUnknown", (&schemaSet{}).structSchema(typ), want)
	})
}

// ---------------------------------------------------------------------------
// 独立期望推导（不调用生成器内部推导函数，防共享逻辑同错）
// ---------------------------------------------------------------------------

// schemaExpect 是测试自研的独立 schema 期望结构。
type schemaExpect struct {
	typ                  string // object | string | boolean | integer | number | array
	format               string
	ref                  string
	items                *schemaExpect
	additionalProperties *schemaExpect
	required             []string
	properties           []fieldExpect
}

// fieldExpect 描述一个属性字段的预期语义。
type fieldExpect struct {
	name   string
	req    bool
	schema *schemaExpect
}

// deriveExpectedStruct 独立推导结构体的 required 名单与字段语义（含内嵌展开）。
func deriveExpectedStruct(t *testing.T, typ reflect.Type) ([]string, []fieldExpect) {
	t.Helper()
	var required []string
	var fields []fieldExpect
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			// 未导出字段按 encoding/json 可见性规则：非内嵌一律忽略；
			// 内嵌且类型未导出时仅 struct 参与（可能含导出字段），其余忽略。
			pt := f.Type
			for pt.Kind() == reflect.Ptr {
				pt = pt.Elem()
			}
			if !f.Anonymous || pt.Kind() != reflect.Struct {
				continue
			}
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		rawName := parts[0]
		var omitempty, inline bool
		for _, p := range parts[1:] {
			switch p {
			case "omitempty":
				omitempty = true
			case "inline":
				inline = true
			}
		}
		name := rawName
		if name == "" {
			name = f.Name
		}
		et := f.Type
		for et.Kind() == reflect.Ptr {
			et = et.Elem()
		}
		if et.Kind() == reflect.Struct && (inline || (f.Anonymous && rawName == "")) {
			ir, ifs := deriveExpectedStruct(t, et)
			required = append(required, ir...)
			fields = append(fields, ifs...)
			continue
		}
		fields = append(fields, fieldExpect{name: name, req: !omitempty, schema: deriveExpectedSchema(t, f.Type)})
		if !omitempty {
			required = append(required, name)
		}
	}
	return required, fields
}

// deriveExpectedSchema 独立推导字段类型的 OpenAPI 语义。
func deriveExpectedSchema(t *testing.T, typ reflect.Type) *schemaExpect {
	t.Helper()
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(time.Time{}) {
		return &schemaExpect{typ: "string", format: "date-time"}
	}
	switch typ.Kind() {
	case reflect.Struct:
		if typ.Name() != "" {
			return &schemaExpect{ref: typ.Name()}
		}
		req, fields := deriveExpectedStruct(t, typ)
		return &schemaExpect{typ: "object", required: req, properties: fields}
	case reflect.String:
		return &schemaExpect{typ: "string"}
	case reflect.Bool:
		return &schemaExpect{typ: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		e := &schemaExpect{typ: "integer"}
		switch typ.Kind() {
		case reflect.Int32, reflect.Uint32:
			e.format = "int32"
		case reflect.Int64, reflect.Uint64:
			e.format = "int64"
		}
		return e
	case reflect.Float32, reflect.Float64:
		return &schemaExpect{typ: "number"}
	case reflect.Slice, reflect.Array:
		return &schemaExpect{typ: "array", items: deriveExpectedSchema(t, typ.Elem())}
	case reflect.Map:
		return &schemaExpect{typ: "object", additionalProperties: deriveExpectedSchema(t, typ.Elem())}
	case reflect.Interface:
		return &schemaExpect{typ: "object"}
	default:
		return &schemaExpect{typ: "string"}
	}
}

// ---------------------------------------------------------------------------
// 断言辅助：yamlNode ↔ schemaExpect 逐项对照
// ---------------------------------------------------------------------------

func assertSchemaNodeMatches(t *testing.T, path string, n *yamlNode, want *schemaExpect) {
	t.Helper()
	if n == nil {
		t.Errorf("%s: 节点缺失（期望 %s）", path, describeWant(want))
		return
	}
	if want.ref != "" {
		target := "#/components/schemas/" + want.ref
		if len(n.pairs) != 1 || n.pairs[0].key != "$ref" || n.pairs[0].val == nil || n.pairs[0].val.scalar != target {
			t.Errorf("%s: 期望 $ref %q，实际 %s", path, target, nodeBrief(n))
		}
		return
	}
	if got := mapValue(n, "type"); got == nil || got.scalar != want.typ {
		t.Errorf("%s: type = %v，期望 %q", path, got, want.typ)
	}
	wantKeys := map[string]bool{"type": true}
	if want.format != "" {
		wantKeys["format"] = true
		if got := mapValue(n, "format"); got == nil || got.scalar != want.format {
			t.Errorf("%s: format = %v，期望 %q", path, got, want.format)
		}
	} else if got := mapValue(n, "format"); got != nil {
		t.Errorf("%s: 多余 format %q", path, got.scalar)
	}
	switch want.typ {
	case "array":
		wantKeys["items"] = true
		assertSchemaNodeMatches(t, path+".items", mapValue(n, "items"), want.items)
	case "object":
		if want.additionalProperties != nil {
			wantKeys["additionalProperties"] = true
			assertSchemaNodeMatches(t, path+".additionalProperties", mapValue(n, "additionalProperties"), want.additionalProperties)
		}
		if want.properties != nil {
			wantKeys["properties"] = true
			if len(want.required) > 0 {
				wantKeys["required"] = true
				assertRequiredMatches(t, path, n, want.required)
			} else if got := mapValue(n, "required"); got != nil {
				t.Errorf("%s: 多余 required %v", path, nodeBrief(got))
			}
			assertPropertiesMatch(t, path, mapValue(n, "properties"), want.properties)
		}
	}
	for _, p := range n.pairs {
		if !wantKeys[p.key] {
			t.Errorf("%s: 多余键 %q（期望键 %v）", path, p.key, keysOf(wantKeys))
		}
	}
}

// assertRequiredMatches 按序对照 required 名单（生成器与推导均按字段声明序）。
func assertRequiredMatches(t *testing.T, path string, n *yamlNode, want []string) {
	t.Helper()
	got := mapValue(n, "required")
	if got == nil || got.kind != kindSeq || len(got.items) != len(want) {
		t.Errorf("%s: required = %s，期望 %v", path, nodeBrief(got), want)
		return
	}
	for i, it := range got.items {
		if it.kind != kindScalar || it.scalar != want[i] {
			t.Errorf("%s: required[%d] = %v，期望 %q（完整期望 %v）", path, i, nodeBrief(it), want[i], want)
			return
		}
	}
}

// assertPropertiesMatch 按序对照 properties 键与每个字段的 schema。
func assertPropertiesMatch(t *testing.T, path string, props *yamlNode, want []fieldExpect) {
	t.Helper()
	if props == nil || props.kind != kindMap {
		t.Errorf("%s: properties 缺失或非 map", path)
		return
	}
	if len(props.pairs) != len(want) {
		t.Errorf("%s: properties 数量 = %d，期望 %d（实际键 %v）", path, len(props.pairs), len(want), propertyKeysOf(props))
	}
	n := len(props.pairs)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if props.pairs[i].key != want[i].name {
			t.Errorf("%s.properties[%d]: 键 = %q，期望 %q", path, i, props.pairs[i].key, want[i].name)
		}
		assertSchemaNodeMatches(t, path+"."+want[i].name, props.pairs[i].val, want[i].schema)
	}
}

// crossCheckEncodingJSON 用 encoding/json 零值序列化对照 required 与键顺序。
func crossCheckEncodingJSON(t *testing.T, name string, typ reflect.Type, node *yamlNode, wantRequired []string) {
	t.Helper()
	mkeys := zeroValueMarshalKeys(t, typ)
	mkeySet := toSet(mkeys)
	reqSet := toSet(wantRequired)

	for _, r := range wantRequired {
		if !mkeySet[r] {
			t.Errorf("schema %s: required 字段 %q 不出现在 encoding/json 零值序列化输出中（required 推导过宽）", name, r)
		}
	}
	for _, k := range mkeys {
		if reqSet[k] {
			continue
		}
		// 非 required 但出现在序列化输出：仅允许已知偏差（非指针 struct + omitempty）。
		sf, ok := fieldForJSONName(typ, k)
		if !ok {
			t.Errorf("schema %s: 零值序列化键 %q 找不到对应 Go 字段", name, k)
			continue
		}
		et := sf.Type
		for et.Kind() == reflect.Ptr {
			et = et.Elem()
		}
		if et.Kind() != reflect.Struct || !hasJSONOption(sf, "omitempty") {
			t.Errorf("schema %s: 零值序列化键 %q 非 required 且非 struct+omitempty 已知偏差（疑似 required 推导错误）", name, k)
		}
	}
	// 键顺序：零值序列化键必须是 properties 键序列的子序列（内联展开位置一致，
	// omitempty 字段缺失；struct+omitempty 字段位置不变）。
	propKeys := schemaPropertyKeys(node)
	j := 0
	for _, k := range mkeys {
		for j < len(propKeys) && propKeys[j] != k {
			j++
		}
		if j >= len(propKeys) {
			t.Errorf("schema %s: 零值序列化键 %q 未按序出现在 properties（%v）中", name, k, propKeys)
			break
		}
		j++
	}
}

// ---------------------------------------------------------------------------
// encoding/json 第三对照与工具函数
// ---------------------------------------------------------------------------

// zeroValueMarshalKeys 用 encoding/json 序列化类型零值，返回输出键集合（按出现顺序）。
func zeroValueMarshalKeys(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	b, err := json.Marshal(reflect.New(typ).Interface())
	if err != nil {
		t.Fatalf("json.Marshal(%s 零值): %v", typ, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("解析 %s 零值序列化: %v", typ, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("%s 零值序列化不是 JSON 对象: %v", typ, tok)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("解析 %s 零值序列化键: %v", typ, err)
		}
		keys = append(keys, k.(string))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatalf("解析 %s 零值序列化值: %v", typ, err)
		}
	}
	return keys
}

// fieldForJSONName 沿字段（含内嵌展开）查找 json 名对应的 Go 字段。
func fieldForJSONName(t reflect.Type, key string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			// 与 fieldProps 相同的可见性规则：仅未导出类型的内嵌 struct 参与展开。
			pt := f.Type
			for pt.Kind() == reflect.Ptr {
				pt = pt.Elem()
			}
			if !f.Anonymous || pt.Kind() != reflect.Struct {
				continue
			}
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		rawName := parts[0]
		inline := false
		for _, p := range parts[1:] {
			if p == "inline" {
				inline = true
			}
		}
		et := f.Type
		for et.Kind() == reflect.Ptr {
			et = et.Elem()
		}
		if et.Kind() == reflect.Struct && (inline || (f.Anonymous && rawName == "")) {
			if sf, ok := fieldForJSONName(et, key); ok {
				return sf, true
			}
			continue
		}
		name := rawName
		if name == "" {
			name = f.Name
		}
		if name == key {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

func hasJSONOption(f reflect.StructField, opt string) bool {
	for _, p := range strings.Split(f.Tag.Get("json"), ",")[1:] {
		if p == opt {
			return true
		}
	}
	return false
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	mb := toSet(b)
	for _, s := range a {
		if !mb[s] {
			return false
		}
	}
	return true
}

func keysOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func propertyKeysOf(n *yamlNode) []string {
	if n == nil {
		return nil
	}
	var keys []string
	for _, p := range n.pairs {
		keys = append(keys, p.key)
	}
	return keys
}

func nodeBrief(n *yamlNode) string {
	if n == nil {
		return "<nil>"
	}
	switch n.kind {
	case kindScalar:
		return n.scalar
	case kindMap:
		var parts []string
		for _, p := range n.pairs {
			parts = append(parts, p.key+":"+nodeBrief(p.val))
		}
		return "{" + strings.Join(parts, " ") + "}"
	case kindSeq:
		var parts []string
		for _, it := range n.items {
			parts = append(parts, nodeBrief(it))
		}
		return "[" + strings.Join(parts, " ") + "]"
	}
	return "?"
}

func describeWant(w *schemaExpect) string {
	if w.ref != "" {
		return "$ref " + w.ref
	}
	return w.typ
}
