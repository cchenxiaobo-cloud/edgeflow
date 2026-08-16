// Command openapi-gen 从 apis/edge/v1alpha1 包类型生成 OpenAPI v3 schema 文档。
//
// 用法：
//
//	go run ./hack/openapi-gen -out docs/openapi/edgeflow-openapi.yaml
//
// 设计约束：
//
//   - 零第三方依赖：仅 Go 标准库（reflect），不引入任何 yaml 库；
//   - 确定性输出：相同输入必然产生相同字节输出（无 map 迭代、无时间戳）；
//   - json tag 是唯一契约：属性名取自 json tag，required 由 omitempty 推导，
//     内联字段（json:",inline"）展开为外层属性（与 encoding/json 语义一致）。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"edgeflow/apis/edge/v1alpha1"
)

// roots 是 OpenAPI schema 生成的根类型（EdgeNode / DeviceModel / Device 三组 CRD 资源）。
// 嵌套类型（Spec / Status / 共享元数据类型等）由 discover 沿字段引用自动补齐。
var roots = []reflect.Type{
	reflect.TypeOf(v1alpha1.EdgeNode{}),
	reflect.TypeOf(v1alpha1.DeviceModel{}),
	reflect.TypeOf(v1alpha1.Device{}),
}

// resourceOrder 控制 components.schemas 中根类型（CRD 资源）的排列顺序，
// 其余嵌套类型按发现顺序排列；顺序固定，保证输出确定性。
var resourceOrder = []string{"EdgeNode", "DeviceModel", "Device"}

var timeType = reflect.TypeOf(time.Time{})

const defaultOut = "docs/openapi/edgeflow-openapi.yaml"

func main() {
	out := flag.String("out", defaultOut, "output YAML file path")
	flag.Parse()

	set := buildSchemas()
	doc := renderDoc(set)
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "openapi-gen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "openapi-gen:", err)
		os.Exit(1)
	}
	fmt.Printf("openapi-gen: wrote %s (%d bytes, %d schemas)\n", *out, len(doc), len(set.order))
}

// generate 构建完整 OpenAPI 文档文本（含头部注释），是测试与产物共用的唯一入口。
func generate() string {
	return renderDoc(buildSchemas())
}

// renderDoc 将 schema 集合渲染为完整 OpenAPI 文档文本（含头部注释）。
func renderDoc(set *schemaSet) string {
	var schemas []yamlPair
	for _, name := range resourceOrder {
		schemas = append(schemas, yamlPair{name, set.byName[name]})
	}
	for _, name := range set.order {
		if isResource(name) {
			continue
		}
		schemas = append(schemas, yamlPair{name, set.byName[name]})
	}

	paths := mapNode(
		pm("/api/v1/{resource}", mapNode(
			pm("get", mapNode(
				pair("summary", "占位路径：EdgeFlow REST 端点定义见 docs/API-SPEC.md"),
				pair("description", "本 OpenAPI 文档仅承载 CRD 资源 schema（components.schemas）；REST API 端点清单、路径参数与响应定义见 docs/API-SPEC.md。"),
				pm("responses", mapNode(
					pm("200", mapNode(
						pair("description", "占位响应：端点定义见 docs/API-SPEC.md"),
					)),
				)),
			)),
		)),
	)

	doc := mapNode(
		pair("openapi", "3.0.3"),
		pm("info", mapNode(
			pair("title", "EdgeFlow API"),
			pair("version", v1alpha1.Version),
			pair("description", "EdgeFlow 边缘计算平台 CRD 资源（edgeflow.io/v1alpha1）OpenAPI v3 schema。REST 端点定义见 docs/API-SPEC.md。"),
		)),
		pm("paths", paths),
		pm("components", mapNode(pm("schemas", mapNode(schemas...)))),
	)

	var sb strings.Builder
	sb.WriteString(headerComment)
	sb.WriteString(render(doc))
	sb.WriteString("\n")
	return sb.String()
}

func isResource(name string) bool {
	for _, r := range resourceOrder {
		if name == r {
			return true
		}
	}
	return false
}

const headerComment = `# 本文件由 hack/openapi-gen 自动生成，请勿手工编辑。
# 重新生成：bash hack/gen-openapi.sh
# 覆盖：apis/edge/v1alpha1 包导出的 CRD 资源类型（EdgeNode / DeviceModel / Device）及其嵌套类型。
# REST 端点定义见 docs/API-SPEC.md。

`

// ---------------------------------------------------------------------------
// schemaSet：类型发现 + schema 构建
// ---------------------------------------------------------------------------

type schemaSet struct {
	order  []string
	byName map[string]*yamlNode
	byType map[string]reflect.Type
}

func buildSchemas() *schemaSet {
	s := &schemaSet{
		byName: make(map[string]*yamlNode),
		byType: make(map[string]reflect.Type),
	}
	for _, rt := range roots {
		s.discover(rt)
	}
	for _, name := range s.order {
		s.byName[name] = s.structSchema(s.byType[name])
	}
	return s
}

// discover 沿字段引用递归收集命名结构体类型（去掉指针/切片/映射包装）。
func (s *schemaSet) discover(t reflect.Type) {
	for {
		switch t.Kind() {
		case reflect.Ptr, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
			continue
		}
		break
	}
	if t.Kind() != reflect.Struct || t == timeType {
		return
	}
	if t.Name() == "" { // 匿名结构体：不注册为独立 schema，仅继续发现嵌套类型
		s.discoverFields(t)
		return
	}
	if _, ok := s.byType[t.Name()]; ok {
		return
	}
	s.order = append(s.order, t.Name())
	s.byType[t.Name()] = t
	s.discoverFields(t)
}

func (s *schemaSet) discoverFields(t reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
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
		s.discover(f.Type)
	}
}

// structSchema 为结构体类型构建 {type: object, required?, properties} 节点。
func (s *schemaSet) structSchema(t reflect.Type) *yamlNode {
	props, req := s.fieldProps(t)
	pairs := []yamlPair{pair("type", "object")}
	if len(req) > 0 {
		var reqNodes []*yamlNode
		for _, name := range req {
			reqNodes = append(reqNodes, scalar(name))
		}
		pairs = append(pairs, yamlPair{"required", &yamlNode{kind: kindSeq, items: reqNodes}})
	}
	pairs = append(pairs, yamlPair{"properties", &yamlNode{kind: kindMap, pairs: props}})
	return &yamlNode{kind: kindMap, pairs: pairs}
}

// fieldProps 提取字段属性对与 required 名单；json:",inline"（或未命名内嵌）
// 字段展开为外层属性，与 encoding/json 语义一致。
func (s *schemaSet) fieldProps(t reflect.Type) (props []yamlPair, required []string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
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
		name, opts := parseJSONTag(tag, f.Name)
		et := f.Type
		for et.Kind() == reflect.Ptr {
			et = et.Elem()
		}
		// 内嵌展开判定基于原始 tag 名（无 tag 或空 tag 名的匿名 struct 字段），
		// 与 encoding/json 语义一致；parseJSONTag 的字段名回落在判定之后生效。
		// 命名字段 + json:",inline" 是生成器扩展（encoding/json 不支持该选项）。
		if et.Kind() == reflect.Struct && (opts.inline || (f.Anonymous && tagName(tag) == "")) {
			ip, ir := s.fieldProps(et)
			props = append(props, ip...)
			required = append(required, ir...)
			continue
		}
		props = append(props, yamlPair{name, s.schemaOfType(f.Type)})
		if !opts.omitempty {
			required = append(required, name)
		}
	}
	return props, required
}

// schemaOfType 将 Go 类型映射为 OpenAPI schema 节点。
func (s *schemaSet) schemaOfType(t reflect.Type) *yamlNode {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == timeType {
		return mapNode(pair("type", "string"), pair("format", "date-time"))
	}
	switch t.Kind() {
	case reflect.Struct:
		if t.Name() != "" {
			return mapNode(pair("$ref", "#/components/schemas/"+t.Name()))
		}
		return s.structSchema(t) // 匿名结构体：内联 object
	case reflect.String:
		return mapNode(pair("type", "string"))
	case reflect.Bool:
		return mapNode(pair("type", "boolean"))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		switch t.Kind() {
		case reflect.Int32, reflect.Uint32:
			return mapNode(pair("type", "integer"), pair("format", "int32"))
		case reflect.Int64, reflect.Uint64:
			return mapNode(pair("type", "integer"), pair("format", "int64"))
		default:
			return mapNode(pair("type", "integer"))
		}
	case reflect.Float32, reflect.Float64:
		return mapNode(pair("type", "number"))
	case reflect.Slice, reflect.Array:
		return mapNode(pair("type", "array"), pm("items", s.schemaOfType(t.Elem())))
	case reflect.Map:
		return mapNode(pair("type", "object"), pm("additionalProperties", s.schemaOfType(t.Elem())))
	case reflect.Interface:
		return mapNode(pair("type", "object"))
	default:
		// 未知类型（chan/func/unsafe 等）：保守映射为 string，避免生成失败。
		return mapNode(pair("type", "string"))
	}
}

type tagOptions struct {
	omitempty bool
	inline    bool
}

func parseJSONTag(tag string, goName string) (string, tagOptions) {
	var opts tagOptions
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "" {
		name = goName
	}
	for _, p := range parts[1:] {
		switch p {
		case "omitempty":
			opts.omitempty = true
		case "inline":
			opts.inline = true
		}
	}
	return name, opts
}

// tagName 返回 json tag 的原始名称部分（不做字段名回落），供内嵌展开判定使用。
func tagName(tag string) string {
	return strings.Split(tag, ",")[0]
}

// ---------------------------------------------------------------------------
// 确定性 YAML 序列化（仅覆盖本工具使用的子集，无第三方依赖）
// ---------------------------------------------------------------------------

type nodeKind int

const (
	kindScalar nodeKind = iota
	kindMap
	kindSeq
)

type yamlNode struct {
	kind   nodeKind
	scalar string
	pairs  []yamlPair  // kindMap：有序键值对
	items  []*yamlNode // kindSeq：有序元素
}

type yamlPair struct {
	key string
	val *yamlNode
}

func scalar(s string) *yamlNode { return &yamlNode{kind: kindScalar, scalar: s} }
func mapNode(pairs ...yamlPair) *yamlNode {
	return &yamlNode{kind: kindMap, pairs: pairs}
}

func pair(key, value string) yamlPair {
	return yamlPair{key: key, val: scalar(value)}
}

// pm 构造一个键值对（值可为任意节点），用于 mapNode 的参数列表。
func pm(key string, val *yamlNode) yamlPair {
	return yamlPair{key: key, val: val}
}

// render 输出 YAML 文本（两级缩进；键与值按同一引号规则渲染）。
func render(n *yamlNode) string {
	var sb strings.Builder
	writeNode(&sb, n, 0)
	return sb.String()
}

func writeNode(sb *strings.Builder, n *yamlNode, indent int) {
	switch n.kind {
	case kindScalar:
		sb.WriteString(strings.Repeat(" ", indent))
		sb.WriteString(renderScalar(n.scalar))
	case kindMap:
		for i, p := range n.pairs {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(strings.Repeat(" ", indent))
			sb.WriteString(renderScalar(p.key))
			sb.WriteString(":")
			writeMapValue(sb, p.val, indent)
		}
	case kindSeq:
		for i, item := range n.items {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(strings.Repeat(" ", indent))
			sb.WriteString("-")
			writeSeqItem(sb, item, indent)
		}
	}
}

func writeMapValue(sb *strings.Builder, v *yamlNode, indent int) {
	switch v.kind {
	case kindScalar:
		sb.WriteString(" ")
		sb.WriteString(renderScalar(v.scalar))
	case kindMap:
		if len(v.pairs) == 0 {
			sb.WriteString(" {}")
			return
		}
		sb.WriteString("\n")
		writeNode(sb, v, indent+2)
	case kindSeq:
		if len(v.items) == 0 {
			sb.WriteString(" []")
			return
		}
		sb.WriteString("\n")
		writeNode(sb, v, indent+2)
	}
}

func writeSeqItem(sb *strings.Builder, v *yamlNode, indent int) {
	switch v.kind {
	case kindScalar:
		sb.WriteString(" ")
		sb.WriteString(renderScalar(v.scalar))
	case kindMap:
		if len(v.pairs) == 0 {
			sb.WriteString(" {}")
			return
		}
		sb.WriteString(" ")
		first := v.pairs[0]
		sb.WriteString(renderScalar(first.key))
		sb.WriteString(":")
		writeMapValue(sb, first.val, indent+2)
		for _, p := range v.pairs[1:] {
			sb.WriteString("\n")
			sb.WriteString(strings.Repeat(" ", indent+2))
			sb.WriteString(renderScalar(p.key))
			sb.WriteString(":")
			writeMapValue(sb, p.val, indent+2)
		}
	case kindSeq:
		if len(v.items) == 0 {
			sb.WriteString(" []")
			return
		}
		sb.WriteString("\n")
		writeNode(sb, v, indent+2)
	}
}

// renderScalar 按保守规则决定是否加单引号：
// 含 YAML 指示符（# : { } [ ] , & * ! | > ' " % @ ` \）、首尾空白、
// 数字/布尔外观、空串等一律加引号，避免解析歧义。
func renderScalar(s string) string {
	if needsQuote(s) {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return s
}

func needsQuote(s string) bool {
	if s == "" || s != strings.TrimSpace(s) {
		return true
	}
	if isNumLike(s) || isBoolLike(s) {
		return true
	}
	for _, r := range s {
		switch r {
		case '#', ':', '{', '}', '[', ']', ',', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`', '\\':
			return true
		}
	}
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "? ") || strings.HasPrefix(s, ": ") {
		return true
	}
	return false
}

func isNumLike(s string) bool {
	if s == "" {
		return false
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return s[0] >= '0' && s[0] <= '9'
}

func isBoolLike(s string) bool {
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return true
	}
	return false
}
