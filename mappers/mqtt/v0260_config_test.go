package mqtt

// v0.26.0 配置文件加载测试：EDGEFLOW_MQTT_CONFIG 的 yaml/json 解析、
// 回退链优先级（With 选项 > 环境变量 > 配置文件 > 默认值）、文件缺失
// 与坏格式的软失败语义。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clearMQTTEnv 清空全部 MQTT 环境变量（hermetic：防止外层环境泄漏进
// 回退链断言）。
func clearMQTTEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvBroker, EnvTopics, EnvCmdTopic, EnvDeviceName, EnvNamespace, EnvTLSCA, EnvTLSInsecure, EnvConfig} {
		t.Setenv(k, "")
	}
}

// writeConfigFile 把内容写入临时目录并返回路径。
func writeConfigFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写配置文件 %s 失败: %v", path, err)
	}
	return path
}

func TestV0260ConfigFileYAMLFallback(t *testing.T) {
	clearMQTTEnv(t)
	path := writeConfigFile(t, "mqtt.yaml", "# EdgeFlow MQTT 配置\nbroker: file-broker:18883\ntopics: file/+/state, file/#\nnamespace: file-ns\nkeep_alive_seconds: 45\n")
	t.Setenv(EnvConfig, path)

	m := New("")
	if m.broker != "file-broker:18883" {
		t.Fatalf("broker = %q, want file-broker:18883", m.broker)
	}
	if len(m.topics) != 2 || m.topics[0] != "file/+/state" || m.topics[1] != "file/#" {
		t.Fatalf("topics = %v, want [file/+/state file/#]", m.topics)
	}
	// device_name 键在键面保留（v0.26.0 无行为），设备名应仍为默认值。
	if m.deviceName != DefaultDeviceName {
		t.Fatalf("deviceName = %q, want 默认 %q（device_name 键暂不接入回退链）", m.deviceName, DefaultDeviceName)
	}
	if m.namespace != "file-ns" {
		t.Fatalf("namespace = %q, want file-ns", m.namespace)
	}
	if m.keepAlive != 45*time.Second {
		t.Fatalf("keepAlive = %v, want 45s", m.keepAlive)
	}
}

func TestV0260ConfigEnvOverridesFile(t *testing.T) {
	clearMQTTEnv(t)
	path := writeConfigFile(t, "mqtt.yml", "broker: file-broker:18883\ntopics: file/+/state\n")
	t.Setenv(EnvConfig, path)
	t.Setenv(EnvBroker, "env-broker:19999")
	t.Setenv(EnvTopics, "env/+/state")

	m := New("")
	if m.broker != "env-broker:19999" {
		t.Fatalf("broker = %q, want env-broker:19999（env 应覆盖文件）", m.broker)
	}
	if len(m.topics) != 1 || m.topics[0] != "env/+/state" {
		t.Fatalf("topics = %v, want [env/+/state]（env 应覆盖文件）", m.topics)
	}
}

func TestV0260ConfigJSONLoad(t *testing.T) {
	clearMQTTEnv(t)
	path := writeConfigFile(t, "mqtt.json", `{"broker":"json-broker:18883","topics":"json/+/state","keep_alive_seconds":"60"}`)
	t.Setenv(EnvConfig, path)

	m := New("")
	if m.broker != "json-broker:18883" {
		t.Fatalf("broker = %q, want json-broker:18883", m.broker)
	}
	if len(m.topics) != 1 || m.topics[0] != "json/+/state" {
		t.Fatalf("topics = %v, want [json/+/state]", m.topics)
	}
	// device_name 键在键面保留（v0.26.0 无行为），设备名应仍为默认值。
	if m.deviceName != DefaultDeviceName {
		t.Fatalf("deviceName = %q, want 默认 %q（device_name 键暂不接入回退链）", m.deviceName, DefaultDeviceName)
	}
	if m.keepAlive != 60*time.Second {
		t.Fatalf("keepAlive = %v, want 60s", m.keepAlive)
	}
}

func TestV0260ConfigFileMissingSoftFail(t *testing.T) {
	clearMQTTEnv(t)
	missing := filepath.Join(t.TempDir(), "no-such-config.yaml")
	t.Setenv(EnvConfig, missing)

	m := New("") // 不 panic：加载失败仅记日志，字段走默认值
	if m.broker != "" {
		t.Fatalf("broker = %q, want 空（默认）", m.broker)
	}
	if m.tlsCAPath != "" {
		t.Fatalf("tlsCAPath = %q, want 空", m.tlsCAPath)
	}
	if len(m.topics) != 1 || m.topics[0] != DefaultTopics[0] {
		t.Fatalf("topics = %v, want 默认 %v", m.topics, DefaultTopics)
	}
	if m.keepAlive != DefaultKeepAlive {
		t.Fatalf("keepAlive = %v, want 默认 %v", m.keepAlive, DefaultKeepAlive)
	}
}

func TestV0260ConfigUnsupportedExtSoftFail(t *testing.T) {
	clearMQTTEnv(t)
	path := writeConfigFile(t, "mqtt.toml", "broker = \"toml-broker\"\n")
	t.Setenv(EnvConfig, path)

	m := New("") // 不支持的扩展名：软失败，走默认
	if m.broker != "" {
		t.Fatalf("broker = %q, want 空（不支持的类型应被忽略）", m.broker)
	}
}

func TestV0260ConfigWithOptionHighestPriority(t *testing.T) {
	clearMQTTEnv(t)
	path := writeConfigFile(t, "mqtt.yaml", "broker: file-broker:18883\ntopics: file/+/state\n")
	t.Setenv(EnvConfig, path)
	t.Setenv(EnvBroker, "env-broker:19999")

	m := New("", WithBroker("opt-broker:17777"), WithTopics([]string{"opt/+/state"}))
	if m.broker != "opt-broker:17777" {
		t.Fatalf("broker = %q, want opt-broker:17777（With 选项最高优先级）", m.broker)
	}
	if len(m.topics) != 1 || m.topics[0] != "opt/+/state" {
		t.Fatalf("topics = %v, want [opt/+/state]（With 选项最高优先级）", m.topics)
	}
}

func TestV0260ConfigExplicitBrokerArgBeatsFile(t *testing.T) {
	clearMQTTEnv(t)
	path := writeConfigFile(t, "mqtt.yaml", "broker: file-broker:18883\n")
	t.Setenv(EnvConfig, path)
	t.Setenv(EnvBroker, "env-broker:19999")

	m := New("arg-broker:16666") // 显式入参 = With 同级，最高优先
	if m.broker != "arg-broker:16666" {
		t.Fatalf("broker = %q, want arg-broker:16666", m.broker)
	}
}

func TestV0260ConfigKeepAliveInvalidIgnored(t *testing.T) {
	clearMQTTEnv(t)
	path := writeConfigFile(t, "mqtt.yaml", "keep_alive_seconds: not-a-number\n")
	t.Setenv(EnvConfig, path)

	m := New("")
	if m.keepAlive != DefaultKeepAlive {
		t.Fatalf("keepAlive = %v, want 默认 %v（非法值应被忽略）", m.keepAlive, DefaultKeepAlive)
	}
}

func TestV0260ConfigTLSKeysFromYAML(t *testing.T) {
	clearMQTTEnv(t)
	path := writeConfigFile(t, "mqtt.yaml", "tls_ca_path: /etc/edgeflow/ca.pem\ntls_insecure: \"1\"\n")
	t.Setenv(EnvConfig, path)

	m := New("")
	if m.tlsCAPath != "/etc/edgeflow/ca.pem" {
		t.Fatalf("tlsCAPath = %q, want /etc/edgeflow/ca.pem", m.tlsCAPath)
	}
	if !m.tlsInsecure {
		t.Fatal("tlsInsecure = false, want true（配置文件 tls_insecure: \"1\"）")
	}
}

func TestV0260ConfigQuotedValuesStripped(t *testing.T) {
	clearMQTTEnv(t)
	path := writeConfigFile(t, "mqtt.yaml", `broker: "quoted-broker:18883"`+"\n"+`namespace: 'single-quoted-ns'`+"\n")
	t.Setenv(EnvConfig, path)

	m := New("")
	if m.broker != "quoted-broker:18883" {
		t.Fatalf("broker = %q, want quoted-broker:18883（双引号应剥离）", m.broker)
	}
	if m.namespace != "single-quoted-ns" {
		t.Fatalf("namespace = %q, want single-quoted-ns（单引号应剥离）", m.namespace)
	}
}
