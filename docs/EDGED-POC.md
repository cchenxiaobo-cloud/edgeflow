# Edged 方案 A POC 报告（WBS 3.2 / P0 决策项）

> 日期：2026-08-13 ｜ 范围：`edge/pkg/edged/` ｜ 决策输入：ROADMAP §2.1 P0 前置（方案 A/B/C 定案）
> 对应提交：`feat(edged): POC of plan-A container lifecycle management`

## 1. 结论

**方案 A（自研精简 kubelet：轻量容器运行时管理 + Pod 生命周期）可行，且建议按方案 A 继续 M2。**

可行性判断：**有条件可行（条件明确，均可控）**。

| 维度 | POC 验证结果 | 判断 |
|------|-------------|------|
| 核心机制（声明式 reconcile 状态机） | 增删收敛、幂等、故障重试全部通过单测 | ✅ 成立 |
| 容器运行时抽象 + 双实现 | Mock 与 Docker CLI 实现同一接口，行为一致 | ✅ 成立 |
| 真实容器生命周期 | Docker 冒烟：创建→幂等→标签发现→删除全流程通过 | ✅ 成立 |
| 运行时不可用/超时处理 | daemon 不可用返回 `docker daemon unavailable:` 前缀错误；单命令 30s 超时 | ✅ 成立 |
| 与现有模块衔接 | 直接消费 metamanager.Store.ListPods()，零新依赖 | ✅ 成立 |
| 多副本（Replicas 展开） | 副本命名/补齐/缩容收敛通过单测 + 真实 Docker 冒烟 | ✅ M2 完整化已实现（§9） |
| 健康检查自愈 | 停止/未知/缺失副本自动重启 + RestartCount 计数（无退避） | ✅ M2 完整化已实现（§9） |
| 生产级细节（CRI、健康检查退避、镜像管理） | 未在 POC/本次范围 | ⚠️ M2 补齐（见 §4/§5） |

方案 A 的核心风险点（自研运行时的"最后一公里"：CRI 接入、故障自愈细节）不构成方案否决项，
因为运行时抽象已把"状态机/调谐逻辑"与"真实运行时"解耦——后续即使从 Docker CLI 换 containerd CRI，
`edged.go` 的调谐核心零改动，只需新增一个 `ContainerRuntime` 实现。

## 2. 状态机设计

```
                 EnsureRunning(不存在)
   StateAbsent ───────────────────────────► StateRunning
       ▲                                          │
       │ EnsureStopped(rm -f)                     │ 容器退出/被外部停止
       │                                          ▼
       └──────────────────────────────────── StateStopped
                       EnsureRunning(docker start)
```

- 四态枚举：`Absent / Running / Stopped / Unknown`（`Unknown` 用于运行时不可用、输出解析失败等不可判定场景）。
- 状态机位于 **reconciler**（`edged.go`），而非运行时层：每轮调谐读取期望集合（MetaManager）与
  实际集合（`rt.List()`，副本粒度），对差异执行 `EnsureRunning`（期望有）或 `EnsureStopped`（期望无，孤儿清理）。
- **多副本（WBS 6.4）**：每个 Pod 的期望副本数为 `Replicas`（缺省 1），容器命名
  `edgeflow-<ns>-<name>-<index>`；reconciler 按副本粒度收敛：实际 > 期望 → 从最大 index 停止多余副本，
  实际 < 期望 → 补齐创建；**健康检查自愈（WBS 6.5）**：期望副本非 Running 且无错误 → 重启并累加
  RestartCount（无退避，M2 加 CrashLoopBackOff，见 §9）。
- **幂等是状态机的基石**：`EnsureRunning` 对已运行 no-op、停止态仅 `start` 不重建；`EnsureStopped` 对
  不存在 no-op；`List` 按 `label=edgeflow.pod` 过滤，只管理自己的容器。
- 启动即调谐一次 + 周期调谐（默认 5s）；`Stop()` 可随时退出循环（edgecore 装配/测试需要）。
- 每轮结果记录 `podKey → {State, Err, LastReconcile, RestartCount}`，经 `Status()` 暴露，为 WBS 6.3 Pod 状态上报预留。

## 3. 验证了什么 / 没验证什么

### 3.1 已验证（本 POC 实际执行）

1. **状态机单测（Mock 运行时）**：`go test -race -cover ./edge/pkg/edged/` → **PASS，覆盖率 85.9%**
   - 新增 Pod → 调谐创建并 Running；删除 Pod → 调谐清理并 Absent；
   - 停止态容器 → 调谐 `start` 恢复（节点重启场景）；
   - 注入运行时故障 → 不 panic、状态记录错误、故障恢复后重试成功；
   - 连续两次调谐不重复创建（幂等）；`List` 失败不中断本轮；
   - `Start()/Stop()` 生命周期：Stop 后循环停止、重复 Stop no-op、Stop 后可重启、重复 Start no-op；
   - MockRuntime 并发（8 goroutine × 100 次）`-race` 无告警。
2. **真实 Docker 冒烟**：`EDGED_DOCKER_SMOKE=1 go test -run TestDockerRuntimeSmoke` → **PASS**
   （本机 Docker daemon 就绪；busybox:1.37.0 本机缓存镜像）
   - `EnsureRunning` 创建容器（`docker run -d --name edgeflow-default-smoke-xxx --label edgeflow.pod=... --label edgeflow.namespace=...`）；
   - 幂等重试不产生第二个容器；`List()` 按标签发现容器且 namespace/name 反解正确；
   - `EnsureStopped`（`docker rm -f`）后状态收敛 Absent；重复删除 no-op；`docker ps -a` 无残留。
   - 说明：busybox 默认命令立即退出，容器创建后状态为 stopped——这恰好验证了
     "存在性判断"与"stopped 收敛路径"（第二轮 EnsureRunning 会走 docker start）。
3. **静态检查**：`go build`、`go vet`、`golangci-lint run ./edge/pkg/edged/...`（standard 组）→ **0 issues**；
   全仓库 `go build ./...` + `go test ./...` 全绿。
4. **依赖克制**：新增代码 **零新 Go 依赖**（DockerRuntime 通过 `os/exec` 调 docker CLI；日志复用 `pkg/log`）。

### 3.2 未验证（POC 边界，M2 补）

| 未验证项 | 说明 | M2 计划 |
|---------|------|---------|
| 真实 containerd CRI | 本机仅 Docker Desktop；Docker CLI 路径与 CRI 语义有差异（如无 sandbox/Pod 概念） | 接入 containerd 客户端（grpc，新增依赖）实现同一接口 |
| 资源占用实测 | 未做容器内存/CPU/节点规模压测 | WBS 8.4 性能测试 |
| 健康检查退避 | 状态级自愈已实现（重启无退避）；容器内进程健康检查（liveness）未实现 | WBS 6.3/6.5 完整版：CrashLoopBackOff + 探针 |
| 镜像拉取 | 依赖 docker run 自动拉取；未做拉取策略/进度/GC | 镜像管理子项 |
| 多副本滚动更新 | 副本数收敛已实现；镜像更新（滚动策略）未做 | WBS 6.4 后续：滚动更新 |
| 网络/存储 | 未处理 Pod 网络（CNI）、volume 挂载 | M2 依赖项（Flannel 缺口，见 ROADMAP §7 缺口 6） |

## 4. 工作量修正建议

ROADMAP WBS 3.2 原估 **30 人天**。POC 表明核心机制（状态机 + 运行时抽象 + 双实现 + reconcile）已就绪
（约 2 人天产出，含单测与冒烟），剩余为生产化补齐。修正估算：

| 子项 | 估算（人天） | 说明 |
|------|------------|------|
| 核心机制 | ✅ POC 已完成 | 无需重做 |
| containerd CRI 实现（替换/并存 Docker CLI） | 5~8 | grpc 客户端 + Pod 语义映射；有 CRI 经验可取下限 |
| 健康检查 + 重启策略（含退避） | 3~5 | liveness/readiness、CrashLoopBackOff |
| Pod 状态上报（WBS 6.3） | 2~3 | Status() 已有数据，接 EdgeHub 上行 |
| 镜像管理（拉取策略/GC/进度） | 3~5 | |
| 多副本/升级（WBS 6.1/6.4） | 4~6 | Replicas 展开、滚动更新 |
| 集成装配 + 云边联调（WBS 8.2） | 3~5 | 接 main.go、真实消息链路 |
| **合计（修正后）** | **20~32** | 中值 ~26，比原估略低；若保留 Docker CLI 生产化（非 CRI）可再省 5~8 |

**修正结论**：原 30 人天估算基本合理（±18 人天区间内），核心机制风险已消除，
剩余工作量集中在"生产化细节"而非"自研路线本身"。ROADMAP 3.2 可维持原排期，任务拆解按上表。

## 5. 风险清单

| # | 风险 | 等级 | 缓解 |
|---|------|------|------|
| 1 | **Docker CLI 依赖**：边缘环境不一定有 docker（生产更可能是 containerd/CRI） | 高 | 抽象层已隔离；M2 实现 CRI 版本；POC 期间 Docker CLI 仅作验证载体 |
| 2 | **镜像拉取慢/超时**：docker run 首次拉取可能 >30s 命令超时；超时后 daemon 侧可能仍在创建 | 中 | 超时后下一轮 reconcile 自动收敛（Inspect 发现已存在即 no-op）；M2 做拉取策略与镜像仓库镜像 |
| 3 | **容器命名冲突**：与其他工具/多节点共用 docker daemon 时名字被占用 | 中 | 命名规范（含副本序号）+ `edgeflow.pod/namespace` 标签隔离；run 冲突时重新 Inspect 兜底 |
| 4 | **无进程级健康检查**：容器内进程僵死但容器未退出时无法发现（状态级自愈已做） | 中 | M2 liveness 探针（依赖 WBS 6.3） |
| 5 | **容器崩溃循环重启**：状态级自愈会不停重启退出型镜像（如 busybox 一次性任务），且无退避 | 中 | M2 重启策略（参照 K8s RestartPolicy + 指数退避）；一次性任务镜像建议 RestartPolicy=Never |
| 6 | **本地多实例互踩**：两个 edgecore 进程同机运行会争抢同名容器 | 低 | POC 不做；部署约定单实例/节点；M2 可用 nodeID 后缀 |
| 7 | **旧命名容器迁移**：升级前遗留的 edgeflow-<ns>-<name>（无副本序号）容器不会被新逻辑清理 | 低 | 开发环境一次性 `docker rm -f` 清理；List 对无序号名兜底按副本 0 解析，不影响新容器 |

## 6. 建议

**继续方案 A**，依据：
1. 核心机制 POC 全绿，抽象层已把"路线风险"降为"实现工作量"，不存在颠覆性障碍；
2. 依赖克制原则下，自研状态机 ~500 行可维护，远轻于引入 kubelet（方案 B 的维护/升级成本与
   本项目"从零自研"定位不符，且 kubelet 依赖体系与本仓库依赖原则冲突）；
3. 订阅接口（metamanager.Subscribe）已就绪，事件驱动改造工作量小（见 §7）。

**转向方案 B 的触发条件**（当前均未出现）：
- 边缘场景出现多 Pod 复杂编排需求（sidecar、亲和性、资源 QoS），自研成本显著上升；
- CRI 接入后发现与 containerd 版本兼容性维护成本失控；
- M2 联调阶段 Pod 状态上报与云端 K8s 语义对齐工作量超过方案 B 增量。

## 7. 后续接入点（MetaManager 增量订阅）

- 当前：**轮询 reconcile**（默认 5s），已满足断网自治验收（断网 30min 容器持续运行），不依赖订阅；
- 订阅已由并行 Agent 合入（`metamanager.Subscribe/Unsubscribe`，commit 089c358）：
  - 接入点：`Edged` 增加 `Notify()`（触发一次 `reconcileOnce`），在 `Subscribe` 回调中调用，
    实现"落盘即调谐"的事件驱动；轮询保留为兜底（慢消费者丢事件由 ListPods 全量对账覆盖）；
  - `Store.Close()` 会关闭订阅通道，Edged 需监听通道关闭以退出（edgecore 停机路径）；
  - 本 POC 刻意未接入：保持核心不依赖订阅，集成装配（cmd/edgecore/main.go）时一并完成。

## 8. 复现方式

```bash
# 单测（Mock 状态机 + DockerRuntime 假执行器，无需 docker）
go test -race -cover ./edge/pkg/edged/

# 真实 Docker 冒烟（需要本机 docker daemon；busybox:1.37.0 已缓存）
EDGED_DOCKER_SMOKE=1 go test -run TestDockerRuntimeSmoke -v ./edge/pkg/edged/

# 端到端冒烟（真实 Docker：多副本 + 健康检查自愈，需要 nginx:1.25-alpine 镜像）
go run ./hack/edged-smoke

# 静态检查
go vet ./edge/pkg/edged/ && golangci-lint run ./edge/pkg/edged/...
```

## 9. M2 完整化：多副本与健康检查自愈（WBS 6.4/6.5）

> 日期：2026-08-14 ｜ 对应提交：`feat(edged): replicas support and health-check self-healing (WBS 6.4/6.5)`

### 9.1 实现内容

**1. 多副本（Replicas 展开，WBS 6.4）**

- 容器命名规范升级：`edgeflow-<namespace>-<name>-<index>`，index = 0..Replicas-1
  （单副本缺省 1，恒为 `-0`）；超长名截断只作用于基础名，副本序号恒在末尾，
  保证 List 能反解 index；
- `ContainerRuntime` 接口改为按副本粒度操作：`EnsureRunning/EnsureStopped/Inspect`
  增加 `index int` 参数，`List()` 返回 `[]InstanceRef{Pod, Index}`；
- reconcile 每轮对每个期望 Pod：
  - **收缩**：实际副本数 > 期望 → 从最大 index 开始 `EnsureStopped` 多余副本
    （缩容后剩余恒为 0..Replicas-1 连续前缀）；
  - **补齐**：实际副本数 < 期望 → 从 index=实际数 开始补齐创建；
  - 副本数收敛不依赖运行时额外能力，全部由 reconcile 状态机驱动，幂等。

**2. 健康检查自愈（WBS 6.5）**

- 每轮对每个期望副本 `Inspect`：非 Running 且无错误（Stopped/Unknown/Absent）
  → 视为运行异常 → `EnsureRunning` 重启（幂等：Stopped 走 start、Absent 走创建，
  缺失副本缺口也由此兜底补齐）；
- `PodStatus` 新增 `RestartCount` 字段：每成功重启一次累加 1（供 M2 退避策略输入）；
- 重启无退避（POC 简化）：每次调谐重试，M2 完整版按 RestartCount 加
  CrashLoopBackOff 指数退避；
- 日志：`健康检查：容器 <名称> 已重启`。

**3. 配套改动**

- MockRuntime 状态表按 `instanceKey（<ns>/<name>#<index>）` 区分副本，新增
  `SetState/DeleteState` 测试注入接口；
- DockerRuntime `List` 从容器名末段反解副本序号（无序号旧命名兜底 0）；
- 孤儿清理/状态表清理（P2-1）按副本实例粒度适配新命名。

### 9.2 验证结果

- **单测**：`go test -race -cover ./edge/pkg/edged/` → PASS，覆盖率 **89.2%**（原 85.9%）；
  新增/更新用例：
  - replicas=3 → 3 副本容器全部创建，幂等轮不重复创建；
  - replicas 3→1 → 停止多余副本（副本 0 保持），List 只剩 1 实例；
  - Replicas 缺省（0）→ 单副本；
  - 副本被外部停止 → 健康检查重启 + RestartCount 累加，多副本下只重启异常副本；
  - 副本被外部删除（缺口）→ 补齐重建；
  - 原 17 个用例全部保持通过（命名变更同步更新）；
- **静态检查**：`go build ./...`、`go vet`、`golangci-lint`（standard 组）→ 0 issues；
- **端到端冒烟（真实 Docker）**：`go run ./hack/edged-smoke` → SMOKE PASS：
  - replicas=2 落盘 → `docker ps` 可见 `edgeflow-default-smoke-nginx-0`/`-1`；
  - 外部 `docker stop` 副本 1 → 1s 内自动重启（RestartCount=1）；
  - 删除期望 Pod → 全部副本容器清理，无残留；
  - `EDGED_DOCKER_SMOKE=1 go test -run TestDockerRuntimeSmoke` → PASS。

### 9.3 缺口与下一步（M2 完整版）

| 缺口 | 说明 | 下一步 |
|------|------|--------|
| 重启退避 | 当前每轮调谐都重试，退出型镜像（如 busybox）会被反复拉起 | CrashLoopBackOff 指数退避（按 RestartCount 与时间窗） |
| 镜像更新滚动 | 副本数收敛已做，镜像变更（如 nginx:1.25→1.26）仍是全量重建语义 | WBS 6.4 滚动更新策略（先建后删/分批） |
| 进程级健康检查 | 状态级（容器退出/停止）可自愈，容器内进程僵死（liveness）无法发现 | 探针（依赖 WBS 6.3） |
| 旧命名容器迁移 | 升级前遗留的无序号容器不会被清理 | 开发环境一次性 `docker rm -f`；生产以部署新装为准 |
| 状态上报扩展 | RestartCount 已记录在 PodStatus，未进上报负载（契约字段不可改） | 云端契约扩字段后同步 |
