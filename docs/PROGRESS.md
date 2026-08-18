# EdgeFlow 项目进度台账（PROGRESS）

> 最后更新：2026-08-18 19:10 (Asia/Shanghai)
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
| M4 | 生产化与规模化 | 🟨 **主体+收尾完成** | +升级回滚（10.2）| commits 2386fa3~4c52f4a，见 §4K/§4L |
| M5 | MVP 发布与文档交付 | 🟨 **发布完成** | v0.1.0 Release Notes/制品/镜像/台账 + P2 闭环 + 演练排期 | commits 28ce6ec~866f796，见 §4M |

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

## 4L. M4 收尾 + M5 前置验证结果（升级回滚/文档示例/P2 处置，2026-08-14）

### 新增模块
| 模块 | 提交 | 内容 |
|------|------|------|
| keadm upgrade/rollback | 7aa035c | 备份模型（backups/<ts>/manifest+sha256）+ 台账（ops-ledger.jsonl 追加写）+ --simulate-failure 演练 + 异常三类兜底 |
| keadm ops-ledger | 7aa035c | 操作台账查询（时间/版本/结果/操作人，limit 可配） |
| P2×6 处置 | a1c619d+4f8eeab | ①TLSSAN fail-fast ②manifest 校验和 ③unit ID+连接上限 ④关闭 ⑤舍入 ⑥延后登记 |
| M5 文档定稿 | f090a30 | 9.2-9.5 四文档（2023 行）+ examples/demo.sh（DEMO PASS×3）+ REVIEWS.md |
| 协调点修复 | eabb360+4c52f4a | refreshManifest 合并式更新、defaultNodeID 清洗、P2-1 台账一致性、P2-3 回归测试 |
| M4D P2×2 处置 | fe093e1 | P2-4 restoreBackup 事务化（staging+原子替换）、P2-5 备份清单白名单校验（见下） |

### 端到端验证
| 场景 | 实测结果 |
|------|---------|
| 升级→失败→回滚演练 | ✅ --simulate-failure（台账 failed、产物未动）→ rollback --latest 恢复，diff 一致 |
| 数据不丢 | ✅ 预置 SQLite/用户文件在升级回滚全程 hash 一致 |
| 异常三类 | ✅ 升级中途失败（台账 failed+提示）/回滚失败（备份保留+cp 提示）/产物缺失（明确错误） |
| 台账可追踪 | ✅ ops-ledger 4 条（failed/ok/ok/ok），KEADM_OPERATOR=alice 记录 |
| 协调点 | ✅ init→join→upgrade→reset 全链路（reset 删 7 产物、清单合并不丢 init 记录） |
| Demo 示例 | ✅ demo.sh 干净环境 DEMO PASS×3（复核第 3 次含 MQTT 段），0 残留 |

### 自动化验证
| 项目 | 结果 |
|------|------|
| go test -race（24 包） | ✅ 全绿，覆盖率 77.8%（keadm 74.6%） |
| golangci-lint | ✅ 0 issues |
| 审查（docs/CODE-REVIEW-M4D.md） | ✅ 有条件通过（0 P0 / 0 P1 / 5 P2）→ P2×5 全部修复（P2×3 + P2×2，见下） |

### M4D P2×2 处置（2026-08-14，commit fe093e1）
| # | 问题 | 结论（一句话） | 证据 |
|----|------|----------------|------|
| P2-4 | restoreBackup 非事务性（中途失败留混合状态） | 已修复：先复制到 staging（.restore-staging-<pid>/）全部回读校验通过后 os.Rename 原子替换（同目录同设备），任一失败清理 staging、备份保留、目标文件不被部分覆盖；权限恢复在 staging 阶段完成（env 0600 / service 0644 / install.sh 0755） | restoreBackup（cmd/keadm/rollback.go）+ TestRestoreBackupTransactional / TestRestoreBackupFailureAtomic |
| P2-5 | manifest 文件清单无白名单校验（纵深防御） | 已修复：findBackup/listBackups 加载时校验 Files 仅含 {edgecore.env, edgecore.service, install.sh}，未知条目（绝对路径/路径穿越）拒绝加载并报「备份清单含未知文件」；restore 只恢复白名单文件 | backupWhitelist/invalidBackupFile（cmd/keadm/rollback.go）+ TestFindBackupRejectsUnknownManifestFiles / TestRollbackRejectsMaliciousManifest / TestListBackupsWarnsUnknownManifest / TestRestoreBackupSkipsNonWhitelist |

> 验证：go build/vet ✅、go test -race -count=1 ./cmd/keadm/... ✅（覆盖 75.6%）、golangci-lint 0 issues；现有升级/回滚测试全部保持绿色。

## 4M. M5 正式发布验证结果（v0.1.0，2026-08-14）

### 发布产物
| 模块 | 提交 | 内容 |
|------|------|------|
| Release Notes | 28ce6ec | 中英双语 278 行：变更/已知问题（22 条）/升级/回滚 |
| 发布台账 | 28ce6ec+1265214+866f796 | 时间线/操作人/制品清单（实际 digest 回填）/验证/异常 |
| 制品归档 | d17bdd5+733d0ae | release/v0.1.0/：6 二进制 + Chart 包 + checksums + sbom.json（33 组件）+ images.json |
| 镜像 | d17bdd5 | 本地 registry 闭环：不可变 tag v0.1.0 + digest 记录 + pull 复验 |
| P2×2 闭环 | fe093e1+ef21a98 | restoreBackup 事务化（staging 原子替换）+ manifest 白名单（路径穿越拒绝） |
| 演练排期 | 28ce6ec | docs/DRILL-SCHEDULE.md：窗口【需确认】/范围/参与人/前置条件/回滚预案/通知模板 |
| P1-1 处置 | 733d0ae+866f796 | 独立复核发现 keadm 不含 P2 修复 → HEAD 重建 + checksum/SBOM 重算 + Notes/台账对齐 |

### 独立复核（docs/RELEASE-REVIEW.md，166 行）
- 结论：⚠️ 有条件通过（7/10 通过、2 有条件、1 不一致）→ **P1-1 已闭环**（keadm 重建含修复，gitCommit=1265214）+ P2-1/P2-2 澄清回填 + P3 观察项清理
- 复核证据：checksum 独立重跑 7/7、SBOM 33 组件与 go list 一致、Chart 与源码逐字节一致、回滚方案四路径一致

### 验证结果
| 项目 | 结果 |
|------|------|
| go test -race（24 包） | ✅ 全绿 |
| golangci-lint | ✅ 0 issues |
| checksum 复验 | ✅ 7/7 OK |
| keadm 版本 | ✅ v0.1.0（gitCommit=1265214 含 P2-4/P2-5 修复） |

### 环境边界（明确标注）
- 无远程制品/镜像仓库凭据：本地归档 + 本地 registry 闭环，远程推送步骤在 RELEASE-CHECKLIST.md（CI release.yml 已就绪）
- 生产演练仅排期建议（无运维团队确认窗口，DRILL-SCHEDULE.md 标注【需确认】）

## 4N. v0.2.0 开发轮验证结果（资源调度/Mapper 装配/Modbus ns/P3 收尾，2026-08-18）

> 基线 HEAD：`c906877`（v0.1.1 发布轮收官）→ 本轮 6 个主题 commit（`9cf7772`/`5cb7336`/`238b0cc`/`566aff9`/`3e0d1ff`/`d3f09fe`）。派单与复核证据见 `.cluster/a1c29599/`（plan.md/review.md/fix_report.md/fix2_report.md/smoke_report.md）。

### 范围决策（4 开发项 + 明确不做项）

| # | 项 | 出处 | 决策 |
|---|----|------|------|
| A | 6.5 资源调度基础（P2 全量）：request/limit、超卖率校验与拒绝、漂移检测重建 | ROADMAP §8「6.5 调度/超卖 P2」 | ✅ 开发 |
| B | Mapper 框架 EventBus 装配开关（edgecore 启动链路装配 MapperRegistry） | PROGRESS §5「Mapper 未接入 EventBus」 | ✅ 开发 |
| C | ModbusMapper DeviceNamespaceResolver（namespace 感知路由） | 代码复核 minor（S6 入 ROADMAP 登记） | ✅ 开发 |
| D | P3 小项收尾：pkg/log SetLevel；M1 通道 P3×4（newID/Shutdown 撞 Start/被踢标志/退避重置） | PROGRESS §5 P3 待办 | ✅ 开发 |
| — | OPC-UA 适配器（WBS 5.2） | ROADMAP §8 | ⏳ 独立立项：零依赖手写 UA 协议栈 ≥30 人天，本轮只登记不开发 |
| — | cosign / 多节点集群 / 100 节点压测 / 30min 长跑 | 环境边界 | ⏸ 与上轮一致，不变 |

**版本决策**：功能增量 → v0.2.0（minor），发布轮复用 v0.1.1 的发布保障三件套模板。

### 4 路派工（并行无依赖）

| worker | 角色 | 产物 |
|--------|------|------|
| worker-res-sched | A 资源调度功能开发 | subagent_01（超时无报告，复核员人工补审） |
| worker-mapper-wire | B Mapper 装配开发 | subagent_02.md |
| worker-modbus-ns | C Modbus 接口补齐 | subagent_03.md |
| worker-p3 | D P3 收尾 | subagent_04.md |

### 代码复核（.cluster/a1c29599/review.md）

- 结论：**有条件通过 → 0 blocker / 1 major / 7 minor / 5 nit**；通过条件：Major-1 必须先修
- Major-1：ParseCPU 接受 Inf/NaN/超 int64 毫核范围（fail-open，可绕过超卖校验）→ **已修**（拒绝 NaN/Inf/溢出 + 前导 `+`，补表驱动测试）
- 数字口径核验 ✅：超卖率计算（4 核×150%=6000m）、request≤limit 跨单位比较、request 求和含副本乘数、update 排除同名同 ns 旧值、拒绝路径错误码（400/409/502）均有测试锁定

### 缺陷修复两批

| 批次 | 内容 | 结果 |
|------|------|------|
| 复核修复（fix_report.md） | P0×2（ParseCPU NaN/Inf/溢出 + 超卖率 env 非有限值回退）、P1×2（409 响应 json.Marshal、舍入口径统一 math.Round）、P2×2（手写函数换 stdlib strings、单边畸形值 fail-closed）+ 测试缺口 3 处补测；退避测试 m3 跳过（需生产代码注入点） | ✅ 完成 |
| 冒烟修复（fix2_report.md） | 冒烟 1d FAIL：resourcesMatch 只挂在 DockerRuntime.EnsureRunning 内部，reconcileOnce 3c 对健康 Running 容器从不进入 → 资源漂移检测是死代码（docker update 后 60s 不重建）。修复：3c 分支 ImageMatches 一致后再查 ResourcesMatch，任一不匹配走同一 stop+重建路径，沿用每轮最多重建 1 个的滚动门控；ResourcesMatch 导出单点实现 + MockRuntime 补齐资源状态注入 | ✅ 完成（commit `d3f09fe`） |

### 全量回归

| 项目 | 结果 |
|------|------|
| go build / go vet / gofmt | ✅ 全部通过 |
| go test -race（全仓） | ✅ 全绿 |
| tests/e2e 双用例（设备链路 + 自治/多节点） | ✅ 全绿（真实进程 + 真实 Docker，约 204s） |
| golangci-lint | ✅ 0 issues |

### 预发冒烟（.cluster/a1c29599/smoke_report.md，真实进程 + 真实 Docker，8/8 PASS）

| # | 冒烟项 | 结论 |
|---|--------|------|
| 1a | 资源 limit 端到端：pod 带 resources 下发 → 容器实际带 --cpus/--memory（docker inspect NanoCpus/Memory 核验） | ✅ PASS |
| 1b | 超卖拒绝：超出节点资源 → 云端 409 `EDGEFLOW_RESOURCE_EXHAUSTED`，拒绝不落盘不建容器 | ✅ PASS |
| 1c | request>limit → 云端 400（CPU/内存分别验证，容器未创建） | ✅ PASS |
| 1d | 资源漂移：docker update 改 limit → 调谐重建（修复 commit `d3f09fe` 后复验） | ✅ PASS |
| 2a | Mapper 默认开启：DeviceCommand 执行 + 周期上报链路 | ✅ PASS |
| 2b | `EDGEFLOW_EDGECORE_ENABLE_MAPPER=false`：影子模式（仅更新 Twin.Desired） | ✅ PASS |
| 3 | Modbus namespace（`EDGEFLOW_MODBUS_NAMESPACE=plant-a`）路由隔离（真实模拟器端到端 + 注册表级双证据） | ✅ PASS |
| 4 | pkg/log SetLevel（按任务约定降级为库级验证） | ✅ PASS |

## 5. 待办项（Backlog）

> 2026-08-14 收尾审计清理：已完成项已移除（Helm 骨架✅、edgecore 占位测试✅、M2 完整化已做部分✅、M3 MQTT EventBus✅、cmd-edgecore 覆盖率✅ 74.6%）；并新增 audit-m02/audit-m35 缺口跟踪项。完整缺口分级见 docs/audit-m02.md §4、docs/audit-m35.md §4。

| 优先级 | 事项 | 说明 |
|--------|------|------|
| ~~P2~~ ✅ | 2.7 配置热重载（CloudCore/EdgeCore） | 已闭环（2026-08-15 后续开发轮）：SIGHUP + 60s mtime 轮询；cloudcore 端口热切换（httpReloader.swapPort，绑定失败回滚）；edgecore 上报周期热生效、cloudAddr/nodeID/reconcileInterval 回写需重启；fail-safe 保持旧配置；单测覆盖（commit `2d0a903`） |
| ~~P2~~ ✅ | M1B/M1C/M1/M3A 审查遗留 P2/P3 | 已闭环（2026-08-15 后续开发轮）：metamanager List LIKE 通配改范围扫描（M1B P2-1，`37fbaf4`）；registry Offline TTL/GC 默认 24h 配置化（M1B P2-3，`37fbaf4`）；devicestatus 无 TTL/GC 结论记录（M3A，`37fbaf4`）；serveWS 与 Shutdown 竞态 + wg.Wait 5s 超时（M1 P2-1，`d9bc9ec`）；写 API 1MiB 请求体限制 413（M1C P2-5，`d9bc9ec`）；newID rand 失败 fallback（M1 P3-1，`d9bc9ec`）；基线核查确认已修复：同 ID pending 交叉清理（P2-1）/ErrAckFailed→502（P2-2）/SetReadLimit（P2-2）/Memory uint64（P2-3） |
| ~~P2~~ ✅ | 9.1 架构文档评审 + 内容回写 | 已闭环（2026-08-15 后续开发轮）：ARCHITECTURE.md 评审记录入档，NATS→MQTT、Token→mTLS 演进、进度回写至 2026-08-15，残留缺口登记 §12（commit `1ae2d81`） |
| ~~P2~~ ✅ | 7.1 证书轮换自动化（keadm cert rotate） | 已闭环（2026-08-15 后续开发轮）：pkg/certs 备份先行+事务化重签+幂等，keadm cert rotate --node/--cert-dir，实操冒烟验证（序列号变化+备份 manifest，commit `2c877d4`） |
| ~~P2~~ ✅ | 10.2 灰度发布（keadm upgrade 分批） | 已闭环（2026-08-15 后续开发轮）：--batch-size（默认 1）/--pause-between（默认 0），fail-fast 中止+成功/失败清单+rollback 衔接，batch 透传，fake 执行器单测（commit `2c877d4`） |
| ~~P2~~ ✅ | 4.4 gzip 消息压缩 | 已闭环（2026-08-15 后续开发轮）：Register 协商式双向压缩（EFGZ 帧、1MiB wire/明文双限、<256B 回落、四象限互操作；评审 B1 协商断裂修复，commit `37f34f9`+`5ac07f8`） |
| P1 | GitHub 远程仓库关联 | 用户把 `~/.ssh/id_ed25519.pub` 粘贴到 GitHub → `git remote add origin` → push（步骤见 ENV-SETUP.md §4.2） |
| P1 | 推送后验证 CI 首次运行 | push 后 Actions 标签页应显示 lint+test 绿勾（M0 验收"CI PR 反馈 ≤10min"至今未实证） |
| ~~P1~~ ✅ | 8.3 E2E 完整场景 | 已关闭：tests/e2e 三用例全 PASS（自治 60s 短时模拟/设备链路/多节点隔离），commit `a0a4344`；30min 时长需真实环境长跑（PERFORMANCE-BASELINE.md 说明） |
| ~~P1~~ ✅ | 6.4 镜像更新滚动策略 + 回滚 | 已关闭：镜像漂移检测+重建（批大小 1 逐轮滚动），单测 5 项全过，commit `a0a4344`；回滚经 re-deploy 语义覆盖 |
| P1 | M1/M2 验收"kubectl get nodes / kubectl apply"字面未达成 | 无真实 K8s 接入，验收以 REST API 适配；需决策：修订验收口径或排期真实 K8s 接入（audit-m02 §4 P1） |
| ~~P1~~ 🔒 | 2.8 NodeJob 未实现 | 已关闭（产品决策）：v0.1.0 范围外，协议占位标注"已关闭"，commit `4c5b9c6` |
| ~~P2~~ ✅ | WBS 7/10 缺口：7.2 RBAC / 7.3 设备认证 / 7.5 审计日志 / 10.1 可观测性 / 8.4 压测 / 10.3 API 兼容矩阵 | 已全部闭环：7.2（`4c5b9c6`）、7.5（`4c5b9c6`）、10.1（`4c5b9c6`）、8.4（`a0a4344`）；**7.3 设备认证**：Register.token 云边双向 + EDGEFLOW_CLOUDCORE_NODE_TOKEN 校验（本轮，单测 6 项）；**10.3 兼容矩阵**：docs/API-COMPATIBILITY.md（本轮） |
| P2 | 9.1 架构文档评审 + 内容回写 | ARCHITECTURE.md 未评审且滞后：NATS→MQTT、Token→mTLS、实现进度至 M5（audit-m02 S11-S13） |
| P2 | 生产多节点集群验证 | kind 单节点真实集群路径已跑通（2026-08-14，CRD apply/部署/注册，≈2min，见 docs/REAL-CLUSTER-GUIDE.md）；多节点网络/存储/证书差异待生产演练（DRILL-SCHEDULE） |
| ~~P2~~ ✅ | keadm 批量操作（batch join/upgrade/rollback） | 已闭环：keadm batch 子命令（清单文件逐节点，复用 join/upgrade/rollback 逻辑），单测 5 项+真实运行验证 2 节点（本轮） |
| ~~P2~~ ✅ | 跨主机 CA 分发自动化 | 已闭环：gen-certs.sh CERT_DIST_DIR 分发包（cloud/ + edge/<CN>/，含 README 部署说明），openssl verify 验证（本轮） |
| ~~P2~~ ✅ | 镜像安全扫描（trivy/grype） | 已闭环：release/v0.1.0 镜像扫描结果见 docs/SECURITY-SCAN.md（本轮） |
| ~~P2~~ ✅ | 6.5 资源管理（调度/资源超卖） | ~~仅 Replicas 伸缩；超卖/调度未做（audit-m02 §4）~~ **已闭环（2026-08-18 v0.2.0 开发轮）**：request/limit 下发与校验、超卖率校验与拒绝（409）、资源漂移检测重建（commits `5cb7336` + `d3f09fe`） |
| P2 | Flannel/边缘容器网络（缺口 6） | M2 交付物从未实现（Docker bridge 方案），需明确关闭或排期 CNI（audit-m02 §4） |
| ~~P2~~ ✅ | 4.4 压缩显式延后登记 | 已闭环（2026-08-15）：gzip 协商式双向压缩已实现（commit `37f34f9`）；Protobuf 编码保留显式延后（规模化阶段，audit-m02 §4） |
| ~~P2~~ ✅ | M3A 审查 P2 项×6 | 已闭环（2026-08-18）：byDevice 路由含 namespace（Route 加 namespace 参数 + DeviceNamespaceResolver 接口，commit `59dd396`）；LastReportedAt 单调性（max() 保护）；多实例 Mapper/空 deviceName/502 Desired 分叉/无 TTL/GC 四项按结论记录代码注释（mock_sensor/twin/devicestatus） |
| ~~P2~~ ✅ | M1C 审查 P2 项×5 | 已闭环（2026-08-18）：context 取消与 handleDownlink 非原子经核验已在前轮修复（ReliableSendContext/downlinkMu），本轮补并发单测；pending 交叉清理与 ErrAckFailed→502 早前已修；云端 operation 校验结论记录 |
| ~~P2~~ ✅ | M1B 审查 P2 项×9 | 已闭环（2026-08-18）：WriteTimeout=15s（newHTTPServer）+ API Encode 失败 Warn 日志 17 处（commit `59dd396`）；Broadcast 送达计数日志（P2-4）；WAL checkpoint 结论记录代码注释；其余 LIKE 转义/Offline TTL 等早前已闭环（2026-08-15） |
| ~~P2~~ ✅ | 多架构发布镜像补 linux-arm64 二进制 | 已闭环：release/v0.1.0/ 新增 cloudcore/edgecore/keadm linux-arm64 交叉编译二进制 + checksums 更新（本轮） |
| ~~P3~~ ✅ | 日志级别过滤（SetLevel） | 已闭环（2026-08-18 v0.2.0 开发轮）：pkg/log SetLevel/GetLevel/Debugf + atomic.Int32 全局级别过滤，默认 LevelInfo 与旧行为逐字节兼容（commit `3e0d1ff`） |
| ~~P3~~ ✅ | M1 通道 P3 项×4 | 已闭环（2026-08-18 v0.2.0 开发轮）：被踢标志即时清除（kick 立即 registered.Store(false)，窗口内心跳改收 not_registered）、Shutdown 撞 Start 窗口防护补测、退避重置两段式断言 + flakyDialer 注入（commit `3e0d1ff`）；newID 兜底经核验前轮已闭环（`d9bc9ec`） |
| P2 | M4C 审查 P2-⑥ :latest immutable / cosign / SBOM | SBOM 已补（M5）；cosign 签名延后：需镜像仓库 + cosign 签名基础设施；风险已登记 MULTIARCH.md §5（处置台账见 4K 节） |

## 6. 里程碑状态

> 2026-08-14 收尾审计回写：此前 M1-M5 全部标"⏳ 未开始"与 §3 模块表矛盾；以下按 audit-m02/audit-m35 实际核对结论更新。

| 里程碑 | 状态 | 说明 |
|--------|------|------|
| M0 架构基线与开发就绪 | ✅ **完成** | 骨架/共享库/CI/CRD 类型/Helm 全部就绪；2 项验收未实证（CI 未运行、CRD 从未 apply，见 audit-m02 §1.1） |
| M1 云边核心通信链路 | ✅ **完成** | 协议/连接/路由/可靠投递/CloudHub/EdgeHub/MetaManager/Edged POC 全达成；3 项归属偏移（2.4/4.5/8.6 实际 M4 完成）；"kubectl get nodes"为 REST 适配 |
| M2 应用部署与边缘自治 | ✅ **验收达成（收尾轮补齐）** | 部署/配置/状态上报/自治 ✅；8.3 E2E 套件 ✅（commit `a0a4344`）+ 6.4 镜像漂移重建 ✅（commit `a0a4344`） |
| M3 设备管理 | 🟨 **第 1-3 轮完成** | DeviceTwin/EventBus/Mapper/流水线/示例 ✅；5.2 Modbus 实际 M4 完成、OPC-UA 未做；端到端延迟 ≤5s 从未测量（audit-m35 §2.1） |
| M4 生产化 | ✅ **验收达成（收尾轮补齐）** | mTLS/升级回滚/Helm/keadm 基础 ✅；7.2/7.5/10.1/8.4 ✅（commit `4c5b9c6`+`a0a4344`）；7.3 设备认证/10.3 兼容矩阵为 P2 残留（2026-08-15 后续开发轮已闭环，见 §5 回写） |
| M5 发布 | 🟨 **发布完成** | v0.1.0 产物齐备（6 二进制+Chart+checksum+SBOM+镜像）；真实集群路径（15min 部署、kubectl get nodes Ready）未执行，已披露为 E2 限制；发布镜像已重建为双架构（2026-08-14，B5） |
