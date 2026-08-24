# EdgeFlow 审计缺口修复 — 独立代码复核报告

- 复核日期：2026-08-14
- 复核人：独立复核员（资深 Go 工程师视角，只读复核，未修改任何代码）
- 复核范围：G1–G7 中本轮处理项（G3 可观测性 / G4 RBAC·审计 / G7 NodeJob 关闭 / G2 镜像漂移 / G1 E2E 套件 / G5 压测 / P3 文档同步 / B5 多架构 / G6 真实集群）
- 对照文档：docs/CLOSE-OUT-REPORT.md
- 复核基线 commit：ac3913f（HEAD）

## 审查状态总览

| 缺口 | 主题 | 状态 | 结论 |
|------|------|------|------|
| G3 | 可观测性（metrics） | ✅ 已复核 | 通过（P2×3） |
| G4 | RBAC/审计（auth + audit） | ✅ 已复核 | 通过（P2×2） |
| G7 | NodeJob 关闭 | ✅ 已复核 | 通过（代码侧）；文档传播不完整（P2） |
| G2 | 镜像漂移 | ✅ 已复核 | 通过（P2×3） |
| G1 | E2E 套件 | ✅ 已复核 | 通过（真实进程+真实 Docker，完整套件实测三用例全 PASS；但台账路径未隔离导致污染仓库，P1×1） |
| G5 | 压测 | ✅ 已复核 | 有条件通过（P1×1：BEAT_SEC 死代码） |
| P3 | 文档同步 | ✅ 已复核 | 有条件通过（P1×1：ROADMAP/PROGRESS 状态列滞后于 HEAD） |
| B5/G6 | 多架构/真实集群 | ✅ 已复核 | 通过（文档证据充分；本机 registry 已清理无法复验，P2） |

## 验证命令记录（实际执行）

```
go build ./...                                     → 通过（exit 0）
go vet ./cloud/pkg/{metrics,auth,audit}/... \
     ./edge/pkg/edged/... ./tests/e2e/... \
     ./hack/load-test/... ./cmd/cloudcore/...      → 通过（0 问题）
go test -race -cover ./cloud/pkg/metrics/...       → ok，96.6%
go test -race -cover ./cloud/pkg/auth/...          → ok，94.1%
go test -race -cover ./cloud/pkg/audit/...         → ok，76.3%
go test -race -cover ./edge/pkg/edged/...          → ok，87.4%
go test -race -cover ./cmd/cloudcore/...           → ok，83.0%
golangci-lint run（6 包）                          → 0 issues
go test -short ./tests/e2e/                        → ok，199.4s（设备链路真实进程用例实跑）
go test -v ./tests/e2e/（完整套件，真实 Docker）   → ok 199.3s：自治 127.2s / 设备 35.5s / 多节点 36.4s 三用例全 PASS（详见 §5）
go build ./hack/load-test/...                      → 通过
```

## 各维度详细审查

### 1. Metrics（cloud/pkg/metrics，G3 / WBS 10.1）— 通过

**Prometheus 文本格式合法性**：✅ 输出 `# HELP` / `# TYPE` / `name value` 结构正确；
标签值用 `%q` 引号包裹并实现 escapeLabel（`\` `"` `\n` 转义）；Content-Type
`text/plain; version=0.0.4; charset=utf-8` 正确。gauge 按指标名排序、counter 按
(path, code) 排序，输出确定性。实测 /metrics 200 + 5 指标齐全（单测 + 装配级
TestRunMetricsEndpoint 双覆盖）。

**指标语义**：✅ gauge/counter 使用正确（节点/容器/连接数 = gauge，请求累计 =
counter，按 路由模式+状态码 分桶，低基数设计正确——nodeID 不进入标签）。
⚠️ P2-1：`*_total` 后缀是 Prometheus 计数器的命名约定，`nodes_total/pods_total/
devices_total` 三个 gauge 以 `_total` 结尾，会误导采集端（rate() 误用风险）。
建议改名（如 `edgeflow_cloudcore_nodes`）或文档化偏差。

**并发安全**：✅ `sync.Mutex` 保护计数 map 与渲染；渲染先到 buffer 再一次性写出
（不长时间持锁阻塞计数）；Provider 注入函数在锁内调用（provider 独立实现、无
重入，安全）。`-race` 全绿。

**middleware 计数正确性**：✅ statusRecorder 语义与 net/http 一致（隐式 200、
重复 WriteHeader 忽略）；r.Pattern 低基数回退 URL.Path（404）；方法前缀剥离；
TestMiddlewareCounts 断言 3 桶计数精确。装配顺序（最外层 metrics → mux →
audit → auth）实测计数正确（含 /metrics 自计数、401 计入）。
⚠️ P2-2：statusRecorder 未实现 http.Flusher/Hijacker/Pusher——当前全部业务
handler 均不需要，属标准中间件局限，记录备查。
⚠️ P2-3：counter 的 HELP/TYPE 仅在首个请求后出现（空 map 不输出）——合法，
Prometheus 无数据即无序列，可接受。

### 2. Auth（cloud/pkg/auth，G4 / WBS 7.2）— 通过

**常数时间比较**：✅ `subtle.ConstantTimeCompare`；空 presented / 空 token 在
比较前短路拒绝（避免长度 0 边界）。长度差异泄露在注释中声明为可接受范围。

**默认 off 向后兼容**：✅ 仅 `EDGEFLOW_CLOUDCORE_AUTH=on` 启用；装配级测试
TestRunAuthOffBackwardCompat 验证无令牌 200。现有部署/测试不受影响。

**401 响应**：✅ 401 + `WWW-Authenticate: Bearer realm="cloudcore"` + JSON 错误体；
测试覆盖无令牌/错令牌/非 Bearer/空令牌/大小写 5 种拒绝路径。

**env 解析**：✅ 开启但令牌缺失 → fail-fast 拒绝启动（exit 1，测试覆盖）；
/healthz 与 /metrics 不参与认证（探活/采集通道不因缺令牌挂掉，合理）。
单令牌即管理员语义在包注释中明确声明（v0.1 范围；多角色授权留后续）。

### 3. Audit（cloud/pkg/audit，G4 / WBS 7.5）— 通过

**JSONL 追加原子性**：✅ O_APPEND + 单次 Write(record+'\n') + mutex 串行化 →
单进程内每记录原子追加；重启重新打开继续追加（TestReopenAppends 验证不覆盖）。

**字段完整性**：✅ {ts, action, path, method, status, operator, ip} 全量填充；
action = 方法 + 路由模式（低基数），path = 实际路径；身份槽机制（context 指针
槽）让认证中间件就地写入身份、外层审计读取，401 也落盘 operator=anonymous——
安全审计不丢未授权尝试（装配级测试断言 401/200 双留痕）。

**失败降级**：✅ 审计写失败仅 log.Errorf、不阻断 API；nil ledger 防御性透传
（测试覆盖）。注：台账**初始化失败**（路径不可写等）→ cloudcore 拒绝启动
（fail-fast，注释声明为安全控制意图）——设计自洽：运行期写失败降级、启动期
配置错误不静默。
⚠️ P2-4：审计为请求路径同步文件写，高 QPS + 慢盘时增加尾延迟；v0.1 管理 API
量级可接受，属已文档化的可用性优先取舍。IP 取 RemoteAddr 去端口、不信代理头
（安全姿势正确）。

### 4. 镜像漂移（edge/pkg/edged，G2 / WBS 6.4）— 通过

**Inspect 比对语义**：✅ `docker inspect --format {{.Config.Image}}` 返回创建时
镜像引用，与 pod.Image 精确比对——两侧同为 tag 引用格式时语义正确；容器不存在
→ (false, nil) 不算漂移；daemon 不可用 → 错误透传（测试覆盖 4 分支）。
⚠️ P2-5：同 tag 重新指向新 digest（期望值不变、远端 tag 已更新）无法检出；
digest 引用未归一化——注释已声明为 POC 简化，生产需 digest 级比对。

**重建流程**：✅ Running+漂移 → EnsureStopped（rm -f）→ 递归 EnsureRunning
（Absent → run -d）；调用顺序单测断言 inspect→inspect→rm→inspect→run；
run 名字冲突 → 重新 Inspect 按实际状态处理（竞态兜底）。
⚠️ P2-6：单副本重建是 rm→run 停机窗口（非蓝绿）；replicas=1 时存在短暂中断，
replicas>1 时批大小 1 的滚动缓解。已注释声明，v0.1 可接受。

**滚动顺序**：✅ reconcile 3c 循环按 index 0→N-1 顺序检查，driftRebuilt 标志
保证每轮最多重建 1 个漂移副本；TestEdgedReconcileRollingRebuild 精确断言 3 副本
逐轮 0→1→2 重建、第 4 轮无动作；重建不计入 RestartCount（主动更新非崩溃重启，
测试断言）。Stopped 态先 start、镜像检查留到下一轮（≤2 轮收敛，语义对齐测试）。

**MockRuntime 同步**：✅ images map + SetImage 注入漂移 + "未记录即一致"兼容
旧测试语义；镜像漂移重建计数（EnsureStopped/Create 同增）与真实实现对齐。

### 5. E2E 套件（tests/e2e，G1 / WBS 8.3）— 通过（实测全绿）

**测试真实性**：✅ 真实装配路径——go build 编译 cloudcore/edgecore 二进制以
子进程运行，真实环境变量覆盖（端口/DB 路径/调谐周期 300ms/MQTT 快速失败），
HTTP API + 真实 docker CLI 断言；用例隔离（唯一后缀 Pod 名防并发/残留进程干扰、
reservePort 独立端口、每用例临时 DB、节点 ID 独立）。`-short` 模式 dockerOK/
镜像缓存检查后跳过容器用例（CI 友好）。

**waitContainerGone 修复**：✅ 以 `err != nil` 作为"已删除"判据（docker inspect
对不存在容器返回非零退出码）；旧实现看输出为空会把 stderr 误判为仍在——修复
正确且是删除收敛断言超时的根因修复。

**三用例**：
- TestAutonomyCloudDisconnect：60s 短窗模拟 30min 自治语义（判定逻辑一致，
  文档声明时长差异）；停云→容器仍运行→重启云（复用原端口）→节点重注册→
  状态恢复同步→删除收敛。✅
- TestDeviceCommandReportE2E：指令下发→影子→上报→云端可见闭环，Desired/
  Properties/单节点视角三重断言（无 Docker 依赖，默认执行）。✅
- TestMultiNodeRegistrationAndPodSync：双节点独立注册/下发/隔离断言
  （nodeA 列表只含 podA），删除收敛。✅

**超时设置**：✅ 全部等待有界（节点注册 60s、Pod phase 90s、容器 60s、进程
优雅停止 SIGINT→15s→SIGKILL→5s 兜底），套件 timeout 20m。
⚠️ P2-7：自治用例固定 60s 墙钟 sleep——测试时长下限，属文档化取舍。
⚠️ P2-8：多节点用例为 2 节点，"10+ 节点 E2E"仍未覆盖（ROADMAP 8.2 缺口
依旧成立，指南 §10 边界如实声明）。
⚠️ **P1-3**：E2E 套件未隔离审计台账路径——`cloudEnv()`（e2e_test.go）只设置
PORT/HUB_PORT，cloudcore 子进程落默认 `data/audit-ledger.jsonl`（相对 CWD =
tests/e2e 目录）→ 每次运行套件都会写入/污染仓库内 `tests/e2e/data/
audit-ledger.jsonl`（复核时实测：一次 `go test ./tests/e2e/` 即在该已提交
文件上追加了数百行）。这正是仓库里该文件存在且无人引用的原因；与套件自身
“用例互不干扰、不残留”的设计相違。修复：`cloudEnv()` 里设置
`EDGEFLOW_CLOUDCORE_AUDIT_PATH` 指向临时目录（与 cmd/cloudcore
main_security_test.go 同法），并删除已提交的死文件。

### 6. 压测（hack/load-test，G5 / WBS 8.4）— 有条件通过

**注册判定**：✅ `NodeName() != ""` 作为注册成功判据正确（nodeName 仅在
RegisterAck 时写入，见 edgehub/client.go:458）；注释解释了为何不能用
LastHeartbeatAckTime（30s 周期 > 短窗口）。

**并发安全**：✅ 每节点 goroutine，results 在 mutex 下 append，WaitGroup 汇合；
`-race` 全绿（含依赖包）。

**统计准确性**：✅ 成功率/平均/P95（排序近似，10 节点规模足够）；心跳统计用
LastHeartbeatAckTime 增量差；PERFORMANCE-BASELINE.md 记录基线 10/10 注册、
201ms 平均/202ms P95，与验收差距（100 节点、内存、消息延迟）如实披露。
⚠️ **P1-1**：`LOAD_TEST_BEAT_SEC` 环境变量被解析（beatSec）但**从未使用**——
edgehub.Options.HeartbeatInterval 未设置（仍 30s 默认），PERFORMANCE-BASELINE
§3 却宣称该变量可调心跳间隔（默认 2s）。死代码 + 文档误导：要么
`Options.HeartbeatInterval = beatSec * time.Second` 接入，要么删除变量与文档
声明。当前工具在短窗口内无法统计心跳（与基线"心跳 0"一致，但"可配置"承诺
不成立）。
⚠️ P2-10：`fmt.Sscanf` 忽略错误——非法 env 值静默回退默认值；`LOAD_TEST_NODES=0`
时 SuccessRate = 0/0 = NaN，-json 输出 JSON 序列化失败（错误被忽略输出为空）。
边界情形，建议校验 nodes ≥ 1。

### 7. 文档同步（P3，commit b809e30）— 有条件通过

**已做的（属实）**：ROADMAP §0/§1.1/§1.2 约 52 行回写（核对基线从"全部 ⬜"
更新为 audit-m02/m35 结论，含里程碑归属偏移注记）；PROGRESS §5 backlog 清理
（已完成项移除）+ §6 里程碑状态从"全部未开始"矛盾态回写；KEADM 标题与命令表
补升级/回滚。

**不一致（抽查 3 处，结论：文档状态列滞后于 HEAD 代码）**：
1. **7.2/7.5/10.1**：ROADMAP §1.2 仍标 ⬜ 未实现（7.2"全仓无 RBAC/authz 代码"），
   PROGRESS §5 仍列"WBS 7 缺失项：7.2 RBAC/7.5 审计日志…10.1 可观测性…8.4 压测"。
   ⚠️ 其中 7.2/7.5/10.1 在 commit 4c5b9c6（21:03）已实现，而 b809e30（21:04）
   的回写发生在**之后**——时间线上无法辩解，属于漏同步。
2. **6.4/8.3/8.4**：ROADMAP §1.2 仍标 6.4"镜像更新滚动策略未实现"、8.3 ⬜、
   8.4 ⬜；PROGRESS §6 M2/M4 行仍称"P1 残留：8.3 E2E 完整场景 + 6.4 滚动策略"
   "7.2/7.5/10.1/8.4 未做"。a0a4344（21:30）晚于 b809e30，时序上可解释，
   但 **HEAD 处文档与代码不一致是事实**，需补一次回写。
3. **2.8 NodeJob**：代码侧占位已标"已关闭：v0.1.0 范围外"（4c5b9c6），但
   ROADMAP §1.2/PROGRESS §5 仍写"协议仅占位'待定'，需明确关闭或排期"——
   关闭决策未传播到跟踪文档。
另：REAL-CLUSTER-GUIDE §10 边界"压测未做"在 HEAD 亦滞后（a0a4344 已补）。

### 8. 多架构（B5）+ 真实集群（G6，commit b809e30）— 通过（证据为文档级）

**B5**：✅ images.json 记录双架构 OCI manifest digest（cloudcore
sha256:6d01b329…，amd64 84fd1e72…/arm64 373fba1c…；edgecore 同），含 buildArgs
与 verifiedBy（manifest inspect + 双平台 docker run --version 一致 + kind 实测）。
MULTIARCH.md 含完整可复现命令（buildx + 本地 registry 5001 + buildkitd.toml）。
⚠️ P2-11：复核时本机 127.0.0.1:5001 registry 未运行、镜像不存在——B5 证据
不可独立复验（指南 §9 清理步骤已删 registry，属预期，但"已验证"只能采信文档）。
多架构构建未进 CI（release.yml 为 GitHub Actions，本机无法验证）。

**G6**：✅ REAL-CLUSTER-GUIDE.md 详实：kind v0.27.0 + k8s v1.32.2，CRD apply +
schema 校验（合法过/非法拒）、keadm init → rollout 1/1、healthz 200、edgecore
注册 Ready ≈2min（含分步耗时表），M5"15min 验收"成立；边界诚实（kubectl get
nodes 口径为 REST 注册表不生成 K8s Node、macOS NodePort 限制用 port-forward、
mTLS 跨主机 CA 未自动化）。config/crd/ 3 个 manifest 新增。
⚠️ P2-12：复核环境无 kind 集群，CRD manifest 与部署步骤未复跑——依赖文档
自证；CRD 校验逻辑建议后续在 CI 加 `kubectl apply --dry-run=server` 冒烟。

## 结论与问题清单

**总体结论：有条件通过（条件 = 修复 P1×3：BEAT_SEC 死代码 + 文档回写 + E2E 台账隔离）**

代码质量整体高：build/vet/race/lint 全绿，覆盖率高（metrics 96.6% / auth 94.1%
/ audit 76.3% / edged 87.4% / cloudcore 83.0%），单测断言精确（调用顺序、计数、
收敛轮次），装配级测试验证真实中间件链，E2E 为真实进程+真实 Docker 且实测全绿。
三个 G1-G5 功能缺口与 G7 占位标注实现正确；主要问题集中在**文档状态列滞后**
（P3 同步不彻底）、**压测工具一处死代码**、**E2E 套件审计台账路径未隔离**
（污染仓库工作树，复核时实测复现）。

### P0（必须修复）
无。

### P1（应该修复）
1. **hack/load-test：LOAD_TEST_BEAT_SEC 死代码**——解析后未接入
   edgehub.Options.HeartbeatInterval；PERFORMANCE-BASELINE §3 宣称可调（默认
   2s）与事实不符。修复：接入或删除变量+文档声明。
2. **文档回写滞后（HEAD 不一致）**——ROADMAP §1.1/§1.2、PROGRESS §5/§6 中
   7.2/7.5/10.1（4c5b9c6 已实现且早于 b809e30）与 6.4/8.3/8.4（a0a4344 已
   实现）仍标未实现/未做；2.8 NodeJob 关闭决策未传播。需补一次同步 commit。
3. **E2E 套件污染仓库**——cloudEnv() 未设置 EDGEFLOW_CLOUDCORE_AUDIT_PATH，
   cloudcore 子进程把审计台账写进 tests/e2e/data/audit-ledger.jsonl（已提交
   的死文件）；每次运行套件都改动工作树。修复：临时目录隔离 + 删除死文件。

### P2（建议改进）
1. metrics：三个 gauge 以 `_total` 后缀命名违反 Prometheus 命名约定（rate 误用
   风险），改名或文档化。
2. metrics/audit：statusRecorder 未实现 Flusher/Hijacker（当前无影响，备查）。
3. metrics：counter HELP/TYPE 在零请求时不输出（合法，可接受）。
4. audit：请求路径同步文件写，高 QPS 慢盘有尾延迟（v0.1 可接受，已文档化）。
5. 镜像漂移：同 tag 换 digest 无法检出；digest 引用未归一化（已注释声明，生产
   需 digest 比对）。
6. 镜像漂移：单副本重建 rm→run 有停机窗口（非蓝绿；N>1 时批大小 1 缓解）。
7. E2E：自治用例固定 60s 墙钟 sleep（测试时长下限，文档化取舍）。
8. E2E：多节点用例 2 节点，10+ 节点未覆盖（8.2 缺口仍成立）。
9. load-test：env 解析忽略错误；NODES=0 → NaN SuccessRate + -json 序列化失败，
    建议校验 nodes ≥ 1。
10. B5：本地 registry 已清理，双架构证据不可复验；建议 CI 化多架构构建冒烟。
11. G6：CRD manifest 未在复核环境复跑；建议 CI 加 `kubectl apply --dry-run=server`。

## 附：抽查点与代码定位

- auth 常数时间比较：cloud/pkg/auth/auth.go validBearer（subtle.ConstantTimeCompare）
- 审计失败降级：cloud/pkg/audit/audit.go Middleware（log.Errorf 不阻断）
- 401 留痕：cmd/cloudcore/main.go apiHandler = auth.Middleware → ledger.Middleware 顺序
- 漂移重建顺序：edge/pkg/edged/docker_runtime.go EnsureRunning Running 分支
- 滚动批大小：edge/pkg/edged/edged.go 3c 循环 driftRebuilt 标志
- waitContainerGone 修复：tests/e2e/e2e_test.go（err != nil 判据）
- 注册判定：hack/load-test/main.go（NodeName() != ""）
- NodeJob 占位：pkg/protocol/message.go TypeNodeJob/TypeNodeJobResult（已关闭标注）
