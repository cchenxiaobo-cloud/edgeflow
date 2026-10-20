# EdgeFlow v0.49.0 发布说明（远程运维终端，G31+G11，spec 0022）

## N1 能力面

- **受控运维会话**（`cloud/pkg/termgw`）：云端会话生命周期（创建/列表/关闭 +
  空闲 10min 过期）+ 命令下发与结果关联（execID → pending chan 同步等待，
  等待窗 = timeoutSec+5s）+ 命令级详细审计 JSONL（cmd/args/exitCode/durationMs/
  operator）+ TermResult 归属校验（会话节点 vs 上报节点，v0.48 P2-6 同款加固）。
- **边缘受控执行器**（`edge/pkg/termexec`）：opt-in（`EDGEFLOW_EDGED_TERM=on`
  默认关）；白名单边侧强制（`EDGEFLOW_EDGED_TERM_ALLOWLIST`，默认六个只读
  运维命令 uname/date/uptime/df/free/ps，精确匹配）；**不走 shell**
  （exec.CommandContext 参数独立传递）；超时终止（context kill）；输出 64KB
  截断 + UTF-8 净化（v0.48 P2-1 同款）；并发闸 4（排队 5s 超时）；本地审计
  JSONL 副账（端到端 operator 留痕）。
- **消息 17→19**：`TermExec`（云→边）/ `TermResult`（边→云）——不启用
  NodeJob 占位（其定义为「v0.1.0 范围外保留占位」，启用即改语义）；旧版本
  default 忽略，双向兼容。
- **端点 102→107**：POST/GET `/api/v1/term/sessions`、POST
  `/api/v1/term/sessions/{id}/exec`、POST `/api/v1/term/sessions/{id}/close`、
  GET `/api/v1/term/audit`（X-Total-Count=过滤总数）。
- **RBAC 联动**：write 面（create/exec/close）= operator+（viewer 403）；
  read 面（列表/审计查询）= viewer+；未认证 401——全局 middleware 统一管控。
- **审计三层**：API 层（既有 audit middleware 全覆盖）+ 云端命令级 JSONL
  （`EDGEFLOW_CLOUDCORE_TERM_AUDIT_PATH`，默认 data/term-audit.jsonl）+
  边侧本地副账（`EDGEFLOW_EDGED_TERM_AUDIT_PATH`）。验收锚「审计完整」。

## N2 兼容性

- 端点/消息只增不改；冻结带（v0240–v0350）零改动；go.mod 零变化。
- 旧边缘收 TermExec、旧云端收 TermResult 均 default 忽略（既有行为）。
- 终端面默认关闭：边缘未 opt-in 时所有命令 rejected（原因说明），云端
  会话/审计面可用。

## N3 测试（新增 16 例）

- `pkg/protocol/term_test.go` 2 例（复核 P3-2）：TermExecPayload.Validate
  直测（13 拒绝路径 + args 总限）；TermResultPayload.Validate 直测（5 拒绝
  路径 + 96KB output 边界）。
- `cloud/pkg/termgw/v0490_test.go` 8 例：生命周期/上限（per-node+global+释放
  重建）/roundtrip（含审计关键字段）/归属校验（伪造丢弃）/离线/空闲过期/创建
  校验/审计分页。
- `edge/pkg/termexec/v0490_test.go` 6 例：未启用拒绝/白名单（默认集+env 覆盖
  +路径分隔符）/退出码与超时终止/截断（sanitizeOutput 单元 + 多字节整字回退
  + JSON 不膨胀 + 近界集成）/审计落盘/并发闸。
- e2e `TestV0490TermTerminalE2E` 六段：US-1 全链（创建→uname→关闭→409）/
  US-2 越权（viewer 写 403 + 匿名 401 + 未知会话 404）/US-3 审计完整
  （create/exec/close 三动作）/US-4 白名单（reboot rejected + df 正常 + 边侧
  副账含 operator）/US-5 超时（sleep 30 @2s 终止 2.0s）/US-6 会话管理
  （429 上限 + 释放重建 + 列表历史保留语义）。

## N4 边界（spec 0022 裁决与非目标）

- 会话为云端易失态（重启丢失，审计在文件）；List 保留全部历史不做状态过滤。
- 同步 exec 受 HTTP WriteTimeout(15s) 实际约束——协议层 timeoutSec 上限 120
  保留，但同步面建议 ≤10s（超时大值需求留后续异步化，KI §50）。
- 边侧审计 v0.49.0 不上云（防篡改副账在节点本地）。
- 命令不补传（运维命令时效性强，丢失即失败返回——spec 裁决 10）。

## N5 门禁

- `go build ./...` / `go vet ./...`：OK；gofmt 清洁（仓库代码）。
- `go test -race`：首轮 ×6 包（termgw 8.443s / termexec 11.871s / protocol
  4.209s / cloudhub 12.192s / rbac 4.986s / cmd/cloudcore 20.352s）；复核处置
  后复验 ×3 包（termgw 7.349s / termexec 10.371s / protocol 1.994s）。
- 契约：107 端点/19 消息全绿（14.311s 首验 + 复验见 N6）。
- 定向 e2e：`TestV0490TermTerminalE2E` **PASS 11.845s**（run6 处置后终验；
  run1-5 修复轨迹：①裸 startProcess 不等 healthz → 补 30s 就绪等待；②RBAC on
  下 waitNodeRegistered 401 → 换带 admin token 轮询；③TermResult 身份比对
  方向错误（误用 m.Target，边→云应为 m.Source）→ 全部结果被拒，修正后链路通；
  ④US-6 断言未计历史会话（List 设计行为）→ 修断言口径）。
- 全量 e2e：**744.634s exit=0**（处置后终验，.cluster/edgeflow-v0490/e2e-full-v0490.log；
  首跑 728.4s FAIL 于 TestV0430VideoManageE2E 断网窗补传计数断言——定向复跑
  46.285s 绿确认为时序偶发非回归，全量重跑通过）。

## N6 复核（15min 硬时限，实际 11m17s，含 -race 实测复跑）

- 原始结论：**PASS（无 P1；P2×4 / P3×10）**；处置后全绿放行（回执
  .cluster/edgeflow-v0490/review.md，原始报告 review-original.md）。
- **P2×4 全修**：①命令解析改固定目录单次解析（resolveInFixedPath，消除
  进程 PATH 依赖与 TOCTOU）；②date 移出默认白名单（args 可改时钟/回读文件，
  「全只读」裁决不成立——默认集五命令均无状态副作用）；③同步面 timeoutSec
  默认收敛 10s 且 >10 直接 rejected（WriteTimeout 15s 约束，异步化留后续）；
  ④参数校验失败分支补命令级审计（取证闭环）。
- **P3**：快修 5（off 特判/protocol Validate 直测 2 例/计数校正/viewer
  exec+close 403 直接断言/恒真写法清理）；留档 5（sessions 历史无上限/审计
  无轮转/请求体无 MaxBytes/Warnf 未限频/shutdown 丢在跑命令）→ KI §50。
- 复核确认的关键面：身份伪造三层防线/双锁无死锁/pending 无泄漏/active 计数
  三路径一致/RBAC 全覆盖/契约四件套一致/只增不改兼容/64KB 截断净化。
- 修正后复验：race ×3 包 + 定向 e2e run6 PASS 11.845s + 全量 e2e 744.634s exit=0。
