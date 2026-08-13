# EdgeFlow 项目进度台账（PROGRESS）

> 最后更新：2026-08-13 09:25 (Asia/Shanghai)
> 维护规则：每个模块完成验证后更新本表；验证证据（命令输出）保留在对应模块记录中。

---

## 1. 项目概况

| 项目 | 内容 |
|------|------|
| 项目名称 | EdgeFlow（对标 KubeEdge 的边缘计算平台） |
| 仓库位置 | `/Users/mac/Documents/edgeflow/` |
| 语言/版本 | Go 1.26.2（arm64），零第三方依赖（纯标准库） |
| 开发方式 | Vibe Coding（AI 辅助），多 Agent 协作研发 |
| 开发计划 | `docs/ROADMAP.md`（基于已确认的 EdgeFlow 开发计划拆解） |

## 2. 环境配置状态

| 项目 | 状态 | 验证方式 | 证据 |
|------|------|---------|------|
| Go 1.26.2 | ✅ 完成 | `go version` | go1.26.2 darwin/arm64 |
| Git 2.50.1 | ✅ 完成 | `git --version` | git version 2.50.1 |
| Docker 29.4.3 | ✅ 完成 | `docker --version` | Docker version 29.4.3 |
| Make 3.81 | ✅ 完成 | `make --version` | GNU Make 3.81 |
| Node v24.12.0 | ✅ 完成 | `node --version` | v24.12.0 |
| VS Code 1.133.0 | ✅ 完成 | 应用已安装 | Visual Studio Code.app |
| golangci-lint 2.12.2 | ✅ 完成 | `golangci-lint version` | 2.12.2 built with go1.26.2 |
| Delve（dlv）1.27.1 | ✅ 完成 | `~/go/bin/dlv version` | Delve Debugger 1.27.1（新终端生效） |
| GOPROXY | ✅ 完成 | `go env GOPROXY` | https://goproxy.cn,direct |
| SSH key（GitHub） | ✅ 已生成 | `~/.ssh/id_ed25519.pub` | 待用户粘贴公钥到 GitHub（见 ENV-SETUP.md §4.2） |
| 配置脚本 | ✅ 完成 | `setup-env.sh`（幂等） | 语法检查通过；按文档可复现 |

## 3. 模块开发状态（对照 ROADMAP）

| 模块 ID | 名称 | 状态 | 完成标准 | 验证证据 |
|---------|------|------|---------|---------|
| M0-1 | 项目骨架 + 共享库 + cloudcore healthz | ✅ **完成** | 见 ROADMAP T0-T8 | 见下方 §4 |
| M0-2 | CRD 类型定义（Node/Device） | ✅ **完成** | EdgeNode/DeviceModel/Device 定义 + DeepCopy + 文档 | commit a541128，apis/ 3 类型 11 测试 |
| M0-3 | CI/CD 完整流水线 | ✅ 基础版完成 | lint+test+覆盖率门槛 | commit ab73062 |
| M0-4 | Helm Chart 骨架 | ✅ **完成** | helm lint 通过 + template 渲染正确 | commit 9d78246，helm v4.2.3 |
| M1 | 云边核心通信链路（CloudHub/EdgeHub） | ✅ **M1 一~三期完成** | +EdgeNode 对接+可靠投递 4.6+PodSync 链路 | commits e569ea1~5312253，见 §4D |
| M2 | 应用部署与边缘自治 | 🟨 **启动轮完成** | Edged 方案 A POC 通过（P0 决策落地） | commits c9db4ba~3826465，见 §4E |
| M3 | 设备管理（Device CRD/Twin/Mapper） | ⏳ 待开发 | 依赖 M2 | — |
| M4 | 生产化与规模化 | ⏳ 待开发 | 依赖 M3 | — |
| M5 | MVP 发布与文档交付 | ⏳ 待开发 | 依赖 M4 | — |

## 4. 首个模块（M0-1）验证结果

### 4.1 代码库状态

| 检查项 | 结果 |
|--------|------|
| Git 提交数 | 7 个 commit（骨架 → 模块 → 配置 → CI → 文档） |
| 最新 commit | `470e32d` docs: 同步 ROADMAP 首模块完成状态 |
| 工作区状态 | 干净（无未提交改动） |
| 代码审查 | ✅ 有条件通过 → 3 项 P1 已全部修复（见 docs/CODE-REVIEW.md） |

### 4.2 自动化验证（2026-08-13 实际执行）

| 验证项 | 命令 | 结果 |
|--------|------|------|
| 编译 | `go build ./...` | ✅ 通过 |
| 静态检查 | `go vet ./...` | ✅ 通过 |
| 格式 | `gofmt -l .` | ✅ 无输出 |
| Lint | `golangci-lint run ./...` | ✅ 0 issues |
| 测试（race） | `go test -race -cover ./...` | ✅ 全部通过 |
| 覆盖率（pkg/config） | — | 100.0% |
| 覆盖率（pkg/httpx） | — | 100.0% |
| 覆盖率（pkg/log） | — | 100.0% |
| 覆盖率（pkg/version） | — | 100.0% |
| 覆盖率（cmd/cloudcore） | — | 88.2% |
| 总覆盖率 | — | **87.5%**（CI 门槛 ≥70% ✅） |

### 4.3 运行时验证（6 种启动场景）

| 场景 | 期望 | 实测 |
|------|------|------|
| 默认启动（配置文件 8080） | :8080/healthz → 200 | ✅ 200，日志"来源: 配置文件" |
| 配置文件缺失 | 回退默认 8080，不报错 | ✅ 200，日志"来源: 默认值" |
| 配置文件改端口 9090 | :9090/healthz → 200 | ✅ 200，日志"来源: 配置文件" |
| 环境变量 EDGEFLOW_CLOUDCORE_PORT=10000 | :10000/healthz → 200 | ✅ 200，日志"来源: 环境变量" |
| 命令行 --port 7070 | :7070/healthz → 200 | ✅ 200，日志"来源: 命令行" |
| 非法端口 70000 / 坏 JSON | 启动前报错，exit 1 | ✅ 报错退出，无 listening 日志 |
| 优雅退出 | SIGTERM → 正常退出 | ✅ 退出码 0，无进程残留 |

### 4.4 healthz 响应示例

```json
{"status":"ok","version":{"version":"v0.1.0","gitCommit":"e494e20","buildTime":"2026-08-13T09:12:09+0800","goVersion":"go1.26.2"}}
```

## 4B. M1 云边通信通道验证结果（2026-08-13）

### 代码库状态
| 检查项 | 结果 |
|--------|------|
| 新增提交 | 6 个（protocol/cloudhub/edgehub/helm/cross-build/apis + gofmt） |
| 依赖 | gorilla/websocket v1.5.3（M1 起引入，WebSocket 需第三方库，计划内决策） |
| 代码审查 | ✅ 有条件通过（无 P0/P1）→ P2×3 已修复（commit e565a15）→ docs/CODE-REVIEW-M1.md |

### 自动化验证
| 验证项 | 结果 |
|--------|------|
| go build / go vet / gofmt | ✅ 全部通过 |
| golangci-lint | ✅ 0 issues |
| go test -race（全仓） | ✅ 全部通过，总覆盖率 81.7%（≥70%） |
| cloudhub 覆盖率 | 77.8% |
| edgehub 覆盖率 | 82.2% |
| protocol 覆盖率 | 90.3% |
| race 连跑 3 遍（cloud/edge） | ✅ 无 flaky |
| helm lint | ✅ 0 failed |

### 端到端联调（真实 cloudcore + edgecore）
| 场景 | 结果 |
|------|------|
| 注册链路 | ✅ cloudcore 日志"节点 edge-e2e-1 注册成功"，edgecore 确认注册 |
| 心跳保活 | ✅ 32s 后进程存活，无重连日志 |
| 断线检测 | ✅ cloudcore 记录"已断开，从注册表移除" |
| 指数退避 | ✅ 2s/4s/8s 递增重试（日志时间戳验证） |
| 自动重连 | ✅ cloudcore 重启后 8s 内 edgecore 重连并重新注册 |
| healthz | ✅ :8080 持续 200 |
| 进程清理 | ✅ 优雅退出无残留 |

## 4C. M1 二期验证结果（MetaManager/注册表/路由，2026-08-13）

### 新增模块
| 模块 | 提交 | 内容 |
|------|------|------|
| edge/pkg/metamanager | 3aaaf28 | SQLite Store（KV + 节点信息，WAL），edgecore 集成 |
| cloud/pkg/registry + API | 3c7b99d | NodeInfo 注册表 + GET /api/v1/nodes[/{nodeID}] |
| cloud/pkg/cloudhub 路由 | 081eb0f | SendToNode/Broadcast/Deliver（WBS 4.3） |

### 自动化验证
| 项目 | 结果 |
|------|------|
| go build/vet/gofmt | ✅ 通过 |
| go test -race（12 包） | ✅ 全绿，总覆盖率 82.8%（≥70%） |
| golangci-lint | ✅ 0 issues |
| 包覆盖率 | registry 100% / metamanager 72.9% / cloudhub 81.5% / cmd-cloudcore 86.4% / cmd-edgecore 56.5%（最低，见待办） |

### 端到端联调（真实进程）
| 场景 | 实测结果 |
|------|---------|
| 注册→API 查询 | ✅ GET /api/v1/nodes 返回节点（Ready，完整字段） |
| 单节点查询 | ✅ GET /api/v1/nodes/edge-e2e-2 → 200 |
| 停止→Offline | ✅ 节点状态变 Offline |
| 重启→持久化 | ✅ MetaManager 日志"已加载 1 条节点元数据"，SQLite 落盘（db+wal+shm） |
| 重连→Ready | ✅ 恢复 Ready |
| 进程清理 | ✅ 优雅退出无残留 |

### 代码审查（docs/CODE-REVIEW-M1B.md）
- 结论：**✅ 通过（0 P0 / 0 P1，9 项 P2 加固项）**
- P2 项：LIKE 通配符转义、WAL checkpoint、Offline 无 TTL、入缓冲静默丢消息（已文档化）、WriteTimeout 缺失、API Encode 错误无日志、RegisteredAt 语义不对称、SQLite 多进程策略、cmd-edgecore 覆盖缺口

## 4D. M1 三期验证结果（EdgeNode 对接/可靠投递/PodSync，2026-08-13）

### 新增模块
| 模块 | 提交 | 内容 |
|------|------|------|
| registry EdgeNode 映射 | 641863e | ToEdgeNode/ListEdgeNodes + GET /api/v1/edgenodes（K8s 风格 items） |
| cloudhub ReliableSend | 3197ad3 | pending+Ack 匹配+超时重试同 ID（WBS 4.6 云端侧） |
| edgehub 自动 Ack+幂等 | 19dd66f | handleDownlink（缓存 1000 FIFO 去重）+ Send + metamanager SavePod |
| cloudcore podsync API | 15366e7 | POST /api/v1/nodes/{id}/podsync → ReliableSend（200/404/504） |
| P1 修复 | 5312253 | syncPod 0%→91.3%、handlePodSync 0%→84.2%、Pod key 含 namespace |

### 自动化验证
| 项目 | 结果 |
|------|------|
| go test -race（12 包） | ✅ 全绿，总覆盖率 83.7% |
| golangci-lint | ✅ 0 issues |
| 关键覆盖率 | registry 100% / metamanager 77.5% / cloudhub 83% / cloudcore 88.8% / edgecore 61.4% / edgehub 84.7% |

### 端到端联调（真实进程）
| 场景 | 实测结果 |
|------|---------|
| EdgeNode API | ✅ Running→Offline→Running 状态流转 |
| PodSync 可靠下发 | ✅ POST → 200 acked:true（云端 ReliableSend → 边缘落盘 → 自动 Ack） |
| SQLite 落盘 | ✅ sqlite3 直查 pods/default/nginx 与 pods/prod/nginx 两条共存（多命名空间隔离） |
| 按 ns 删除 | ✅ delete default 后仅剩 prod，无误删 |
| 重启持久化 | ✅ 节点元数据与 Pod 数据同库保留 |

### 代码审查（docs/CODE-REVIEW-M1C.md）
- 结论：有条件通过（0 P0）→ **P1×3 全部修复**（commit 5312253，含覆盖率前后对比证据）
- P2 项：pending 交叉清理、ErrAckFailed 映射 502、ReliableSend 无 context、handleDownlink 非原子、云端 operation 校验（已记录待办）

## 4E. M2 启动轮验证结果（Edged POC/增量订阅/4.6 P2，2026-08-14）

### 新增模块
| 模块 | 提交 | 内容 |
|------|------|------|
| metamanager 增量订阅 | 089c358 | Subscribe/Unsubscribe/Event，EventPodUpsert/Delete，缓冲满丢弃（声明式收敛兜底） |
| 4.6 P2 收尾 | 8321b0e | pending 交叉清理/ErrShuttingDown、ReliableSendContext、ErrAckFailed→502、downlink 原子化、operation 校验 |
| Edged POC | c9db4ba | ContainerRuntime 接口 + Mock/Docker 双实现 + 声明式 reconcile + Status + POC 报告 |
| edgecore 装配 | 9fe47c1 | Edged 启动 + 订阅触发 Trigger + 优雅退出顺序 |
| P2 修复×5 | df2e607 | 零值超时回落/image 校验/ctx 传递/死字段/周期 env 可配 |

### 端到端联调（真实进程 + 真实 Docker 容器）
| 场景 | 实测结果 |
|------|---------|
| PodSync add | ✅ POST → 200 acked → 订阅触发 → Edged 调谐 → Docker 容器 edgeflow-default-web-demo 创建运行 |
| reconcile 循环 | ✅ 日志稳定（1 pods, 1 running, 0 error） |
| SQLite 落盘 | ✅ pods/default/web-demo |
| PodSync delete | ✅ 200 acked → 容器移除 |
| DockerRuntime 冒烟 | ✅ 6 步（创建/检查/幂等/标签发现/删除/删除幂等） |
| 进程清理 | ✅ 优雅退出无残留 |

### 自动化验证
| 项目 | 结果 |
|------|------|
| go test -race（13 包） | ✅ 全绿，总覆盖率 82.4%（edged 85.1%） |
| golangci-lint | ✅ 0 issues |
| 审查（docs/CODE-REVIEW-M2A.md） | ✅ 有条件通过（无 P0/P1）→ P2×5 修复 + P2×3 记录待办 |

### P2 待办（3 项未修）
- P2-1：Edged.status 过期条目清理（WBS 6.3 PodStatus 上报前必改）
- P2-3：docker run 冲突兜底未验标签（低危）
- P2-8：优雅退出最坏延迟 30s（在途 docker 命令，文档已注明）

## 5. 待办项（Backlog）

| 优先级 | 事项 | 说明 |
|--------|------|------|
| P1 | GitHub 远程仓库关联 | 用户把 `~/.ssh/id_ed25519.pub` 粘贴到 GitHub → `git remote add origin` → push（步骤见 ENV-SETUP.md §4.2） |
| P1 | 推送后验证 CI 首次运行 | push 后 Actions 标签页应显示 lint+test 绿勾 |
| P2 | cmd-edgecore 覆盖率缺口（56.5%） | "注册成功→SQLite 落盘"集成链路补集成级单测（M2 前） |
| P2 | M1C 审查 P2 项×5 | pending 交叉清理/ErrAckFailed→502/context 取消/非原子/云端 operation 校验（见 CODE-REVIEW-M1C.md） |
| P2 | M1B 审查 P2 项×9 | LIKE 转义/WAL checkpoint/Offline TTL/WriteTimeout 等（见 CODE-REVIEW-M1B.md） |
| P3 | 日志级别过滤（SetLevel） | pkg/log 的 Level 目前仅前缀标记，后续模块需要时实现 |
| P3 | M1 通道 P3 项×4 | newID 忽略 rand 错误 / Shutdown 撞 Start 初始化窗口 / 被踢连接标志延迟清除 / 退避重置缺单测（见 CODE-REVIEW-M1.md） |
| P2 | edgecore 占位程序测试 | 非核心包，M1 开发时补 |
| P2 | Helm Chart 骨架（M0-4） | 进入部署阶段前完成 |

## 6. 里程碑状态

| 里程碑 | 状态 | 说明 |
|--------|------|------|
| M0 架构基线与开发就绪 | ✅ **完成** | 骨架/共享库/CI/CRD/Helm/架构文档全部就绪 |
| M1 云边核心通信链路 | ⏳ 未开始 | 依赖 M0 收尾 |
| M2 应用部署与边缘自治 | ⏳ 未开始 | — |
| M3 设备管理 | ⏳ 未开始 | — |
| M4 生产化 | ⏳ 未开始 | — |
| M5 发布 | ⏳ 未开始 | — |
