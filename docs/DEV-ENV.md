# EdgeFlow 开发环境方案（DEV-ENV）

> **WBS**：1.3 开发环境（本地 K8s 集群、边缘节点模拟）
> **版本**：v0.1（草案）
> **日期**：2026-08-13
> **可用性说明**：**M1 起完整可用**（云边通信打通后，`--edge` 模式可拉起真实边缘节点模拟）。当前 M0 阶段以下部分**立即可用**：kind 集群创建、二进制构建、`--cloud` 本地运行 cloudcore（healthz 验证）。
> **配套**：docs/ARCHITECTURE.md（目标架构）、hack/dev-up.sh / hack/dev-down.sh（一键脚本）
> **适用平台**：macOS（Apple Silicon, arm64），与 docs/ENV-SETUP.md 环境一致

---

## 1. 目标与拓扑

### 1.1 开发环境要解决什么

EdgeFlow 是云边两级系统，需要同时验证三样东西：

1. **云端 Kubernetes**：CRD、控制器（EdgeController/DeviceController 等）要跟 apiserver 交互；
2. **CloudCore**：云端进程（当前 M0 已有 healthz，M1 起有 CloudHub/控制器）；
3. **EdgeCore + 边缘节点**：边缘进程、断网自治、设备接入。

没有一台真实的"边缘机器"怎么办？——**在开发机上全部模拟**，这就是本方案的目的。

### 1.2 本地开发拓扑（M1 目标形态）

```
┌───────────────────────────────────────────────────────────────┐
│                    开发机（macOS arm64）                        │
│                                                               │
│  ┌───────────────────────────────────────────┐                │
│  │  kind 集群（Docker 内，模拟云端 Kubernetes） │                │
│  │  · apiserver :6443（映射到本机）             │                │
│  │  · CRD / 控制器（M1+，后续可部署进集群）      │                │
│  └───────────────────────────────────────────┘                │
│                                                               │
│  ┌───────────────────────────────────────────┐                │
│  │ cloudcore（本机进程，Mac 二进制）            │                │
│  │  · /healthz :8080                          │                │
│  │  · WebSocket :10000（M1 起）                │                │
│  │  · 通过 kubeconfig 访问 kind apiserver      │                │
│  └───────────────────────────────────────────┘                │
│                                                               │
│  ┌───────────────────────────────────────────┐                │
│  │ edgecore 模拟（两种方式，见 §3.3）           │                │
│  │  方式A：本机直接跑 edgecore（Mac 二进制）     │                │
│  │  方式B：Docker 容器跑 edgecore（Linux 二进制）│                │
│  │        （多节点场景：edgeflow-edge-1/2/3…）  │                │
│  └───────────────────────────────────────────┘                │
│                 │  EdgeHub 主动连接（WS :10000）                │
│                 ▼                                              │
│           （模拟边缘与云端的"网络"，断网自治测试 = 停掉这段连接）      │
└───────────────────────────────────────────────────────────────┘
```

**为什么 kind**：kind 把 Kubernetes 控制面跑在 Docker 容器里，创建/销毁快（分钟级）、不污染开发机、版本可指定。对 CloudCore 开发来说，一个单节点集群就够（后续多集群/多版本测试可加）。

**为什么边缘用进程/容器模拟而不是真机**：零成本、可批量（模拟 10 个节点 = 10 个容器）、可随意断网（`docker stop` 或杀掉进程）来测自治。真机验证留到 M5 规模化阶段（ROADMAP §5 WBS 8.4 性能测试再考虑）。

### 1.3 演进路线

| 阶段 | 形态 | 状态 |
|------|------|------|
| M0（现在） | kind 集群 + 本机 cloudcore（healthz）+ edgecore 占位 | ✅ 脚本已就绪，`--edge` 部分待 M1 |
| M1 | + edgecore 真实连接云端（注册/心跳/重连） | 脚本 `--edge` 完整启用 |
| M2 | + 应用下发/自治测试（断网 = 停容器/杀进程） | 可直接复用 |
| M4 | cloudcore 可部署进 kind 集群（Helm，WBS 8.5） | 新增 `hack/dev-up.sh --helm`（待开发） |

---

## 2. 前置依赖

| 依赖 | 版本 | 验收命令 | 说明 |
|------|------|----------|------|
| Docker Desktop | ≥ 29.x | `docker info` | kind 的运行时；须已启动 |
| kind | ≥ 0.20（建议最新） | `kind version` | 本地 K8s 集群 |
| kubectl | 与集群版本接近即可 | `kubectl version --client` | 操作集群 |
| Go | ≥ 1.26 | `go version` | 构建二进制 |
| make | 3.81 | `make --version` | 构建入口 |
| curl | — | `curl --version` | 健康检查（脚本用） |

本机现状（2026-08-13）：Docker 29.4.3 ✅、Go 1.26.2 ✅、make ✅、curl ✅；**kind 与 kubectl 未安装（见 §3.1）**。

---

## 3. 安装步骤

### 3.1 安装 kind 与 kubectl（一次性）

```bash
brew install kind kubectl

# 验收
kind version          # kind v0.x.x go1.2x.x darwin/arm64
kubectl version --client
```

> 若 brew 安装慢：确认 `docs/ENV-SETUP.md §4.1` 的代理配置；kubectl 也可用 `brew install kubernetes-cli`（等价）。

### 3.2 创建 kind 集群

```bash
# 方式一：一键脚本（推荐，幂等，可重复执行）
./hack/dev-up.sh

# 方式二：手动
kind create cluster --name edgeflow-dev
kubectl config use-context kind-edgeflow-dev
kubectl get nodes        # 期望：NAME=edgeflow-dev-control-plane, STATUS=Ready
```

> 集群名固定为 `edgeflow-dev`（可用环境变量 `EDGEFLOW_CLUSTER_NAME` 覆盖，脚本与文档保持一致）。
> 集群创建后 kubectl 自动生成 context：`kind-edgeflow-dev`。

### 3.3 边缘节点模拟（M1 起有效，两种方式）

**方式 A：本机直接跑 edgecore（最简单，推荐日常开发）**

```bash
./bin/edgecore                              # 本机 Mac 二进制
# M1 后支持连接参数，例如：
# ./bin/edgecore --cloudhub ws://127.0.0.1:10000 --node-id edge-001
```

优点：日志直接看、可用 dlv 断点调试（docs/ENV-SETUP.md §3.4）。缺点：模拟多节点时要开多个进程（每个配不同 `--node-id`）。

**方式 B：Docker 容器跑 edgecore（模拟独立边缘节点，推荐多节点/断网测试）**

```bash
./hack/dev-up.sh --edge          # 启动 EDGEFLOW_EDGE_NODES 个容器（默认 1 个）
```

脚本会自动：① 交叉编译 Linux 版 edgecore（`bin/edgecore-linux-<arch>`，Mac 二进制无法进 Linux 容器）；② 用 `alpine` 镜像跑容器；③ 容器内 edgecore 通过 `host.docker.internal:10000` 连接本机 cloudcore。

| 场景 | 操作 | 效果 |
|------|------|------|
| 断网自治测试 | `docker stop edgeflow-edge-1` | 模拟边缘断网（M2 起验证容器持续运行） |
| 恢复网络 | `docker start edgeflow-edge-1` | 模拟重连与同步 |
| 多节点 | `EDGEFLOW_EDGE_NODES=3 ./hack/dev-up.sh --edge` | 3 个模拟边缘节点 |
| 清理 | `./hack/dev-down.sh` | 全部拆除 |

> M0 阶段注意：edgecore 目前是占位程序（打印版本后退出），容器会启动后立即退出——**这是预期行为**，M1 实现 EdgeHub 后容器将保持运行。

**方式 C（未来）**：真实边缘设备（树莓派等）加入——M5 规模化阶段（keadm，WBS 8.6），本方案不覆盖。

### 3.4 启动 cloudcore（本机进程）

```bash
# 前台（推荐日常开发，Ctrl+C 优雅退出）
./bin/cloudcore

# 或由脚本后台托管（写 PID 到临时目录，dev-down 统一清理）
./hack/dev-up.sh --cloud

# 验证
curl -s http://127.0.0.1:8080/healthz     # 期望 {"status":"ok",...}
```

> cloudcore 通过 kubeconfig 访问 kind 集群（M1 起控制器需要）；默认 kubeconfig 即 `~/.kube/config`，无需额外配置。

### 3.5 完整验证清单（M1 后）

```bash
kubectl --context kind-edgeflow-dev get nodes      # 边缘节点注册成功、Ready
kubectl --context kind-edgeflow-dev get crd        # CRD 可创建（M0-2 起）
curl -s http://127.0.0.1:8080/healthz              # cloudcore 存活
docker ps                                          # edgeflow-edge-N 容器 Running
# 断网自治（M2）：docker stop edgeflow-edge-1 → 观察容器内应用持续运行 → docker start → 120s 内状态同步
```

---

## 4. 一键脚本（hack/）

### 4.1 hack/dev-up.sh —— 启动

幂等：重复执行不报错、不重复创建。参数：

| 参数/环境变量 | 默认 | 说明 |
|--------------|------|------|
| （无参数） | — | 仅 preflight + kind 集群 + 构建二进制 |
| `--cloud` | 关 | 后台启动本机 cloudcore（PID 文件在临时目录，dev-down 清理） |
| `--edge` | 关 | 拉起 N 个边缘节点模拟容器（M1 起有效） |
| `EDGEFLOW_CLUSTER_NAME` | `edgeflow-dev` | kind 集群名 |
| `EDGEFLOW_EDGE_NODES` | `1` | 边缘模拟容器数量 |
| `EDGEFLOW_CLOUDCORE_PORT` | `8080` | cloudcore healthz 端口 |
| `EDGEFLOW_EDGE_IMAGE` | `alpine:3.20` | 边缘容器镜像（若 edgecore 需要 CGO 可换 `golang:1.26`） |
| `EDGEFLOW_EDGE_ARGS` | 空 | 追加给容器内 edgecore 的参数（M1 后传 `--cloudhub ...`） |

典型用法：

```bash
./hack/dev-up.sh                       # M0：集群 + 构建
./hack/dev-up.sh --cloud               # + 后台跑 cloudcore
./hack/dev-up.sh --cloud --edge        # M1 后：完整开发环境
EDGEFLOW_EDGE_NODES=3 ./hack/dev-up.sh --edge   # 3 个边缘节点
```

### 4.2 hack/dev-down.sh —— 清理

幂等：按"反向顺序"拆除，不存在的东西跳过：

1. 停止脚本托管的 cloudcore（先 SIGTERM 优雅退出，5s 内不退则 SIGKILL）；
2. 删除边缘模拟容器（`edgeflow-edge-*`）；
3. 删除 kind 集群（`kind delete cluster`）；
4. 清理脚本运行时目录（仅限临时目录，安全校验）。

```bash
./hack/dev-down.sh            # 全部拆除
./hack/dev-down.sh --keep-cluster   # 保留 kind 集群（只想停进程/容器时）
```

> 两个脚本都做了防护：只操作 `edgeflow-` 前缀的资源、只删自己创建的 PID/日志、破坏性命令前有提示。详见脚本内注释。

---

## 5. 端口规划

| 端口 | 用途 | 占用方 | 冲突排查命令 |
|------|------|--------|-------------|
| 8080 | cloudcore /healthz（管理） | 本机 cloudcore | `lsof -i :8080` |
| 10000 | 云边 WebSocket（M1 起） | 本机 cloudcore | `lsof -i :10000` |
| 6443 | kind apiserver（映射到本机） | Docker | `lsof -i :6443` |
| 随机 | kind 各组件 | Docker | `docker ps` |

---

## 6. 常见问题（FAQ）

### 6.1 kind 起不来 / 报错

| 现象 | 原因 | 解决 |
|------|------|------|
| `Cannot connect to the Docker daemon` | Docker Desktop 未启动 | 启动 Docker Desktop，等鲸鱼图标稳定后重试 |
| `port 6443 is already in use` | 6443 被其他程序占用 | `lsof -i :6443` 找到进程停掉；或改 kind 映射端口（`kind create cluster --config` 指定 `networking.apiServerPort`） |
| 创建卡在 `Creating cluster...` 很久 | 拉取 kindest/node 镜像慢 | 见 §6.2 |
| 创建一半失败、状态残留 | 上次失败未清理 | `kind delete cluster --name edgeflow-dev` 后重跑 dev-up |

### 6.2 镜像拉取慢 / 拉取失败

kind 的节点镜像（`kindest/node:*`）托管在 Docker Hub，国内网络可能很慢：

1. **配置 Docker 镜像加速器**：Docker Desktop → Settings → Docker Engine，在 `registry-mirrors` 中加入国内镜像源（如 `https://docker.m.daocloud.io`、`https://dockerproxy.com` 等，按当前可用源选），Apply & Restart。
2. **手动预拉取**：
   ```bash
   docker pull kindest/node:v1.31.0
   kind create cluster --name edgeflow-dev --image kindest/node:v1.31.0
   ```
3. 仍失败：走代理（ENV-SETUP.md §4.1 同款配置：`HTTPS_PROXY` 设置后重启 Docker Desktop）。

> 边缘模拟容器镜像（alpine）很小，一般不受影响；如慢同样走加速器。

### 6.3 端口冲突

| 现象 | 解决 |
|------|------|
| 8080 被占用（另一个 cloudcore 或别的服务） | `lsof -i :8080` 确认后停掉；或换端口：`./bin/cloudcore --port 9090` / `EDGEFLOW_CLOUDCORE_PORT=9090` / 改 `config/cloudcore.json`（优先级：命令行 > 环境变量 > 文件，见 ARCHITECTURE.md §5） |
| 10000 被占用（M1 起） | 同上模式；云边端口将来进配置（`edgecore.json` 的 `cloudhub.url` 与云端对应，改时两边一致） |
| 6443 被占用 | 见 §6.1 |

### 6.4 kubectl 连不上 / context 混乱

```bash
kubectl config get-contexts          # 查看所有 context
kubectl config use-context kind-edgeflow-dev   # 切到 kind 集群
kubectl cluster-info                 # 验证连通
```

> 注意：`kubectl get nodes` 在 M0 阶段只显示 kind 的 control-plane 节点（模拟云端 K8s 自身）；**边缘节点要 M1 注册功能完成后才会出现**（这是预期）。

### 6.5 edgecore 容器启动后立即退出

M0 阶段正常：edgecore 是占位程序（打印版本后退出）。判断方法：

```bash
docker logs edgeflow-edge-1     # 应看到 edgecore starting / exited (skeleton build)
```

M1 实现 EdgeHub 后，`./hack/dev-up.sh --edge` 拉起的容器将保持 Running。若 M1 后仍退出：`docker logs` 查看连接报错（常见：cloudcore 未启动 / 端口不对 / Token 未配置）。

### 6.6 想完全重置开发环境

```bash
./hack/dev-down.sh          # 拆干净（集群 + 进程 + 容器）
./hack/dev-up.sh --cloud    # 重新拉起
```

---

## 7. 与 ROADMAP 的关系

| ROADMAP 条目 | 本方案覆盖 |
|-------------|-----------|
| WBS 1.3 开发环境（本地 K8s 集群、边缘节点模拟） | 全文 |
| M0 验收"CI PR 反馈 ≤10min" | kind 集群用于本地快速验证（CI 本身在 GitHub Actions，见 .github/workflows/ci.yml） |
| WBS 8.4 性能测试（100 节点模拟） | 本方案容器化模拟是基础；100 节点规模需单独脚本（M4 扩展） |
| ROADMAP 缺口 10（1.3 工具链未细化） | 本文档即该缺口的解决方案 |
| M2 Flannel（缺口 6） | 边缘容器网络在容器模拟场景下由 Docker 网络承担；真机场景再引入 Flannel（⚠️ 归属待确认） |

---

*本文档为 WBS 1.3 交付物草案；脚本（hack/dev-up.sh、hack/dev-down.sh）与本文档同步维护。M1 云边通信打通后，按 §3.5 清单做一次完整验证并更新本文档状态。*
