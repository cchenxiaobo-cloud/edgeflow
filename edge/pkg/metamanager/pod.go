// Pod 元数据存储（M2 前置：PodSync 消息落盘 MetaManager）。
//
// 设计：Pod 元数据复用通用 KV 表 meta_kv，key 前缀 pods/，value 是
// Pod 的 JSON 字符串（与云端 PodSync 下发内容一致，原样保存，不做字段
// 裁剪——M2 由 Edged 消费时按需解析）。与节点信息（node:info:）互不干扰。
package metamanager

import (
	"encoding/json"
	"errors"
	"fmt"
)

// podKeyPrefix 是 Pod 元数据的 key 前缀：
// 一条 Pod 对应一个 key（pods/<namespace>/<name>），List 前缀扫描即可全部列出。
// 旧版 key（pods/<name>，不含 namespace）为开发期数据，不做迁移（见 podKey 注释）。
const podKeyPrefix = "pods/"

// podKey 派生 Pod 存储 key：pods/<namespace>/<name>。
// namespace 缺省填 "default"（对齐 K8s 语义）：
//   - 多命名空间同名 Pod（如 default/nginx 与 prod/nginx）互不覆盖，delete 也不会误删他命名空间记录；
//   - 旧版 key pods/<name> 是开发期数据（P1-3 修复前写入），本仓库不迁移——
//     若需保留可手工改为 pods/default/<name>，或由云端重新下发。
func podKey(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	return podKeyPrefix + namespace + "/" + name
}

// Pod 是边缘端缓存的 Pod 元数据（与云端 PodSync 契约的 pod 字段一致，
// 字段不可改；后续 M2 扩展字段时在此追加并保持向后兼容）。
type Pod struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Replicas  int    `json:"replicas"`
}

// SavePod 把 Pod 的 JSON 表示落盘（PodSync operation=add/update 时调用）。
// 幂等：同 namespace+name 重复保存只覆盖，不新增记录（对应云端重复下发同 Pod）。
// 入参必须是合法 JSON 且含 name 字段（key 由 namespace+name 派生，namespace 缺省 "default"），
// 否则报错——错误会经 EdgeHub 自动 Ack 以 code=error 回传云端。
func (s *Store) SavePod(podJSON string) error {
	// 校验合法性并提取 namespace/name（key 派生依据）；额外字段忽略，不做裁剪
	var pod Pod
	if err := json.Unmarshal([]byte(podJSON), &pod); err != nil {
		return fmt.Errorf("解析 Pod JSON 失败: %w", err)
	}
	if pod.Name == "" {
		return errors.New("Pod JSON 缺少 name 字段")
	}
	return s.Put(podKey(pod.Namespace, pod.Name), podJSON)
}

// ListPods 返回全部已落盘的 Pod JSON（按 key 升序，即按 namespace/name 排序）。
// 启动时用于日志展示"已加载 N 条 Pod 元数据"；M2 由 Edged 消费驱动容器运行时。
func (s *Store) ListPods() ([]string, error) {
	entries, err := s.List(podKeyPrefix)
	if err != nil {
		return nil, err
	}
	pods := make([]string, 0, len(entries))
	for _, e := range entries {
		pods = append(pods, e.Value)
	}
	return pods, nil
}

// DeletePod 删除一条 Pod 元数据（PodSync operation=delete 时调用）。
// 幂等：Pod 不存在时静默成功（对应云端重复下发删除）。
// namespace 缺省 "default"（与 SavePod 的 key 派生规则一致），
// 只删除该命名空间下的同名 Pod，不影响其他命名空间。
func (s *Store) DeletePod(namespace, name string) error {
	if name == "" {
		return errors.New("Pod 名称不能为空")
	}
	return s.Delete(podKey(namespace, name))
}
