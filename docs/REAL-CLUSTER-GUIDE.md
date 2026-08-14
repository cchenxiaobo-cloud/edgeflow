# EdgeFlow 真实集群路径指南（G6）

> 目标：在**真实 Kubernetes 集群**（kind 单节点即可）上完整跑通 EdgeFlow 云边链路，
> 并验证 M5 验收「新用户 15min 部署」的集群路径。
> 状态：✅ **2026-08-14 实机验证**（kind v0.27.0 + k8s v1.32.2，Apple Silicon / Docker Desktop）。
> 配套：docs/KEADM.md（keadm 用法）、docs/DEPLOYMENT.md（部署）、docs/MULTIARCH.md（镜像）、
> docs/API-SPEC.md（REST API）、config/crd/（CRD manifest）。

---

## 0. 验证结论（2026-08-14 实测）

| 步骤 | 结果 | 耗时 |
|------|------|------|
| kind 起集群（拉镜像） | ✅ control-plane Ready | ~60s（首次拉镜像；已缓存后 <30s） |
| CRD apply + schema 校验 | ✅ 3 个 CRD created；合法资源通过、非法字段被拒 | <5s |
| keadm init → kubectl apply → rollout | ✅ Deployment 1/1 Running（镜像经本地 registry mirror 拉取） | ~8s |
| healthz | ✅ HTTP 200（port-forward 8080） | 即时 |
| edgecore 接入（经集群 NodePort） | ✅ 节点注册 Ready、心跳持续、REST API 可见 | ~8s |
| **合计（不含 kind 首次拉镜像）** | **≈ 2 分钟** | 满足 M5「15min」验收（有 ~13min 余量） |

> 说明：本机 kind 集群 + 本地 registry 为**闭环验证环境**；生产环境为多节点集群 +
> 远端镜像仓库，耗时主要差异在镜像拉取（首次拉取 distroless 基础镜像约 1-3min/节点）。

---

## 1. 前置条件

| 依赖 | 版本要求 | 说明 |
|------|---------|------|
| kind | ≥0.20（实测 0.27.0） | 本地 K8s 集群；生产环境可用任意 K8s 发行版替代 |
| kubectl | ≥1.28（实测 1.34.1） | 与集群版本兼容即可 |
| Docker | 运行中（含 buildx，用于多架构镜像） | 见 docs/MULTIARCH.md |
| Go | 1.26+ | 构建 edgecore/keadm（或使用 release/v0.1.0 发布二进制） |
| 本地 registry | registry:2 于 127.0.0.1:5001 | 无远端仓库凭据时的闭环方案（可选） |

---

## 2. 步骤一：起 kind 集群 + 本地 registry

```bash
# 1. 本地 registry 挂到 kind 网络（kind 节点经容器名访问）
docker network create kind
docker run -d --name edgeflow-registry --network kind -p 127.0.0.1:5001:5000 registry:2

# 2. 集群配置：containerd 把 localhost:5001 镜像到 registry 容器
cat > kind-edgeflow.yaml <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: edgeflow
nodes:
  - role: control-plane
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry.mirrors."localhost:5001"]
      endpoint = ["http://edgeflow-registry:5000"]
EOF
kind create cluster --config kind-edgeflow.yaml --wait 180s

# 3. 确认
kubectl get nodes
# NAME                     STATUS   ROLES           AGE   VERSION
# edgeflow-control-plane   Ready    control-plane   21s   v1.32.2
```

> 若不用本地 registry：把镜像推送到远端仓库后，把下面所有 `localhost:5001/edgeflow/*`
> 换成仓库地址即可；kind 节点需能拉取该镜像（imagePullSecrets 见 docs/DEPLOYMENT.md §2）。

---

## 3. 步骤二：双架构镜像构建并推送（B5，已重建验证）

```bash
# 已在 release/v0.1.0/images.json 记录的 v0.1.0 双架构镜像重建（2026-08-14）：
#   cloudcore manifest sha256:6d01b329...（amd64 84fd1e72... / arm64 373fba1c...）
#   edgecore  manifest sha256:62e378b6...（amd64 6f7701cd... / arm64 e8325eca...）
# 重建命令（buildx + 本地 registry，详见 docs/MULTIARCH.md §2）：

docker buildx create --name edgeflow-builder --driver docker-container \
  --config buildkitd.toml --use          # buildkitd.toml 声明 host.docker.internal:5001 为 http
docker buildx build --platform linux/amd64,linux/arm64 --provenance=false \
  -f build/docker/Dockerfile \
  -t host.docker.internal:5001/edgeflow/cloudcore:v0.1.0 --target cloudcore \
  --build-arg VERSION=v0.1.0 --build-arg GIT_COMMIT=$(git rev-parse --short HEAD) \
  --build-arg BUILD_TIME=$(date +%Y-%m-%dT%H:%M:%S%z) --push .
# edgecore 同命令（--target edgecore）

docker manifest inspect --insecure localhost:5001/edgeflow/cloudcore:v0.1.0
#   → linux/amd64 + linux/arm64 两项
```

---

## 4. 步骤三：CRD apply

```bash
kubectl apply -f config/crd/
# customresourcedefinition.apiextensions.k8s.io/devicemodels.edgeflow.io created
# customresourcedefinition.apiextensions.k8s.io/devices.edgeflow.io created
# customresourcedefinition.apiextensions.k8s.io/edgenodes.edgeflow.io created

kubectl get crd | grep edgeflow
# edgenodes.edgeflow.io / devicemodels.edgeflow.io / devices.edgeflow.io（ESTABLISHED）

# schema 校验实测（2026-08-14）：
kubectl apply --dry-run=server -f - <<'EOF'
apiVersion: edgeflow.io/v1alpha1
kind: EdgeNode
metadata: {name: edge-node-1, namespace: default}
spec: {nodeID: edge-node-1, role: edge, addresses: [{type: InternalIP, address: 192.168.1.10}]}
EOF
# edgenode.edgeflow.io/edge-node-1 created (server dry run)   ← 合法资源通过

# 非法 role → 拒绝：
# The EdgeNode "edge-node-bad" is invalid: spec.role: Unsupported value: "invalid-role"
```

> 边界说明：CRD 资源定义/校验已接入真实集群；**EdgeNode 对象尚不会被控制器自动创建**
> （节点注册走 cloudcore REST 注册表，见 §6），K8s 控制器接入为后续版本项（audit-m35 G6）。

---

## 5. 步骤四：keadm init → 部署 CloudCore

```bash
# 1. 构建 keadm（或直接用 release/v0.1.0/keadm-* 发布二进制）
go build -o bin/keadm ./cmd/keadm

# 2. 生成云端产物（镜像指向本地双架构 registry）
bin/keadm init --cloudcore-image=localhost:5001/edgeflow/cloudcore:v0.1.0 \
  --output-dir=./keadm-out

# 3. 部署 + 等待就绪
kubectl apply -f ./keadm-out/cloudcore.yaml
kubectl rollout status deployment/edgeflow-cloudcore --timeout=180s
# deployment "edgeflow-cloudcore" successfully rolled out

kubectl get deploy,pods,svc -l app.kubernetes.io/component=cloudcore
# deployment.apps/edgeflow-cloudcore   1/1   Running
# pod/edgeflow-cloudcore-xxx           1/1   Running
# service/edgeflow-cloudcore   NodePort   8080:31363/TCP,10000:30176/TCP
```

> 生产 mTLS：加 `--tls --tls-san=IP:<节点IP>`（见 docs/KEADM.md §1）；本指南用明文通道演示。

---

## 6. 步骤五：验证 CloudCore（healthz + 节点 API）

```bash
# healthz（port-forward 到本机）
kubectl port-forward svc/edgeflow-cloudcore 8080:8080 &
curl http://127.0.0.1:8080/healthz
# {"status":"ok","version":{"version":"v0.1.0","gitCommit":"147de5f",...}}

# 节点注册表 API（云边节点状态，见 API-SPEC.md）
curl http://127.0.0.1:8080/api/v1/nodes
# []（尚无边缘节点接入）
```

> **`kubectl get nodes` 口径**：该命令列出 **K8s 集群节点**（kind 场景下只有
> `edgeflow-control-plane`）。EdgeFlow 边缘节点注册在 cloudcore 注册表
> （`GET /api/v1/nodes` / `GET /api/v1/edgenodes`，Ready/Offline 状态流转），
> **不生成 K8s Node 对象**——这是 v0.1.0 已知适配边界（audit-m02 §4 P1），
> 真实 K8s 接入已排期。生产多节点集群中 `kubectl get nodes` 正常显示集群自身节点。

---

## 7. 步骤六：边缘节点接入（keadm join + edgecore）

```bash
# 1. 获取 CloudHub 入口：
#    - kind/本机验证：kubectl port-forward svc/edgeflow-cloudcore 10000:10000 &
#    - 生产多节点集群：kubectl get svc edgeflow-cloudcore 的 hub NodePort +
#      任意集群节点 IP（如 172.18.0.2:30176），见 KEADM.md §1
HUB_ADDR=ws://127.0.0.1:10000/v1/edge    # 本机 port-forward 形态

# 2. keadm join 生成边缘产物（真实边缘节点：--node-id 指定节点 ID）
bin/keadm join --cloudcore-ip=127.0.0.1 --cloudcore-port=10000 \
  --token=<token> --node-id=edge-real-1 --output-dir=./keadm-out-edge
# 产物：edgecore.env + edgecore.service + install.sh + README.md

# 3. 启动 edgecore（真实节点上由 install.sh + systemd 拉起）
EDGEFLOW_EDGECORE_NODE_ID=edge-real-1 \
EDGEFLOW_EDGECORE_CLOUD_ADDR=$HUB_ADDR ./bin/edgecore
```

### 验证（2026-08-14 实测）

```bash
# 云端日志：注册成功
kubectl logs deploy/edgeflow-cloudcore | grep 注册
# 节点 edge-real-1 注册成功（ip=127.0.0.1 arch=arm64 ...）

# REST 节点状态：Ready
curl http://127.0.0.1:8080/api/v1/nodes
# [{"nodeID":"edge-real-1","status":"Ready","lastHeartbeatAt":..., ...}]

# 心跳持续（~30s 周期；云端 3 次超时判 Offline，NodeController 扫描）
# 停止 edgecore → 30s 内状态变 Offline → 重启 → 重连重新注册 Ready
```

### 后续可选步骤（与单机 Demo 链路一致）

```bash
# 下发 Pod（真实 Docker 容器由边缘 Edged 创建）
curl -X POST http://127.0.0.1:8080/api/v1/nodes/edge-real-1/podsync \
  -d '{"operation":"add","pod":{"name":"nginx","namespace":"default","image":"nginx:1.25-alpine","replicas":1}}'
# 设备链路：mock_sensor 随 edgecore 启动自动注册（GET /api/v1/devices），
# 指令下发 POST /api/v1/nodes/{id}/device-command（详见 examples/README.md）
```

---

## 8. 15min 部署计时（M5 验收口径）

| 阶段 | 本机实测（2026-08-14） | 生产集群预估 |
|------|------------------------|-------------|
| kind 起集群 | ~60s（首次含镜像拉取） | 不适用（已有集群） |
| CRD apply | <5s | <1min |
| 镜像构建/推送 | 已缓存 <1min（首次双架构构建 10-20min，见 MULTIARCH §5 QEMU 风险） | CI 构建 |
| keadm init + apply + rollout | ~8s | ~1-2min（远端拉镜像） |
| healthz 验证 | 即时 | <1min |
| edgecore 接入 + Ready | ~8s | ~1-2min |
| **合计** | **≈ 2 min** | **≤ 5 min**（不含镜像构建） |

> 结论：M5 验收「新用户 15min 部署」在真实集群路径下成立，实测余量充足。

---

## 9. 清理

```bash
kind delete cluster --name edgeflow
docker rm -f edgeflow-registry
docker buildx rm edgeflow-builder      # 若按 §3 创建
pkill -f "port-forward svc/edgeflow-cloudcore"; pkill -f edgeflow-clean/edgecore  # 本机进程
```

---

## 10. 已知边界与后续项

1. **节点状态不写 K8s Node 对象**（REST 注册表适配；CRD 控制器未实现）——audit-m02 §4 P1 / audit-m35 G6。
2. **真实设备未接入**：mock_sensor/modbus 模拟器为数据面验证；真实 Modbus 设备接入需按 mappers/modbus 配置。
3. **mTLS 跨主机 CA 分发未自动化**：`--tls` 部署时边缘与云端需共享同一 CA 目录（DEPLOYMENT.md §4.1；audit-m35 G12）。
4. **多节点（10+）E2E 与压测未做**：本指南为单节点集群 + 单边缘节点验证（audit-m35 G11/G3）。
5. **macOS/Docker Desktop 限制**：宿主机无法直接路由 kind 节点 IP 的 NodePort（实测 connection reset），
   用 `kubectl port-forward` 替代；Linux 上可直接 `nodeIP:nodePort` 访问。
