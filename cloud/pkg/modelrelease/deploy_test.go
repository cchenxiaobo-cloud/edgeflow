package modelrelease

// 部署执行器单测（设计 §10.1 C4；fake reliableSend 注入）：
//
//   - 消息载荷断言：podsync pod 名/namespace/镜像/replicas；config-sync
//     保留键 + metadata 平铺 + 冲突键保留优先；
//   - 消息顺序断言：podsync 先于 config-sync；podsync 失败 → 不发
//     config-sync（半部署不产生）；
//   - 错误映射全路径：offline/timeout/rejected/其他 → perNode 文案；
//   - 写穿失败 → Warn 不出错（DeployVersion 仍返回 nil）；
//   - DeployError.Unwrap 支持 errors.Is。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/pkg/protocol"
)

// fakeSender 是可靠投递 fake：记录消息、按序号/类型可控失败。
type fakeSender struct {
	mu       sync.Mutex
	msgs     []*protocol.Message
	failAll  error         // 非 nil：全部发送失败
	failOnce int           // >0：前 N 次发送失败（模拟半部署）
	errSeq   map[int]error // 序号（0 起）→ 错误
}

func (f *fakeSender) send(_ context.Context, _ string, msg *protocol.Message, _ cloudhub.ReliableOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := len(f.msgs)
	f.msgs = append(f.msgs, msg)
	if f.failAll != nil {
		return f.failAll
	}
	if f.failOnce > 0 {
		f.failOnce--
		return errors.New("simulated transient failure")
	}
	if err, ok := f.errSeq[idx]; ok {
		return err
	}
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

func (f *fakeSender) msg(i int) *protocol.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.msgs[i]
}

// decodePodPayload 解析 PodSync 载荷（fake 侧校验用）。
type fakePodPayload struct {
	Operation string `json:"operation"`
	Pod       struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Image     string `json:"image"`
		Replicas  int    `json:"replicas"`
	} `json:"pod"`
}

// fakeCfgPayload 解析 ConfigSync 载荷。
type fakeCfgPayload struct {
	Operation string `json:"operation"`
	Config    struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Kind      string            `json:"kind"`
		Data      map[string]string `json:"data"`
	} `json:"config"`
}

// newDeployFixture 构造 Deployer + 模型/版本台账 + fake sender。
func newDeployFixture(t *testing.T) (*Deployer, *fakeSender, *modelrepo.Model, *modelrepo.ModelVersion) {
	t.Helper()
	store := modelrepo.NewMemoryModelStore()
	ctx := context.Background()
	model := &modelrepo.Model{Name: "Defect.Detector", Type: "detection"}
	if err := store.CreateModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	ver := &modelrepo.ModelVersion{
		Model:   model.Name,
		Version: "v1.2.0",
		Mirror:  "registry.example.com/edgeflow/models/defect-detector:v1.2.0",
		Sha256:  "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		Archs:   []string{"amd64"},
		Metadata: map[string]string{
			"threshold": "0.8",
			"batchSize": "32",
			"version":   "evil-metadata", // 与保留键冲突，应被保留键覆盖
		},
	}
	if err := store.CreateVersion(ctx, ver); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetVersion(ctx, model.Name, ver.Version)
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	d, err := NewDeployer(store, sender.send)
	if err != nil {
		t.Fatal(err)
	}
	d.Now = func() time.Time { return time.UnixMilli(1787000000000) }
	return d, sender, model, got
}

// TestC4_DeployVersion_载荷 断言三要素：消息类型/目标节点/载荷内容
// （命名约定 edgeflow-model-<sanitized>、namespace=edgeflow、
// config-sync 保留键 + metadata 平铺 + 冲突保留键优先）。
func TestC4_DeployVersion_载荷(t *testing.T) {
	d, sender, model, ver := newDeployFixture(t)
	if err := d.DeployVersion(context.Background(), "edge-node-1", "rel-1", ver); err != nil {
		t.Fatalf("DeployVersion 意外失败: %v", err)
	}
	if sender.count() != 2 {
		t.Fatalf("应恰好 2 条消息（podsync+config-sync），got %d", sender.count())
	}

	// ① PodSync：add + 命名约定（Defect.Detector → defect-detector）
	pm := sender.msg(0)
	if pm.Type != protocol.TypePodSync || pm.Source != "cloud" || pm.Target != "edge-node-1" {
		t.Fatalf("podsync 信封错误: %+v", pm)
	}
	var pod fakePodPayload
	if err := pm.DecodePayload(&pod); err != nil {
		t.Fatal(err)
	}
	if pod.Operation != "add" {
		t.Fatalf("podsync operation = %q, want add", pod.Operation)
	}
	wantName := "edgeflow-model-defect-detector"
	if pod.Pod.Name != wantName {
		t.Fatalf("pod.name = %q, want %q", pod.Pod.Name, wantName)
	}
	if pod.Pod.Namespace != "edgeflow" {
		t.Fatalf("namespace = %q, want edgeflow", pod.Pod.Namespace)
	}
	if pod.Pod.Image != ver.Mirror {
		t.Fatalf("image = %q, want %q", pod.Pod.Image, ver.Mirror)
	}
	if pod.Pod.Replicas != 1 {
		t.Fatalf("replicas = %d, want 1", pod.Pod.Replicas)
	}

	// ② ConfigSync：add + ConfigMap + 保留键 + 平铺 + 冲突保留键优先
	cm := sender.msg(1)
	if cm.Type != protocol.TypeConfigSync || cm.Target != "edge-node-1" {
		t.Fatalf("config-sync 信封错误: %+v", cm)
	}
	var cfg fakeCfgPayload
	if err := cm.DecodePayload(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Operation != "add" || cfg.Config.Kind != "ConfigMap" || cfg.Config.Namespace != "edgeflow" {
		t.Fatalf("config-sync operation/kind/namespace 错误: %+v", cfg.Config)
	}
	if cfg.Config.Name != wantName {
		t.Fatalf("config.name = %q, want %q", cfg.Config.Name, wantName)
	}
	data := cfg.Config.Data
	if data["model"] != model.Name {
		t.Fatalf("保留键 model = %q", data["model"])
	}
	if data["version"] != "v1.2.0" {
		t.Fatalf("保留键 version 应被保留（冲突 metadata 不得覆盖）: %q", data["version"])
	}
	if data["mirror"] != ver.Mirror || data["sha256"] != ver.Sha256 || data["type"] != "detection" {
		t.Fatalf("保留键 mirror/sha256/type 缺失或错误: %+v", data)
	}
	if data["releasedAt"] != "1787000000000" {
		t.Fatalf("releasedAt 保留键 = %q, want 1787000000000", data["releasedAt"])
	}
	if data["threshold"] != "0.8" || data["batchSize"] != "32" {
		t.Fatalf("metadata 平铺缺失: %+v", data)
	}
	if len(data) != 8 { // 6 保留键 + 2 平铺（version 冲突被保留键截断）
		t.Fatalf("data 键数 = %d, want 8: %v", len(data), data)
	}

	// ③ 部署影子写穿
	ds, err := d.Store.ListDeployments(context.Background(), model.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("部署影子应有 1 条，got %d", len(ds))
	}
	if ds[0].NodeID != "edge-node-1" || ds[0].Version != "v1.2.0" ||
		ds[0].Mirror != ver.Mirror || ds[0].ReleaseID != "rel-1" {
		t.Fatalf("部署影子内容错误: %+v", ds[0])
	}
}

// TestC4_Podsync失败_不发ConfigSync 半部署不产生：podsync 失败 →
// config-sync 不发 + 错误文案映射。
func TestC4_Podsync失败_不发ConfigSync(t *testing.T) {
	for _, mock := range []struct {
		name string
		err  error
		want string
	}{
		{"offline", cloudhub.ErrNodeOffline, "node offline or not registered"},
		{"timeout", cloudhub.ErrAckTimeout, "ack timeout after retries"},
		{"rejected", cloudhub.ErrAckFailed, "edge rejected ack"},
		{"other", errors.New("connection reset"), "send failed: connection reset"},
	} {
		t.Run(mock.name, func(t *testing.T) {
			d, sender, _, ver := newDeployFixture(t)
			sender.failAll = mock.err
			err := d.DeployVersion(context.Background(), "edge-node-1", "rel-1", ver)
			if sender.count() != 1 {
				t.Fatalf("podsync 失败后不应发 config-sync，got %d 条", sender.count())
			}
			if err == nil {
				t.Fatal("应返回错误")
			}
			reason := DeployReason(err)
			if reason != mock.want {
				t.Fatalf("Reason = %q, want %q", reason, mock.want)
			}
			// DeployError.Unwrap 支持 errors.Is 判定原错误（控制器/API 可判）
			if !errors.Is(err, mock.err) {
				t.Fatalf("errors.Is(err, 原错误) 应成立: %v", err)
			}
		})
	}
}

// TestC4_ConfigSync失败_半部署L23 podsync 成功、config-sync 失败 →
// 返回错误（perNode 计 failed），影子不写。
func TestC4_ConfigSync失败_半部署L23(t *testing.T) {
	d, sender, model, ver := newDeployFixture(t)
	sender.errSeq = map[int]error{1: cloudhub.ErrAckTimeout}
	err := d.DeployVersion(context.Background(), "edge-node-1", "rel-1", ver)
	if sender.count() != 2 {
		t.Fatalf("podsync 成功应继续发 config-sync，got %d 条", sender.count())
	}
	if err == nil || !errors.Is(err, cloudhub.ErrAckTimeout) {
		t.Fatalf("应返回 ack timeout 错误，got %v", err)
	}
	if DeployReason(err) != "ack timeout after retries" {
		t.Fatalf("Reason = %q", DeployReason(err))
	}
	ds, err := d.Store.ListDeployments(context.Background(), model.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Fatalf("config-sync 失败不应写部署影子（半部署 L23），got %d", len(ds))
	}
}

// TestC4_写穿失败_Warn不出错 下发已生效、影子写穿失败 → DeployVersion
// 仍返回 nil（对齐 sendDeviceCommand 的 SetDesired 失败处理）。
func TestC4_写穿失败_Warn不出错(t *testing.T) {
	d, sender, _, ver := newDeployFixture(t)
	// 包装 store：SetDeployment 恒失败（模拟 etcd 写穿故障）
	failing := &shimStore{ModelStore: d.Store, failSetDeployment: errors.New("etcd down")}
	d.Store = failing
	if err := d.DeployVersion(context.Background(), "edge-node-1", "rel-1", ver); err != nil {
		t.Fatalf("写穿失败应 Warn 不出错，got %v", err)
	}
	if sender.count() != 2 {
		t.Fatalf("下发消息应仍为 2 条，got %d", sender.count())
	}
}

// shimStore 覆盖 SetDeployment 的测试包装（其余委托内存 store）。
type shimStore struct {
	modelrepo.ModelStore
	failSetDeployment error
}

func (s *shimStore) SetDeployment(ctx context.Context, model, nodeID string, d modelrepo.DeploymentState) error {
	if s.failSetDeployment != nil {
		return s.failSetDeployment
	}
	return s.ModelStore.SetDeployment(ctx, model, nodeID, d)
}

// TestC4_模型缺失 DeployVersion 对不存在模型 → 错误（节点计 failed 的
// 兜底路径：目标版本不可用）。
func TestC4_模型缺失(t *testing.T) {
	store := modelrepo.NewMemoryModelStore()
	sender := &fakeSender{}
	d, err := NewDeployer(store, sender.send)
	if err != nil {
		t.Fatal(err)
	}
	ver := &modelrepo.ModelVersion{Model: "ghost", Version: "v1"}
	err = d.DeployVersion(context.Background(), "n1", "r1", ver)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("模型缺失应报错（含模型名），got %v", err)
	}
	if sender.count() != 0 {
		t.Fatalf("模型缺失不应下发任何消息，got %d", sender.count())
	}
}

// TestC4_NewDeployer 校验：nil store/send → error（fail-fast 装配语义）。
func TestC4_NewDeployer(t *testing.T) {
	send := func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error { return nil }
	if _, err := NewDeployer(nil, send); err == nil {
		t.Fatal("nil store 应报错")
	}
	if _, err := NewDeployer(modelrepo.NewMemoryModelStore(), nil); err == nil {
		t.Fatal("nil send 应报错")
	}
}
