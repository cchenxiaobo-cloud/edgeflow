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
| M2 | 应用部署与边缘自治 | 🟨 **第 1-4 轮完成** | +Edged 多副本/健康自愈（6.4/6.5）| commits c9db4ba~47d9e21，见 §4E/§4F/§4H/§4I |
| M3 | 设备管理（Device CRD/Twin/Mapper） | 🟨 **第 1-3 轮完成** | +Mapper 接入 EventBus（MQTT 数据面）| commits 7d82c0c~99b5624，见 §4G/§4H/§4I |
| M4 | 生产化与规模化 | 🟨 **主体完成** | +keadm/NodeController/Modbus/多架构 | commits 2386fa3~499f225，见 §4J/§4K |
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

## 4F. M2 第二轮验证结果（PodStatus 上报/自治，2026-08-14）

### 新增模块
| 模块 | 提交 | 内容 |
|------|------|------|
| cloud/pkg/podstatus + API | 707128f | 云端 Pod 状态存储（93.3% 覆盖）+ GET /api/v1/pods、/api/v1/nodes/{id}/pods |
| 边缘上报循环 + P2-1 | bc58e40 | 30s 周期上报（env 可配）+ status map 清理 |
| P1 修复 + P2×5 | 最新 | Absent 终态保留窗口 90s + 云端 Absent→Delete + phase 校验 + recover + 周期上下限 |

### 端到端联调（真实进程 + 真实 Docker）
| 场景 | 实测结果 |
|------|---------|
| PodSync add → 状态上报 | ✅ /api/v1/pods 显示 Running（上报周期 3s 验证） |
| 断网自治 | ✅ 停 cloudcore 40s，容器持续运行，Edged 本地调谐不受影响 |
| 恢复同步 | ✅ cloudcore 重启 → 重连注册 → 上报恢复 |
| **删除收敛（P1 核心）** | ✅ delete → 容器移除 → Absent 终态上报 → 云端列表从 1 → 0 |

### 自动化验证
| 项目 | 结果 |
|------|------|
| go test -race（14 包） | ✅ 全绿，总覆盖率 83.0% |
| golangci-lint | ✅ 0 issues |
| 审查（docs/CODE-REVIEW-M2B.md） | ✅ 有条件通过（1 P1 + 10 P2）→ P1 已修复（端到端验证）+ P2×5 修复 + P2×5 记录待办 |

### P2 待办（5 项未修）
- store 全局单锁（并发量低，可接受）
- 节点删除后 Pod 状态残留（有意为之：节点重连后状态保留，文档化）
- 边侧时钟偏差（LastReconcileAt 依赖本地时钟）
- 无批量上报（逐 Pod 一条消息）
- 旧连接关闭窗口可投递（微秒级窗口，低危）

## 4G. M3 启动轮验证结果（设备管理链路，2026-08-14）

### 新增模块
| 模块 | 提交 | 内容 |
|------|------|------|
| edge/pkg/mapper | 7d82c0c | DeviceMapper 接口 + MapperRegistry（RWMutex、幂等 StartAll/StopAll、errors.Join） |
| mappers/mock_sensor | 7d82c0c | 模拟温湿度传感器（targetTemp 收敛、无 goroutine 泄漏、停止冻结） |
| edge/pkg/devicetwin | 744afaa | Twin/TwinStore（SetDesired/UpsertReported 合并语义、深拷贝、自动创建） |
| cloud/pkg/devicestatus | 744afaa | 设备状态存储（字段级合并保 desired、nodeID 权威） |
| 设备 API | 744afaa | GET /api/v1/devices、/api/v1/nodes/{id}/devices、POST device-command（五态错误语义） |
| edgecore 装配 | 698ee5f | mapperCommandExecutor 接 Dispatch、collectMapperReports 采样汇入影子、上报循环 |

### 端到端联调（真实进程）
| 场景 | 实测结果 |
|------|---------|
| 设备自动注册 | ✅ MockSensor sensor-01 启动（2s 采集） |
| 周期上报 | ✅ /api/v1/devices 显示 properties（temperature/humidity） |
| 指令下发 | ✅ POST device-command targetTemp=25 → 200 acked:true |
| **指令影响** | ✅ 温度 31.05 → 25.59 向目标收敛（边缘日志"快照 2 个属性已写入影子"） |
| desired 同步 | ✅ 云端显示 {"targetTemp": 25} |
| 单节点 API | ✅ /api/v1/nodes/{id}/devices 返回 sensor-01 |
| 进程清理 | ✅ 优雅退出无残留 |

### 自动化验证
| 项目 | 结果 |
|------|------|
| go test -race（18 包） | ✅ 全绿，总覆盖率 85.1%（devicetwin 100%、mapper 96.4%、devicestatus 95.5%） |
| golangci-lint | ✅ 0 issues |
| 审查（docs/CODE-REVIEW-M3A.md） | ✅ 通过（0 P0 / 0 P1 / 6 P2）→ P2×6 记录待办 |

### M2 收尾状态（本轮核对）
- ✅ 已完成：PodSync 下发→容器运行→状态上报→断网自治→恢复同步→删除收敛（M2 核心链路）
- 📋 M2 完整化待办（列入 §5）：Edged 健康检查/多副本（6.4/6.5）、ConfigMap/Secret 下发（6.2）、镜像更新滚动策略、8.3 E2E 完整场景

## 4H. M3 二期 + M2 完整化验证结果（EventBus/ConfigSync，2026-08-14）

### 新增模块
| 模块 | 提交 | 内容 |
|------|------|------|
| ConfigSync 链路 | 5403daa | 云端 POST config-sync 五态 + 可靠投递 + 边缘 handleConfigSync 落盘（configs/ns/name）+ 自动 Ack |
| MQTT EventBus | 2a0d0a3 | paho v1.5.1 封装（QoS1、AutoReconnect+ConnectRetry、OnConnect 订阅恢复、IsOnline 瞬态检测、主题段校验）+ docs/EVENTBUS-GUIDE.md |
| P1 修复 | f3f3df1 | Secret 日志脱敏（P1-1）+ onConnect 全程持 RLock（P1-2 map 竞态）+ 重连并发测试 |

### 端到端验证（真实进程/mosquitto）
| 场景 | 实测结果 |
|------|---------|
| 配置下发 | ✅ POST config-sync add app-config → 200 acked → sqlite3 查 configs/default/app-config |
| 配置删除 | ✅ delete → sqlite3 确认删除；多配置共存（app-config + db-secret） |
| 非法路径 | ✅ 400/404 拦截 |
| MQTT 收发 | ✅ mosquitto 临时 broker：发布/订阅 QoS1 全量断言 |
| MQTT 重连 | ✅ 停 broker → 自动重连（ConnectRetry 1s）→ OnConnect 恢复订阅 → 恢复收发 |
| 重连并发竞态 | ✅ race 下并发 Subscribe/Unsubscribe 无 panic（P1-2 回归测试） |

### 自动化验证
| 项目 | 结果 |
|------|------|
| go test -race（19 包） | ✅ 全绿，总覆盖率 82.3% |
| golangci-lint | ✅ 0 issues |
| 审查（docs/CODE-REVIEW-M3B.md） | ✅ 有条件通过（0 P0 / 2 P1 / 12 P2）→ P1×2 已修复 + P2×4 顺手修 + P2×8 记录待办 |

### P2 待办（8 项，节选）
- Mapper 未接入 EventBus（独立库交付，装配下一轮）
- broker 生命周期管理（systemd/容器化）
- configs 无增量通知（当前无消费方，轮询对账）
- ConfigMap/Secret 同名同 ns 互覆盖（文档化决策，需产品确认）
- QoS1 不保证去重（消费方需幂等）
- Secret 落盘明文（生产需加密）
- Connect ctx 取消后 paho 后台重试无法终止（Disconnect 兜底）
- TestQoS1Delivery 偶发端口抢占（未复现，13 连跑全绿）

## 4I. M3 三期 + M2 完整化验证结果（MQTT 数据面/多副本/自愈，2026-08-14）

### 新增模块
| 模块 | 提交 | 内容 |
|------|------|------|
| Mapper 接入 EventBus | 99b5624 | MockSensor MQTT 模式（telemetry 发布 QoS1 + command 订阅）+ 降级本地模式 |
| Edged 多副本 | 47d9e21 | 副本命名 edgeflow-ns-name-index、补齐/收缩策略、缺口兜底 |
| Edged 健康自愈 | 47d9e21 | Inspect 非 Running→重启 + RestartCount（覆盖率 89.2%） |
| P1 修复×2 | c885229 | 旧命名迁移（Index=-1 优先清理，消除 churn）+ CrashLoopBackOff（3 次阈值/30s 退避/60s 稳定重置） |

### 端到端验证（真实进程 + Docker + mosquitto）
| 场景 | 实测结果 |
|------|---------|
| 多副本 | ✅ podsync replicas=2 → docker 出现 -0/-1 双容器 |
| 健康自愈 | ✅ docker stop 副本 1 → 12s 内自动重启（时间差证实） |
| MQTT 遥测流 | ✅ mosquitto_sub 收到 devices/default/sensor-01/telemetry（temperature/humidity/ts） |
| **MQTT 指令收敛** | ✅ 发布 command targetTemp=22 → 温度 25.99→24.21→23.07→22.97 收敛 |
| 降级路径 | ✅ broker 缺席 → Warn + 本地模式（不退出） |
| 进程清理 | ✅ 无残留 |

### 自动化验证
| 项目 | 结果 |
|------|------|
| go test -race（19 包） | ✅ 全绿，总覆盖率 81.0%（edged 89.2%、mock_sensor 91.1%） |
| golangci-lint | ✅ 0 issues |
| 审查（docs/CODE-REVIEW-M4A.md） | ✅ 有条件通过（0 P0 / 2 P1 / 12 P2）→ P1×2 根因修复 + 回归测试（旧命名迁移无 churn、退避不再重启） |

### P2 待办（节选）
- Replicas=0 无法表达 scale-to-zero（int 非指针）
- RestartCount 未进 PodStatusPayload + 不持久化
- Publish token.Wait() 半死连接窗口阻塞
- broker 晚启动时 command 订阅不自动恢复（需重启 edgecore）
- 调谐轮串行 30s 超时延迟累积
- 32-bit hash 碰撞、滚动更新、进程级 liveness

## 4J. M4 前置验证结果（mTLS/镜像/Helm，2026-08-14）

### 新增模块
| 模块 | 提交 | 内容 |
|------|------|------|
| pkg/certs | 0a7fcc2 | 纯标准库证书管理：CA/服务端/客户端证书幂等生成、LoadTLSConfig 双向强制、TLS1.2+、私钥 0600、半套 fail-fast |
| CloudHub TLS | 0a7fcc2 | WithTLS Option + tls.NewListener + mTLS 审计日志（peer CN） |
| EdgeHub wss | 0a7fcc2 | TLSConfig 注入 + ws→wss 归一化，TLS off 完全向后兼容 |
| 镜像构建 | 3bb60ce | 多阶段 distroless：cloudcore 16.7MB / edgecore 22.5MB、nonroot(65532) |
| Helm 完整化 | 3bb60ce | values 真实化/资源/探针/env + /data 可写挂载（P1-3 修复） |
| M4B P1×4+P2×2 | 70d1f5e | 文档连接串/TLS 变量修正、SAN 可注入（env+脚本）、CA 公私钥匹配校验 |

### 端到端验证（真实进程）
| 场景 | 实测结果 |
|------|---------|
| mTLS 双向认证 | ✅ 云侧"mTLS 连接已认证（peer CN=edgeflow-edge-tls-2）"+ 注册成功；边侧 wss:// 连接 |
| 拒绝路径 | ✅ TLS off → bad handshake 持续退避；云侧"HTTP request to HTTPS server"记录 |
| 证书权限 | ✅ edgecore.key 0600 |
| 镜像 | ✅ docker run --version 输出 v0.1.0；healthz 200 容器内冒烟 |
| Helm | ✅ lint 0 failed、template 渲染（含卷挂载）、install --dry-run 通过 |

### 自动化验证
| 项目 | 结果 |
|------|------|
| go test -race（20 包） | ✅ 全绿，总覆盖率 79.8%（certs 含 SAN/错配 key 测试） |
| golangci-lint / helm lint | ✅ 0 issues / 0 failed |
| 审查（docs/CODE-REVIEW-M4B.md） | ✅ 有条件通过（0 P0 / 4 P1 / 5 P2）→ P1×4 修复 + P2×2 修复 + P2×3 记录待办 |

### P2 待办（3 项）
- 存量压测 TestShutdownDuringNewConnections 偶发 race 观察项（10+ 复跑全绿，非本轮改动）
- TLS 握手无超时/并发上限（net/http 固有）
- gen-certs 与 Go 侧 CN 约定（脚本已可配 CLIENT_CN，默认对齐 edgeflow-edgecore）

## 4K. M4 主体验证结果（keadm/NodeController/Modbus/多架构，2026-08-14）

### 新增模块
| 模块 | 提交 | 内容 |
|------|------|------|
| cmd/keadm（8.6） | 2386fa3 | init（生成 cloudcore.yaml）/join（env+systemd）/reset（白名单+确认）/version；异常路径 exit=2 |
| cloud/pkg/nodecontroller（2.4） | f71684e | 心跳超时扫描（interval/timeout env 可配、时钟可注入），覆盖率 96.8% |
| Modbus（5.2） | a290686 | 自实现协议模拟器（5 功能码+错误码）+ goburrow Mapper + op_ledger 台账（30 天清理） |
| 多架构镜像 | 301daee | 本地 registry 闭环：manifest 双架构 + QEMU 交叉运行版本一致 + CI release.yml |
| M4C P2×3 | 499f225 | NodeID 白名单（防 systemd 注入）、coil 严格解析、go.mod 标记 |

### 端到端验证
| 场景 | 实测结果 |
|------|---------|
| keadm | ✅ version/init exit=0，产物 cloudcore.yaml+NOTES；异常路径 exit=2 |
| NodeController | ✅ SIGSTOP 冻结心跳 → Offline → SIGCONT → Ready（状态机闭环） |
| Modbus 读写 | ✅ 模拟器 + Mapper：读温度湿度、写目标温度回读验证、线圈读写 |
| Modbus 台账 | ✅ op_ledger 落 SQLite，按条件查询，30 天清理 |
| 多架构 | ✅ manifest amd64+arm64，双架构 --version 一致（v0.1.0） |
| 安全扫描/质量门 | ✅ 24 包 race 全绿、lint 0、覆盖率 77.9% |

### 审查（docs/CODE-REVIEW-M4C.md，178 行）
- 结论：✅ 有条件通过（0 P0 / 0 P1 / 9 P2）→ P2×3 修复 + P2×6 处置完毕（4 修复 / 1 关闭 / 1 延后）

### M4C P2 处置台账（P2×6，2026-08-14）

| 项 | 内容 | 方式 | 结论（一句话） | 证据 |
|----|------|------|----------------|------|
| ① | TLSSAN 无语法校验（非法条目 Warn 跳过） | 修复 | 非法条目 fail-fast：启动即报错退出（exit 1），不再静默跳过导致证书仅回环可用 | parseSANList + TestParseSANList/TestRunInvalidTLSSAN（cmd/cloudcore） |
| ② | reset 白名单误删同名文件 / install.sh 无校验和 | 修复 | 产物校验清单 keadm-manifest.json 记录 sha256；reset 删除前校验，不匹配跳过并提示人工确认；旧产物无清单时保持原行为并提示 | manifest.go + TestResetSkipsTamperedFile/TestManifestMergesInitAndJoin（cmd/keadm） |
| ③ | 模拟器 unit ID 不校验 / 无连接数上限 | 修复 | unit ID 仅接受 1-247（0 广播/248-255 保留按规范应答 0x0B）；maxConns 默认 8，超限拒连 | TestUnitIDOutOfRangeRejected/TestMaxConnsRejectsExcess（pkg/modbussim） |
| ④ | NodeController List→MarkOffline 陈旧快照窗口 | 关闭 | 已在 docs/NODECONTROLLER.md §6 文档化为已知边界（check-then-act 概率极低、下一心跳自愈），无需改动 | NODECONTROLLER.md §6「与断开事件竞态」 |
| ⑤ | modbus mapper float→uint16 截断 | 修复 | 写入改为四舍五入到 0.1°C 粒度（25.55→256 非 255），值域校验前置不越界 | math.Round + TestHandleCommandTargetTempRounds（mappers/modbus） |
| ⑥ | :latest 无 immutable 保护 / 无 cosign/SBOM | 延后 | 需镜像仓库 + cosign 签名基础设施，属 M5 发布阶段；风险已登记 MULTIARCH.md §5，待办见下表 §5 | MULTIARCH.md §5 风险表 |

> 处置 commit：`fix`（①②③⑤ 代码+测试）与 `docs`（④⑥+台账）各一个，见 4K 验证记录与 git log。

## 5. 待办项（Backlog）

| 优先级 | 事项 | 说明 |
|--------|------|------|
| P1 | GitHub 远程仓库关联 | 用户把 `~/.ssh/id_ed25519.pub` 粘贴到 GitHub → `git remote add origin` → push（步骤见 ENV-SETUP.md §4.2） |
| P1 | 推送后验证 CI 首次运行 | push 后 Actions 标签页应显示 lint+test 绿勾 |
| P2 | cmd-edgecore 覆盖率缺口（56.5%） | "注册成功→SQLite 落盘"集成链路补集成级单测（M2 前） |
| P1 | M2 完整化（6.2/6.4/6.5） | ConfigMap/Secret 下发、Edged 健康检查/多副本、镜像更新滚动策略（8.3 E2E 完整场景） |
| P1 | M3 二期：MQTT EventBus（3.6） | NATS MQTT POC 后接入真实 MQTT broker，替换内存模拟通道 |
| P2 | M3A 审查 P2 项×6 | byDevice 路由含 namespace、多实例 Mapper、空 deviceName 校验、LastReportedAt 单调性、502 Desired 分叉、无 TTL/GC（见 CODE-REVIEW-M3A.md） |
| P2 | M1C 审查 P2 项×5 | pending 交叉清理/ErrAckFailed→502/context 取消/非原子/云端 operation 校验（见 CODE-REVIEW-M1C.md） |
| P2 | M1B 审查 P2 项×9 | LIKE 转义/WAL checkpoint/Offline TTL/WriteTimeout 等（见 CODE-REVIEW-M1B.md） |
| P3 | 日志级别过滤（SetLevel） | pkg/log 的 Level 目前仅前缀标记，后续模块需要时实现 |
| P3 | M1 通道 P3 项×4 | newID 忽略 rand 错误 / Shutdown 撞 Start 初始化窗口 / 被踢连接标志延迟清除 / 退避重置缺单测（见 CODE-REVIEW-M1.md） |
| P2 | edgecore 占位程序测试 | 非核心包，M1 开发时补 |
| P2 | Helm Chart 骨架（M0-4） | 进入部署阶段前完成 |
| P2 | M4C 审查 P2-⑥ :latest immutable / cosign / SBOM | 延后（M5 发布阶段）：需镜像仓库 + cosign 签名基础设施；风险已登记 MULTIARCH.md §5（处置台账见 4K 节） |

## 6. 里程碑状态

| 里程碑 | 状态 | 说明 |
|--------|------|------|
| M0 架构基线与开发就绪 | ✅ **完成** | 骨架/共享库/CI/CRD/Helm/架构文档全部就绪 |
| M1 云边核心通信链路 | ⏳ 未开始 | 依赖 M0 收尾 |
| M2 应用部署与边缘自治 | ⏳ 未开始 | — |
| M3 设备管理 | ⏳ 未开始 | — |
| M4 生产化 | ⏳ 未开始 | — |
| M5 发布 | ⏳ 未开始 | — |
