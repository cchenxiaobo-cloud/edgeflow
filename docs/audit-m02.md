# EdgeFlow M0-M2 里程碑收尾审计（audit-m02）

> **审计人**：收尾核对员（子代理）
> **日期**：2026-08-14 20:30 (Asia/Shanghai)
> **范围**：M0-M2 里程碑（WBS 1、2、3、4、6 一二级任务 + M0/M1/M2 验收标准 + 特别核对项）
> **性质**：只读收尾检查，未改动任何代码/文档
> **基线**：docs/ROADMAP.md（WBS 清单与验收标准）、docs/PROGRESS.md（§3 模块表/§4 验证记录/§5 待办/§6 里程碑）
> **证据**：git log（108 commits，工作区干净）、代码目录存在性、`go test -race` 抽查（20 包全绿，总覆盖率 77.8%）、`helm lint`（0 failed）、docs/ 各审查报告与交付报告

---

## 0. 审计方法

1. 从 ROADMAP.md 提取 M0-M2 覆盖任务清单（WBS 1/2/3/4/6 一二级 + 里程碑验收标准）。
2. 逐项核对实际状态：git log 提交证据 → 代码/产物存在性 → 测试抽查 → 审查/交付报告交叉验证。
3. 特别核对 4 项：WBS 2.8 NodeJob、WBS 4.5 TLS 归属、WBS 6.4 滚动策略、WBS 8.3 E2E 自治 30min。
4. 结论分级：✅ 完成（附证据）｜🟨 部分（缺什么）｜⬜ 未做｜⚠️ 归属变更/文档滞后。

**抽测结果（2026-08-14 实跑）**：`go test -race -count=1` 覆盖 pkg/protocol、log、config、httpx、version、apis、cloud/×5、edge/×6、cmd/×3 全部 ok；总覆盖率 77.8%（门槛 ≥70% ✅）；`helm lint build/charts/edgeflow` 0 failed；git 工作区干净。

---

## 1. 核对表（任务 / 声明状态 / 实际状态 / 证据 / 结论）

### 1.1 M0 —— WBS 1 基础架构（声明 ✅ 完成）

| # | 任务 | 声明 | 实际 | 证据 | 结论 |
|---|------|------|------|------|------|
| 1 | 1.1 项目骨架（go.mod/Makefile/占位程序/README/git） | ✅ | 完整 | commit `4093fe3`；Makefile 含 build/test/run/lint/cross-build | ✅ |
| 2 | 1.2 CI/CD 流水线 | ✅ 基础版 | ci.yml（lint 强制 + vet + build + test-race + 覆盖率 ≥70%）在；**多架构构建在 M4 才补**（release.yml，commit `301daee`） | `.github/workflows/ci.yml`、`release.yml` | 🟨 基础完成，多架构部分延至 M4 |
| 3 | 1.3 开发环境 | ✅ | dev-up/dev-down 脚本 + DEV-ENV.md；无真实 K8s 集群（kind 未实际启用），边缘节点用 edgecore 模拟 | `hack/dev-up.sh`、`docs/DEV-ENV.md` | ✅（模拟方案，已文档化） |
| 4 | 1.4 API 规范/CRD 定义 | ✅（PROGRESS M0-2） | apis/edge/v1alpha1 三类型（EdgeNode/DeviceModel/Device）+ DeepCopy + 11 测试 | commit `a541128`；`go test ./apis/...` ✅ | 🟨 **缺 CRD manifest（yaml）**：无任何可 `kubectl apply` 的 CRD 清单（全仓搜无），无 OpenAPI schema |
| 5 | 1.5 共享库（log/config/version/httpx） | ✅ | 四包齐全，M0 时 100% 覆盖，至今测试全绿 | commits `98a50a6`/`f49ce32` | ✅ |
| 6 | 1.6 构建与发布脚本 | ✅ | Makefile cross-build（linux amd64/arm64）+ release.yml + M5 实际发布产物 | `Makefile`、`release/v0.1.0/` | ✅ |
| 7 | 1.7 代码质量基建 | ✅ | .golangci.yml v2 + CI 强制 lint | `.golangci.yml`、ci.yml | ✅ |
| 8 | 9.1 架构文档 | ⬜→✅（ROADMAP 声明 M0 交付） | ARCHITECTURE.md 存在，但标注 **v0.1 草案待评审**、无评审记录、内容滞后（见 §3 过时项 11-13） | `docs/ARCHITECTURE.md` L4-7 | 🟨 有文档，**评审验收未满足** |
| 9 | M0 验收：make build 本地 + CI 通过 | ✅ | 本地 ✅；**CI 从未在 GitHub 运行**（远程仓库未关联，PROGRESS §5 P1） | PROGRESS §5 P1；git remote 无 origin | 🟨 本地通过，CI 未实证 |
| 10 | M0 验收：首个 CRD 可 kubectl apply | ✅（声明） | **未达成**：无 CRD manifest，CRD 从未 apply 到任何集群 | 全仓无 CRD yaml（REVIEWS.md R9.2-3 亦确认"CRD 尚未接入 K8s"） | ⬜ 未做（验收被 REST API 化适配替代） |
| 11 | M0 验收：CI PR 反馈 ≤10min | ✅（声明） | **无法验证**：CI 从未运行 | 同 #9 | ⬜ 未验证 |
| 12 | M0 验收：Helm Chart 骨架 + helm lint | ✅ | 骨架 commit `9d78246`，M4 完整化 `3bb60ce`；helm lint 实测 0 failed | `build/charts/edgeflow/` | ✅ |

### 1.2 M1 —— WBS 2/3/4 云边核心链路（声明 ✅ 一~三期完成）

| # | 任务 | 声明 | 实际 | 证据 | 结论 |
|---|------|------|------|------|------|
| 13 | 4.1 消息协议 | ✅ | pkg/protocol JSON 信封 + 类型枚举（含 NodeJob 占位"待定"） | commit `e569ea1`；message.go L24-25 | ✅（NodeJob 见 §2.1） |
| 14 | 4.2 连接管理 | ✅ | 注册/心跳 30s/指数退避（2/4/8s）/自动重连 | `6241a78`/`7b1c27a`；edgehub/client.go L10 | ✅ |
| 15 | 4.3 消息路由 | ✅ | SendToNode/Broadcast/Deliver | commit `081eb0f`（WBS 4.3） | ✅ |
| 16 | 4.4 压缩与序列化 | ✅（M1 范围） | 序列化=JSON ✅；**压缩未实现**（ARCHITECTURE 计划 gzip M2+、Protobuf M4/规模化，计划内延后但无待办跟踪）；增量同步部分实现（metamanager 增量订阅） | ARCHITECTURE L263/L314-315；commit `089c358` | 🟨 计划内延后，缺显式排期跟踪 |
| 17 | 4.5 安全传输 | ✅（M1 含"4.5 基础版"） | **M1 完全未做**（cloudhub 注释自认"M1 基础版不做来源校验"）；M4 直接实现**完整 mTLS**（commit `0a7fcc2`，WBS 7.1/7.4）。ARCHITECTURE 所称"M1 Token 过渡"**从未实现** | cloud/pkg/cloudhub/server.go L236；PROGRESS §4J | ⚠️ **归属变更：4.5 → M4 完成**；M1-M3 通道全程无认证 |
| 18 | 4.6 可靠投递 | ✅ | 云端 ReliableSend（pending+Ack+超时重试同 ID）+ 边缘自动 Ack + 幂等去重 + P2 收尾（cross-cleanup/502/原子化/op 校验） | `3197ad3`/`19dd66f`/`8321b0e` | ✅ |
| 19 | 2.1 CloudHub | ✅ | WS 服务端 + 会话 + 注册 + 心跳监控（monitorInterval=timeout/3）+ TLS Option | `6241a78` 起多轮 | ✅ |
| 20 | 2.3 EdgeController（简化） | ✅ | registry（NodeInfo）+ EdgeNode 映射（ToEdgeNode/ListEdgeNodes + GET /api/v1/edgenodes，K8s 风格 items） | `3c7b99d`/`641863e` | ✅（无真实 apiserver，REST 化适配） |
| 21 | 2.4 NodeController | ✅（M1 范围） | **M1 未实现**（仅 cloudhub 内嵌断线检测）；独立 NodeController 心跳超时扫描在 **M4** 完成（SIGSTOP 冻结→Offline→Ready 状态机闭环） | commit `f71684e`（M4 主体轮）；PROGRESS §4K | ⚠️ **归属变更：2.4 → M4 实现** |
| 22 | 2.6 云端元数据 | ✅ | 内存 registry + REST API（无 apiserver/etcd，已文档化为适配） | `3c7b99d`/HANDOFF | ✅（适配完成） |
| 23 | 3.1 EdgeHub | ✅ | WS 客户端 + 注册/心跳/重连/Ack + 自动 Ack 幂等 + wss | `7b1c27a`/`19dd66f`/`0a7fcc2` | ✅ |
| 24 | 3.3 MetaManager | ✅ | SQLite（WAL）KV + 节点信息 + Pod 持久化 + 增量订阅（缓冲满丢弃，声明式收敛兜底） | `3aaaf28`/`089c358` | ✅ |
| 25 | 3.2 Edged 基础版（POC） | ✅（M1 含 3.2 基础） | 方案 A POC：ContainerRuntime 接口 + Mock/Docker 双实现 + 声明式 reconcile + POC 报告 | commit `c9db4ba`；docs/EDGED-POC.md | ✅ |
| 26 | M1 验收：keadm join 注册 | ✅（声明） | **M1 未做**；keadm 基础 CLI（init/join/reset/version）在 **M4** 完成 | commit `2386fa3`（WBS 8.6） | ⚠️ **归属变更：8.6 基础 → M4** |
| 27 | M1 验收：kubectl get nodes Ready | ✅（声明） | **未按字面达成**（无真实 K8s）；适配为 REST `GET /api/v1/edgenodes`（Running/Offline 状态流转） | `641863e`；PROGRESS §4D | 🟨 验收适配，非字面达成 |
| 28 | M1 验收：心跳 ≤30s | ✅ | edgehub 每 30s 心跳，云端 3 次超时判离线 | edgehub/client.go L10；cloudhub/server.go | ✅ |
| 29 | M1 验收：断线 60s 内重连 | ✅ | 指数退避 2/4/8s；E2E：cloudcore 重启后 8s 内重连重新注册 | PROGRESS §4B | ✅ |
| 30 | M1 验收：MetaManager SQLite 持久化 | ✅ | 落盘 + 重启加载（db+wal+shm） | PROGRESS §4C/§4D | ✅ |
| 31 | M1 验收：单测 ≥70% | ✅ | 实测总覆盖率 77.8%（20 包 race 全绿） | 本次抽测 | ✅ |

### 1.3 M2 —— WBS 3.2 完整/3.4/6.x 应用部署与自治（声明 🟨 第 1-4 轮完成）

| # | 任务 | 声明 | 实际 | 证据 | 结论 |
|---|------|------|------|------|------|
| 32 | 3.2 Edged 完整 | 🟨→（M4A 后完成） | DockerRuntime + Replicas 补齐/收缩 + Inspect 健康自愈 + CrashLoopBackOff（3 次/30s 退避/60s 稳定重置）+ 旧命名迁移 | `c9db4ba`/`47d9e21`/`c885229` | ✅（containerd CRI 实现列为 P2 延后） |
| 33 | 3.4 边缘自治 | ✅ 基础 | 断网 40s 容器持续运行 + 本地调谐不受影响 + 恢复重连后上报恢复；**30min 时长未验证**（见 #40） | PROGRESS §4F；M2 第二轮交付报告 | ✅ 基础（时长验收见 8.3 缺口） |
| 34 | 6.1 边缘应用部署 | ✅ | PodSync 下发→订阅触发→Edged 调谐→真实 Docker 容器创建/删除 | `15366e7`/`9fe47c1`；PROGRESS §4E | ✅ |
| 35 | 6.2 配置下发 | ✅ | ConfigMap/Secret sync 链路（五态 + 可靠投递 + 落盘 configs/ns/name + 自动 Ack + Secret 日志脱敏） | commit `5403daa`（WBS 6.2）+ `f3f3df1` | ✅ |
| 36 | 6.3 状态上报 | ✅ | 30s 周期上报（env 可配）+ status map 清理 + Absent 终态保留 90s + 云端 Absent→Delete 收敛 | `707128f`/`bc58e40`/`52e5ff5` | ✅ |
| 37 | 6.4 边缘应用升级 | 🟨 | **健康自愈/重启有**；**镜像更新滚动策略无**：EnsureRunning 对"已运行"容器直接 no-op，不比对镜像——镜像漂移不触发重建；无回滚 | edge/pkg/edged/docker_runtime.go L143-146；PROGRESS §4I P2 待办"滚动更新"仍开 | 🟨 **滚动更新+回滚缺失**（P1 缺口，见 §2.3） |
| 38 | 6.5 资源管理 | 🟨 | Replicas 补齐/收缩 ✅；**调度/资源超卖未实现** | `47d9e21`（WBS 6.4/6.5） | 🟨 仅副本伸缩，调度/超卖未做 |
| 39 | M2 验收：kubectl apply 部署到边缘 | ✅（声明） | 适配为 `POST /api/v1/nodes/{id}/podsync`（200/404/502/504 五态），无真实 kubectl | `15366e7`；API-SPEC.md §6.1 | 🟨 验收适配，非字面达成 |
| 40 | M2 验收：断网 30min 容器持续运行 | 🟨 | **仅 40s 短时验证**，无 30min 时长用例 | PROGRESS §4F"停 cloudcore 40s" | 🟨 未按验收时长验证（并入 8.3） |
| 41 | M2 验收：恢复 120s 同步 | ✅ | cloudcore 重启→重连注册→上报恢复（E2E 实测） | PROGRESS §4F | ✅ |
| 42 | M2 验收：ConfigMap/Secret 下发 | ✅ | 见 #35 | | ✅ |
| 43 | M2 验收：E2E 自治全过（8.3） | 🟨 | **8.3 E2E 完整场景未做**（多节点/故障恢复/自治 30min 套件不存在）；M5 demo.sh 是演示脚本，非 8.3 场景套件 | PROGRESS §5 P1；M3 启动轮报告"8.3 E2E 完整场景已列入台账 P1 待办"；M4/M5 报告无 E2E 记录 | ⬜ **P1 缺口**（见 §2.4） |
| 44 | Flannel（M2 交付物，ROADMAP 缺口 6） | ⬜ | **从未实现**：边缘容器网络走 Docker bridge，无 CNI；ARCHITECTURE 标注"⬜ M2"且缺口 6 未处置 | ARCHITECTURE L100/L140；全仓无 flannel/cni 代码 | ⬜ 缺口 6 仍开放 |

---

## 2. 特别核对项

### 2.1 WBS 2.8 云端任务管理（NodeJob）—— ⬜ 未做
- 证据：pkg/protocol/message.go L24-25 仅消息类型**占位"待定"**；全仓无 NodeJob CRD/控制器/API；ARCHITECTURE L125/L278/L433 仍标注"归属待确认/待定"；ROADMAP §7 缺口 1 未关闭。
- 结论：**未实现**。ROADMAP 建议"M1 后启动、M4 前完成"未兑现；M4/M5 交付报告均无 NodeJob 记录。建议明确处置（关闭或排期）。

### 2.2 WBS 4.5 安全传输（TLS）—— ⚠️ 归属变更已发生，但 ROADMAP 未回写
- 实际：M1 无任何认证（cloudhub/server.go L236 注释自认）；**M4 一次到位 mTLS**（commit `0a7fcc2`，含 pkg/certs CA/签发/SAN、双向强制、TLS1.2+、私钥 0600、wss 归一化、拒绝路径验证）。
- 偏差：ROADMAP §3 M1 范围"4.5 基础版"未执行；ARCHITECTURE" M1 用 Token 过渡"从未实现（跳过 Token 直接 mTLS）。
- 建议：ROADMAP/ARCHITECTURE 回写"4.5 与 7.1/7.4 合并于 M4 交付"。

### 2.3 WBS 6.4 镜像更新滚动策略 —— 🟨 未真正实现（P1）
- 证据：`EnsureRunning` 三态处理——StateRunning → `return nil`（**无镜像比对**）；StateStopped → start；StateAbsent → run。即云端更新 Pod 镜像后，已运行容器**不会被重建**；策略性滚动（分批/健康门槛/回滚）不存在。
- 佐证：PROGRESS §4I P2 待办仍列"滚动更新"；M2 启动轮/第二轮报告"下一步 P1：镜像更新滚动策略"→ 后续 M4A 只做了 replicas + 自愈（commit 消息"WBS 6.4/6.5"），滚动策略未闭环。
- 结论：6.4 仅完成"健康自愈/重启"，**滚动更新 + 回滚为 M2 范围真实缺口**（若 M2 验收按 ROADMAP 字面，则 M2 未完全达标）。

### 2.4 WBS 8.3 E2E 完整场景（自治 30min）—— ⬜ 未做（P1）
- 证据：无 E2E 场景套件/脚本（仅 M5 演示 examples/demo.sh，覆盖 Demo 链路非 8.3 场景）；断网自治仅 40s 短测；M3 启动轮报告确认"已列入台账 P1 待办"；M4/M5 各交付报告 grep 无 E2E 场景记录。
- 结论：8.3 完整场景（多节点/故障恢复/自治 30min/升级）未交付，M2 验收"E2E 自治全过"未达成。

---

## 3. 过时记录清单（文档滞后项）

| # | 位置 | 过时内容 | 实际情况 |
|---|------|----------|----------|
| S1 | PROGRESS.md 头部 | "最后更新：2026-08-13 09:25" | 正文已记录到 08-14（§4E-§4M） |
| S2 | PROGRESS.md §6 里程碑表 | M1-M5 全部"⏳ 未开始" | 实际 M1-M5 均已推进，M5 已发布 v0.1.0（§3/§4M 自相矛盾） |
| S3 | PROGRESS.md §5 P1 行 | "M2 完整化（6.2/6.4/6.5）：ConfigMap/Secret 下发、健康检查/多副本、镜像更新滚动策略（8.3 E2E 完整场景）" | 6.2 ✅（5403daa）、健康检查/多副本 ✅（47d9e21）；**残留未做仅**：镜像更新滚动策略 + 8.3 E2E 完整场景，应拆分更新 |
| S4 | PROGRESS.md §5 P2 | "cmd-edgecore 覆盖率缺口（56.5%）（M2 前）" | 实测 **74.6%**，已达标，应关闭 |
| S5 | PROGRESS.md §5 P2 | "Helm Chart 骨架（M0-4）｜进入部署阶段前完成" | 已交付（9d78246 骨架 + 3bb60ce 完整化，helm lint 0 failed），应关闭 |
| S6 | PROGRESS.md §5 P2 | "edgecore 占位程序测试｜非核心包，M1 开发时补" | edgecore 已是真实常驻服务（cmd/edgecore 74.6%），条目前提已失效，应关闭 |
| S7 | ROADMAP.md §0 基线表 | "CRD 定义 / Helm 骨架 / 架构文档 ⬜ 未开始" | 全部完成（CRD 类型 a541128、Helm 9d78246、架构文档 92ede1b） |
| S8 | ROADMAP.md §1.2 状态列 | WBS 1.x 多数 🟨/⬜ | WBS 1 全部完成 |
| S9 | ROADMAP.md §3 里程碑表 | M1 范围含"4.5 基础版、8.6 基础、2.4"；M2 范围含 Flannel | 4.5/8.6/2.4 实际归属 M4；Flannel 未做且缺口 6 未处置，表格未回写 |
| S10 | ROADMAP.md §7 缺口 1/6 | 2.8 NodeJob 归属待确认；Flannel 无对应条目 | 均未处置（2.8 未做、Flannel 未做），且无结论回写 |
| S11 | ARCHITECTURE.md L4-7 | "v0.1 草案待评审""当前实现仅到 M0" | 实现已至 M5 发布，文档严重滞后且从未评审（9.1 验收未满足） |
| S12 | ARCHITECTURE.md L26 | 边缘消息总线"NATS（Leaf Node，⚠️ 待 POC）" | 实际 M3B 起用 **MQTT/mosquitto + paho**（2a0d0a3，EVENTBUS-GUIDE.md），NATS 方案已放弃，文档未更新 |
| S13 | ARCHITECTURE.md L270/L336 | "M1 用 Token 过渡""节点 Token 注入" | Token 方案从未实现（M1-M3 无认证，M4 直接 mTLS），文档未更新 |
| S14 | DEV-ENV.md | "当前 M0 阶段以下部分立即可用" | M0 已结束数月轮次，措辞过时（内容仍可用） |

---

## 4. 缺口清单（分级）

| 级别 | 缺口 | 说明 | 建议 |
|------|------|------|------|
| **P1** | 8.3 E2E 完整场景（含自治 30min 时长验证） | M2 验收"E2E 自治全过"未达成；仅有 40s 短测 | 补 E2E 场景套件或显式修订验收口径（如"自治 ≥30min 冒烟 + 场景化用例"） |
| **P1** | 6.4 镜像更新滚动策略 + 回滚 | 已运行容器镜像漂移不重建（EnsureRunning no-op）；无分批/门槛/回滚 | 实现镜像 diff → 重建 + 策略；或明确降级为"重建式更新"并修订验收 |
| **P1** | M1/M2 验收"kubectl get nodes / kubectl apply"字面未达成 | 无真实 K8s 接入（CRD 未 apply、无 apiserver），验收以 REST API 适配 | 决策：验收口径正式改为 REST 化（文档回写），或排期真实 K8s 接入 |
| **P1** | 2.8 NodeJob 未实现 | 仅协议占位"待定"；ROADMAP 缺口 1 未关闭 | 明确关闭（M4 后不再排）或排期入后续版本 |
| **P2** | 6.5 资源管理（调度/资源超卖） | 仅 Replicas 伸缩；超卖/调度未做 | 排期或修订 6.5 完成标准 |
| **P2** | Flannel/边缘容器网络（缺口 6） | M2 交付物从未实现，无处置结论 | 明确关闭（Docker bridge 够用）或排期 CNI |
| **P2** | M0 验收"CRD 可 kubectl apply" | 无 CRD manifest，从未 apply | 补 manifest 或修订验收 |
| **P2** | M0 验收"CI PR 反馈 ≤10min" | CI 从未运行（远程仓库未关联，PROGRESS §5 P1 仍开放） | 关联远程 + 首次 push 验证后关闭 |
| **P2** | 9.1 架构文档未评审 + 内容滞后（S11-S13） | M0 验收"评审通过（有评审记录）"未满足 | 安排评审并回写 NATS→MQTT、Token→mTLS、实现进度 |
| **P2** | 4.4 压缩未实现且无跟踪 | 计划内延后（gzip M2+、Protobuf M4+），但 PROGRESS 待办无对应条目 | 补一条显式延后登记（含目标版本） |
| **P3** | M1-M3 通道无认证的历史事实 | Token 过渡被跳过，直接 M4 mTLS（M4 已覆盖） | 文档回写即可，无代码动作 |
| **P3** | containerd CRI 实现 | PROGRESS §4E 已列 P2 延后（Docker runtime 满足当前） | 保持跟踪 |

---

## 5. 核对统计

- **核对项总数**：44 项（M0 12 + M1 19 + M2 13）
- **✅ 完成**：28 项（63.6%）——M0 骨架/共享库/CI 基础/Helm；M1 协议/连接/路由/可靠投递/CloudHub/EdgeHub/MetaManager/Edged POC/心跳/重连/持久化/覆盖率；M2 部署/配置/状态上报/自治基础/Edged 完整
- **🟨 部分**：8 项（18.2%）——1.4 CRD manifest、1.2 多架构、9.1 架构评审、4.4 压缩、kubectl 验收适配×2、6.4、6.5、30min 时长
- **⬜ 未做**：5 项（11.4%）——2.8 NodeJob、8.3 E2E 完整场景、Flannel、M0 验收"CRD apply"、"CI ≤10min"验证
- **⚠️ 归属变更**：3 项——4.5（M1→M4，与 7.1/7.4 合并）、2.4（M1→M4）、8.6 keadm 基础（M1→M4）
- **📋 过时记录**：14 处（PROGRESS 6、ROADMAP 4、ARCHITECTURE 3、DEV-ENV 1）
- **结论**：M0 ✅ 完成（2 项验收未实证）；M1 ✅ 完成（3 项归属变更需回写、kubectl 验收为适配）；M2 🟨 **核心链路完成、里程碑验收未完全达成**（8.3 E2E/30min 自治 + 6.4 滚动更新为 P1 缺口），与 PROGRESS 声明"M2 第 1-4 轮完成"大体一致，但声明未标注这两项 P1 残留。

---

*本报告为只读审计产物，未修改任何代码/文档；所有结论均附 git commit、代码位置或测试输出证据。*
