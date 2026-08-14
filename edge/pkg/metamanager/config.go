// ConfigMap/Secret 元数据存储（M2：ConfigSync 消息落盘 MetaManager）。
//
// 设计：配置元数据复用通用 KV 表 meta_kv，key 前缀 configs/，value 是
// ConfigSync 契约中 config 字段的 JSON 字符串（与云端下发内容一致，原样
// 保存，不做字段裁剪——后续由边缘消费方（Edged 等）按需解析）。
// 与节点信息（node:info:）、Pod 元数据（pods/）互不干扰。
//
// 安全说明（重要）：ConfigMap/Secret 以明文 JSON 落盘 SQLite。当前阶段
// 与 KubeEdge 一致（边缘端文件明文保存），生产环境必须对 Secret 的
// value 加密（如 KMS/信封加密）后再落盘——见交付说明「缺口与风险」。
package metamanager

import (
	"encoding/json"
	"errors"
	"fmt"
)

// configKeyPrefix 是配置元数据的 key 前缀：
// 一条配置对应一个 key（configs/<namespace>/<name>），List 前缀扫描即可全部列出。
const configKeyPrefix = "configs/"

// configKey 派生配置存储 key：configs/<namespace>/<name>（仿 podKey）。
// namespace 缺省填 "default"（对齐 K8s 语义）：
//   - 多命名空间同名配置（如 default/app-config 与 prod/app-config）互不覆盖，
//     delete 也不会误删他命名空间记录；
//   - key 不含 kind：ConfigMap 与 Secret 同名同命名空间时共用一条记录
//     （K8s 中 ConfigMap 与 Secret 是独立资源，本实现将两者视为同一
//     配置槽位，云端下发时按 kind 覆盖——当前阶段按 name 唯一即可，
//     若需严格区分可在 key 中追加 kind，见交付说明）。
func configKey(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	return configKeyPrefix + namespace + "/" + name
}

// Config 是边缘端缓存的配置元数据（与云端 ConfigSync 契约的 config 字段
// 一致，字段不可改；后续扩展字段时在此追加并保持向后兼容）。
// Data 是 map[string]string：ConfigMap 的 value 为原始字符串；Secret 的
// value 先做 base64 编码语义（云端负责编码，边缘原样存储）。
type Config struct {
	Name      string            `json:"name"`      // 配置名称
	Namespace string            `json:"namespace"` // 命名空间（缺省 "default"）
	Kind      string            `json:"kind"`      // 类型：ConfigMap / Secret
	Data      map[string]string `json:"data"`      // 键值数据（Secret 为 base64 语义）
}

// SaveConfig 把配置的 JSON 表示落盘（ConfigSync operation=add/update 时调用）。
// 幂等：同 namespace+name 重复保存只覆盖，不新增记录（对应云端重复下发同配置）。
// 入参必须是合法 JSON 且含 name 字段（key 由 namespace+name 派生，namespace
// 缺省 "default"），否则报错——错误会经 EdgeHub 自动 Ack 以 code=error 回传云端。
// 不校验 kind 取值（云端已做白名单校验；边缘仿 SavePod 只做最小校验，原样存储）。
func (s *Store) SaveConfig(configJSON string) error {
	// 校验合法性并提取 namespace/name（key 派生依据）；额外字段忽略，不做裁剪
	var cfg Config
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("解析 Config JSON 失败: %w", err)
	}
	if cfg.Name == "" {
		return errors.New("Config JSON 缺少 name 字段")
	}
	return s.Put(configKey(cfg.Namespace, cfg.Name), configJSON)
}

// ListConfigs 返回全部已落盘的配置 JSON（按 key 升序，即按 namespace/name 排序）。
// 启动时用于日志展示"已加载 N 条配置元数据"；后续由消费方全量对账使用。
func (s *Store) ListConfigs() ([]string, error) {
	entries, err := s.List(configKeyPrefix)
	if err != nil {
		return nil, err
	}
	configs := make([]string, 0, len(entries))
	for _, e := range entries {
		configs = append(configs, e.Value)
	}
	return configs, nil
}

// DeleteConfig 删除一条配置元数据（ConfigSync operation=delete 时调用）。
// 幂等：配置不存在时静默成功（对应云端重复下发删除）。
// namespace 缺省 "default"（与 SaveConfig 的 key 派生规则一致），
// 只删除该命名空间下的同名配置，不影响其他命名空间。
func (s *Store) DeleteConfig(namespace, name string) error {
	if name == "" {
		return errors.New("配置名称不能为空")
	}
	return s.Delete(configKey(namespace, name))
}
