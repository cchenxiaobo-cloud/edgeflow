package modelrelease

// 部署执行器（设计 §6，WBS-6/deploy.go；C4 测试）。
//
// DeployVersion 对单目标节点执行三步下发（幂等，可被接管重试/回滚重复
// 调用，不产生重复副作用——podsync/config-sync 边缘 SavePod/SaveConfig
// 同键覆盖 + EdgeHub 消息级 ID 去重（F05））：
//
//	1. podsync add：edgeflow-model-<sanitized> Pod（namespace=edgeflow，
//	   image=版本镜像，replicas=1）；
//	2. config-sync add：同名 ConfigMap，data = 保留键（model/version/
//	   mirror/sha256/type/releasedAt）+ version.Metadata 平铺追加（冲突
//	   键保留键优先 + Warn，防覆盖版本标识，设计 §6.2）；回滚时 version
//	   键改回 PrevActive——边缘无状态机依赖，纯声明式收敛；
//	3. 两步均 acked → 部署影子写穿 SetDeployment（设计 §6.3；写穿失败
//	   仅 Warn 不出错——下发已生效，仅影子视图缺该记录）。
//
// 错误映射（写入 perNode.Reason 的机器可读文案，对齐 sendDeviceCommand
// 六态语义，设计 §6.1）：ErrNodeOffline → `node offline or not registered`；
// ErrAckTimeout → `ack timeout after retries`；ErrAckFailed →
// `edge rejected ack`；其余 → `send failed: <err>`。
//
// 两步顺序与半部署（L23 登记）：podsync 失败 → 不发 config-sync，
// perNode=failed；podsync 成功、config-sync 失败 → perNode=failed
// （节点已切镜像但参数未更新 = 半部署状态，重试发布或回滚收敛，边缘
// 声明式调谐保证最终一致）。
//
// 命名约定（产品契约，设计 §6.1）：sanitize(name) = name 转小写 +
// '.'→'-'（modelrepo.SanitizeModelName）；podName = cfgName =
// "edgeflow-model-" + sanitize(modelName)；namespace 固定 "edgeflow"；
// replicas=1 固定（模型实例多副本由用户自行 podsync 编排）。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// VersionDeployer 是控制器依赖的部署执行器能力面（测试注入 fake
// deployer；生产实现 = *Deployer）。
type VersionDeployer interface {
	// DeployVersion 把版本部署到单节点（设计 §6.1 三步）。返回错误时
	// 调用方用 DeployReason(err) 提取写入 perNode.Reason 的机器可读文案。
	DeployVersion(ctx context.Context, nodeID, releaseID string, ver *modelrepo.ModelVersion) error
}

// 编译期断言：*Deployer 满足控制器依赖面。
var _ VersionDeployer = (*Deployer)(nil)

// Deployer 是部署执行器（设计 WBS-5）：可靠投递函数可注入（对齐
// sendDeviceCommand 的 reliableSend 注入模式），单测用 fake 断言载荷。
//
// 零值不可用：NewDeployer 构造（store/send 均必填，nil → error）。
// ReliableSend 语义 = cloudhub.Server.ReliableSendContext（默认
// 5s×3 次尝试；测试注入 fake）。
type Deployer struct {
	Store modelrepo.ModelStore
	// ReliableSend 是云边可靠投递函数（生产 = hub.ReliableSendContext；
	// 测试 = fake 记录消息并模拟 offline/timeout/rejected）。
	ReliableSend func(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error
	// Now 是时钟（测试注入；nil → time.Now）。
	Now func() time.Time
}

// NewDeployer 构造部署执行器（store/send 必填，缺失 fail-fast 对齐
// NewEtcdModelStore 的 nil 校验风格）。
func NewDeployer(store modelrepo.ModelStore, send func(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error) (*Deployer, error) {
	if store == nil {
		return nil, fmt.Errorf("modelrelease: NewDeployer 需要非 nil 的 ModelStore")
	}
	if send == nil {
		return nil, fmt.Errorf("modelrelease: NewDeployer 需要非 nil 的 ReliableSend")
	}
	return &Deployer{Store: store, ReliableSend: send, Now: time.Now}, nil
}

// DeployError 是部署失败错误：Reason 为写入 perNode.Reason 的机器可读
// 文案（设计 §6.1 映射表）；Err 保留原始错误（errors.Is 可判
// cloudhub.ErrNodeOffline/ErrAckTimeout/ErrAckFailed）。
type DeployError struct {
	Reason string
	Err    error
}

func (e *DeployError) Error() string { return e.Reason }

// Unwrap 支持 errors.Is(err, cloudhub.Err*)。
func (e *DeployError) Unwrap() error { return e.Err }

// classifySendError 把可靠投递错误映射为机器可读文案（设计 §6.1 映射表，
// 导出供测试与 API 断言）。
func classifySendError(err error) string {
	switch {
	case errors.Is(err, cloudhub.ErrNodeOffline):
		return "node offline or not registered"
	case errors.Is(err, cloudhub.ErrAckTimeout):
		return "ack timeout after retries"
	case errors.Is(err, cloudhub.ErrAckFailed):
		return "edge rejected ack"
	default:
		return "send failed: " + err.Error()
	}
}

// DeployReason 提取可写入 perNode.Reason 的部署错误文案（控制器/API
// 共用）：*DeployError → Reason（映射表文案）；其余错误走映射表
// classifySendError（裸 cloudhub.Err* 也统一机器可读，设计 §6.1）。
func DeployReason(err error) string {
	if err == nil {
		return ""
	}
	var de *DeployError
	if errors.As(err, &de) {
		return de.Reason
	}
	return classifySendError(err)
}

// BuildPodSyncPayload 构造 podsync 消息载荷（operation=add；设计 §6.1/§6.4
// 命名约定）。导出供 H1 载荷断言；DeployVersion 内部使用。
func BuildPodSyncPayload(ver *modelrepo.ModelVersion) map[string]any {
	return map[string]any{
		"operation": "add",
		"pod": map[string]any{
			"name":      DeploymentObjectName(ver.Model),
			"namespace": "edgeflow",
			"image":     ver.Mirror,
			"replicas":  1,
		},
	}
}

// DeploymentObjectName 返回下发对象名（podName = cfgName =
// "edgeflow-model-" + sanitize(modelName)；设计 §6.1 命名约定，产品契约）。
func DeploymentObjectName(modelName string) string {
	return "edgeflow-model-" + modelrepo.SanitizeModelName(modelName)
}

// BuildConfigSyncData 构造 config-sync 配置数据（设计 §6.2 保留键约定）：
//
//	保留键：model/version/mirror/sha256/type/releasedAt（随版本走，
//	由发布器保证覆盖写）；
//	version.Metadata 全部键值平铺追加（模型参数随版本走）；
//	与保留键冲突 → 保留键优先 + 控制器日志 Warn（防覆盖版本标识）。
//
// releasedAt 为部署时刻（Unix 毫秒，转字符串——data 为 map[string]string）。
func BuildConfigSyncData(model *modelrepo.Model, ver *modelrepo.ModelVersion, releasedAt int64) map[string]string {
	data := map[string]string{
		"model":      model.Name,
		"version":    ver.Version,
		"mirror":     ver.Mirror,
		"sha256":     ver.Sha256,
		"type":       model.Type,
		"releasedAt": strconv.FormatInt(releasedAt, 10),
	}
	for k, v := range ver.Metadata {
		if _, reserved := data[k]; reserved {
			log.Warnf("[modelrelease] config-sync 元数据键 %q 与保留键冲突，保留键优先（model=%s version=%s）", k, model.Name, ver.Version)
			continue
		}
		data[k] = v
	}
	return data
}

// BuildConfigSyncPayload 构造 config-sync 消息载荷（operation=add；
// ConfigMap 形态如设计 §6.2）。releasedAt 由调用方传入（部署时刻，
// Unix 毫秒；测试可固定值断言）——设计 WBS-5 签名无 releasedAt 参数，
// 此处以显式参数落地"releasedAt 保留键随版本走"契约（实施登记：
// 导出形态与设计一致，仅多一枚时间戳参数，H1 载荷断言传固定值即可）。
func BuildConfigSyncPayload(model *modelrepo.Model, ver *modelrepo.ModelVersion, releasedAt int64) map[string]any {
	return map[string]any{
		"operation": "add",
		"config": map[string]any{
			"name":      DeploymentObjectName(model.Name),
			"namespace": "edgeflow",
			"kind":      "ConfigMap",
			"data":      BuildConfigSyncData(model, ver, releasedAt),
		},
	}
}

// DeployVersion 实现 VersionDeployer（设计 §6.1 三步 + §6.3 写穿）：
// podsync add → 均 acked 后 config-sync add → 均 acked 后部署影子写穿
// （失败仅 Warn 不出错）。返回错误语义见文件头（DeployError/DeployReason）。
func (d *Deployer) DeployVersion(ctx context.Context, nodeID, releaseID string, ver *modelrepo.ModelVersion) error {
	if d == nil || d.Store == nil || d.ReliableSend == nil {
		return fmt.Errorf("modelrelease: deployer 未装配（Store/ReliableSend 必填）")
	}
	if ver == nil {
		return fmt.Errorf("modelrelease: DeployVersion 收到 nil 版本")
	}
	model, err := d.Store.GetModel(ctx, ver.Model)
	if err != nil {
		return fmt.Errorf("modelrelease: 取模型 %s 失败: %w", ver.Model, err)
	}
	now := time.Now().UnixMilli()
	if d.Now != nil {
		now = d.Now().UnixMilli()
	}

	// ① podsync add（幂等 upsert；失败 → 不发 config-sync，半部署不产生）
	podMsg, err := protocol.NewMessage(protocol.TypePodSync, "cloud", nodeID, BuildPodSyncPayload(ver))
	if err != nil {
		return fmt.Errorf("modelrelease: 构建 PodSync 消息失败: %w", err)
	}
	if err := d.ReliableSend(ctx, nodeID, podMsg, cloudhub.ReliableOptions{}); err != nil {
		return &DeployError{Reason: classifySendError(err), Err: err}
	}

	// ② config-sync add（保留键 + metadata 平铺；失败 → 半部署 L23）
	cfgMsg, err := protocol.NewMessage(protocol.TypeConfigSync, "cloud", nodeID, BuildConfigSyncPayload(model, ver, now))
	if err != nil {
		return fmt.Errorf("modelrelease: 构建 ConfigSync 消息失败: %w", err)
	}
	if err := d.ReliableSend(ctx, nodeID, cfgMsg, cloudhub.ReliableOptions{}); err != nil {
		return &DeployError{Reason: classifySendError(err), Err: err}
	}

	// ③ 部署影子写穿（设计 §6.3）：失败 → Warn 不出错（下发已生效，
	// 仅影子视图缺记录，release/perNode 已持久化不受影响；对齐
	// sendDeviceCommand 的 SetDesired 失败处理）。
	if err := d.Store.SetDeployment(ctx, model.Name, nodeID, modelrepo.DeploymentState{
		Model:     model.Name,
		NodeID:    nodeID,
		Version:   ver.Version,
		Mirror:    ver.Mirror,
		ReleaseID: releaseID,
	}); err != nil {
		log.Warnf("[modelrelease] 部署影子写穿失败（nodeID=%s release=%s model=%s）: %v", nodeID, releaseID, model.Name, err)
	}
	return nil
}
