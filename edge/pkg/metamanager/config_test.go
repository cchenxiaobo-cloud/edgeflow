package metamanager

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// configJSON 构造一条符合 ConfigSync 契约的配置 JSON 字符串（默认命名空间 default）。
func configJSON(name, kind string, data map[string]string) string {
	cfg, _ := json.Marshal(Config{
		Name:      name,
		Namespace: "default",
		Kind:      kind,
		Data:      data,
	})
	return string(cfg)
}

// configJSONNS 构造指定命名空间的配置 JSON 字符串。
func configJSONNS(namespace, name, kind string, data map[string]string) string {
	cfg, _ := json.Marshal(Config{
		Name:      name,
		Namespace: namespace,
		Kind:      kind,
		Data:      data,
	})
	return string(cfg)
}

// TestConfigLifecycle 覆盖 SaveConfig/ListConfigs/DeleteConfig 全流程（WBS 6.2）：
// 保存 → 列出 → 覆盖更新 → 删除；非法输入报错。
func TestConfigLifecycle(t *testing.T) {
	s := newTestStore(t)

	// 初始为空
	configs, err := s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("初始 ListConfigs 长度 = %d，期望 0", len(configs))
	}

	// 保存一条 ConfigMap 与一条 Secret（Secret 的 value 按 base64 编码语义存储）
	appConfig := configJSON("app-config", "ConfigMap", map[string]string{"key1": "value1"})
	secret := configJSON("app-secret", "Secret", map[string]string{"password": "cGFzc3dvcmQ="})
	if err := s.SaveConfig(appConfig); err != nil {
		t.Fatalf("SaveConfig(app-config) 失败: %v", err)
	}
	if err := s.SaveConfig(secret); err != nil {
		t.Fatalf("SaveConfig(app-secret) 失败: %v", err)
	}

	configs, err = s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("ListConfigs 长度 = %d，期望 2", len(configs))
	}
	if configs[0] != appConfig || configs[1] != secret {
		t.Errorf("ListConfigs 内容/顺序异常: %v", configs)
	}
	// value 是配置的 JSON 原样保存（不裁剪、不改写）
	if !strings.Contains(configs[0], `"kind":"ConfigMap"`) || !strings.Contains(configs[0], `"key1":"value1"`) {
		t.Errorf("Config JSON 未原样保存: %s", configs[0])
	}

	// 同 name 覆盖保存（云端重复下发同配置）：不新增记录
	if err := s.SaveConfig(configJSON("app-config", "ConfigMap", map[string]string{"key1": "value2"})); err != nil {
		t.Fatalf("SaveConfig(app-config 覆盖) 失败: %v", err)
	}
	configs, err = s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("覆盖后 ListConfigs 长度 = %d，期望 2（同 name 覆盖不新增）", len(configs))
	}
	if !strings.Contains(configs[0], `"key1":"value2"`) {
		t.Errorf("覆盖后内容不是最新记录: %s", configs[0])
	}

	// 删除（按命名空间+名称，与 SaveConfig 的 key 派生规则一致）
	if err := s.DeleteConfig("default", "app-config"); err != nil {
		t.Fatalf("DeleteConfig(default/app-config) 失败: %v", err)
	}
	configs, err = s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 1 || configs[0] != secret {
		t.Errorf("删除后 ListConfigs = %v，期望只剩 app-secret", configs)
	}
	// 删除不存在的配置：幂等不报错
	if err := s.DeleteConfig("default", "app-config"); err != nil {
		t.Errorf("DeleteConfig(不存在) 期望幂等成功，实际: %v", err)
	}
}

// TestSaveConfigErrors 验证非法输入报错：非 JSON、缺 name 字段、空 name 删除。
func TestSaveConfigErrors(t *testing.T) {
	s := newTestStore(t)

	if err := s.SaveConfig("not-json{"); err == nil {
		t.Error("SaveConfig(非法 JSON) 期望报错，实际成功")
	}
	if err := s.SaveConfig(`{"namespace":"default","kind":"ConfigMap","data":{"k":"v"}}`); err == nil {
		t.Error("SaveConfig(缺 name) 期望报错，实际成功")
	}
	if err := s.SaveConfig(""); err == nil {
		t.Error("SaveConfig(空串) 期望报错，实际成功")
	}
	if err := s.DeleteConfig("default", ""); err == nil {
		t.Error("DeleteConfig(空 name) 期望报错，实际成功")
	}
	// 出错时不应留下脏数据
	configs, err := s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("出错后 ListConfigs 长度 = %d，期望 0（不落脏数据）", len(configs))
	}
}

// TestConfigPersistenceAcrossReopen 是 WBS 6.2 验收核心用例：
// 保存配置 → 关闭 Store → 重新 Open → 配置元数据仍在（模拟 edgecore 重启，
// 与节点信息、Pod 元数据同库共存）。
func TestConfigPersistenceAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persist", "edgeflow.db")

	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("第一次 Open 失败: %v", err)
	}
	appConfig := configJSON("app-config", "ConfigMap", map[string]string{"key1": "value1"})
	if err := s1.SaveConfig(appConfig); err != nil {
		t.Fatalf("SaveConfig 失败: %v", err)
	}
	// 同库共存：Pod 与节点信息不受影响
	if err := s1.SavePod(podJSON("nginx", "nginx:1.25", 1)); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("第一次 Close 失败: %v", err)
	}

	// 模拟 edgecore 重启：重新 Open 同一路径
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("重新 Open 失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	configs, err := s2.ListConfigs()
	if err != nil {
		t.Fatalf("重启后 ListConfigs 失败: %v", err)
	}
	if len(configs) != 1 || configs[0] != appConfig {
		t.Fatalf("重启后 ListConfigs = %v，期望 [%s]（配置元数据持久化）", configs, appConfig)
	}
	// 与 Pod 元数据互不干扰
	pods, err := s2.ListPods()
	if err != nil {
		t.Fatalf("重启后 ListPods 失败: %v", err)
	}
	if len(pods) != 1 {
		t.Errorf("重启后 ListPods 长度 = %d，期望 1", len(pods))
	}
}

// TestConfigKeyNamespaceIsolation 验证：配置 key 必须含 namespace，
// 多命名空间同名配置互不覆盖、delete 不误删他命名空间记录（仿 Pod P1-3 用例）。
func TestConfigKeyNamespaceIsolation(t *testing.T) {
	s := newTestStore(t)

	cfgDefault := configJSON("app-config", "ConfigMap", map[string]string{"k": "v"})       // ns=default
	cfgProd := configJSONNS("prod", "app-config", "ConfigMap", map[string]string{"k": "v"}) // ns=prod，同名不同 ns

	// 同名配置存入两个命名空间：应各存各的，互不覆盖
	if err := s.SaveConfig(cfgDefault); err != nil {
		t.Fatalf("SaveConfig(default/app-config) 失败: %v", err)
	}
	if err := s.SaveConfig(cfgProd); err != nil {
		t.Fatalf("SaveConfig(prod/app-config) 失败: %v", err)
	}
	configs, err := s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("两个命名空间同名配置应共存（ListConfigs 长度 = %d，期望 2）", len(configs))
	}

	// 底层 key 应为 configs/<namespace>/<name>（值按 key 升序：default < prod）
	if v, ok, err := s.Get("configs/default/app-config"); err != nil || !ok || v != cfgDefault {
		t.Errorf("configs/default/app-config 未按预期落盘（ok=%v, err=%v）", ok, err)
	}
	if v, ok, err := s.Get("configs/prod/app-config"); err != nil || !ok || v != cfgProd {
		t.Errorf("configs/prod/app-config 未按预期落盘（ok=%v, err=%v）", ok, err)
	}

	// 删除 default 命名空间下的 app-config：prod 下的应保留
	if err := s.DeleteConfig("default", "app-config"); err != nil {
		t.Fatalf("DeleteConfig(default/app-config) 失败: %v", err)
	}
	configs, err = s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 1 || configs[0] != cfgProd {
		t.Errorf("删除 default/app-config 后 ListConfigs = %v，期望只剩 prod/app-config", configs)
	}

	// 再删 prod 命名空间下的 app-config：清空
	if err := s.DeleteConfig("prod", "app-config"); err != nil {
		t.Fatalf("DeleteConfig(prod/app-config) 失败: %v", err)
	}
	configs, err = s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("删除 prod/app-config 后 ListConfigs 长度 = %d，期望 0", len(configs))
	}
}

// TestConfigKeyNamespaceDefault 验证 namespace 缺省填 "default"：
// 不带 namespace 的配置与显式 default 命名空间的配置使用同一 key。
func TestConfigKeyNamespaceDefault(t *testing.T) {
	s := newTestStore(t)

	if err := s.SaveConfig(`{"name":"app-config","kind":"ConfigMap","data":{"k":"v"}}`); err != nil {
		t.Fatalf("SaveConfig(缺 namespace) 失败: %v", err)
	}
	// 落盘 key 应为 configs/default/app-config
	if _, ok, err := s.Get("configs/default/app-config"); err != nil || !ok {
		t.Errorf("缺 namespace 时应落盘到 configs/default/app-config（ok=%v, err=%v）", ok, err)
	}
	// 删除时 namespace 缺省同样填 default
	if err := s.DeleteConfig("", "app-config"); err != nil {
		t.Fatalf("DeleteConfig(缺 namespace) 失败: %v", err)
	}
	if _, ok, err := s.Get("configs/default/app-config"); err != nil || ok {
		t.Errorf("删除后 configs/default/app-config 应不存在（ok=%v, err=%v）", ok, err)
	}
}
