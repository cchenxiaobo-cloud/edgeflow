# spec 0022 远程运维终端（v0.49.0，G31+G11）

## 0. 背景与目标

replan P2「G31 远程运维（Web 终端/受控/审计）」：为运维人员提供从云端对边缘节点
执行受控命令的能力。验收锚（L204）：**越权拒绝 + 审计完整**。

核心形态：**受控命令会话**（非交互 PTY）。云端 Web/API 创建会话 → 逐条下发白名单
命令 → 边缘受控执行器执行并回传结果 → 云端同步返回 + 命令级审计。前端可将
「创建会话 + 逐条 exec」组装为终端外观（v0.49.0 只交付 API 面）。

## 1. 用户故事

- **US-1 全链**：operator 创建会话（指定节点）→ 执行白名单命令（如 `uname -a`）
  → 拿到 stdout/exitCode/时长 → 关闭会话；全程可查询审计。
- **US-2 越权拒绝**：viewer 角色调 create/exec/close → 403；未认证 → 401；
  边侧白名单外命令 → 结果 rejected（不执行）。
- **US-3 审计完整**：每次 create/exec/close 产生 API 层审计（既有 middleware）+
  命令级详细审计（含 cmd/args/exitCode/时长）；审计查询端点可分页读取。
- **US-4 会话管理**：会话列表（按节点过滤）；空闲超时自动过期（10min）；
  每节点并发上限 3、全局 32；关闭后会话不可再执行。
- **US-5 执行边界**：超时命令被终止并返回 timeout；输出超 64KB 截断并标记
  truncated；节点离线时 exec 立即失败（503，不入队不补传）。

## 2. 设计裁决

1. **新消息 TermExec/TermResult**（17→19）：不启用 NodeJob/NodeJobResult 占位
   （其定义为「v0.1.0 范围外保留占位」，启用即改语义）。旧云端收 TermResult 走
   default 忽略；旧边缘收 TermExec 同理（只增不改）。
2. **白名单边侧强制**：`EDGEFLOW_EDGED_TERM_ALLOWLIST`（逗号分隔可执行文件名，
   精确匹配、大小写敏感；默认 `uname,date,uptime,df,free,ps`）。云端不做白名单
   判断（单一裁决点在执行侧）。拒绝返回 `rejected=true` + 原因，不执行。
3. **不走 shell**：`exec.Command(cmd, args...)` 直调，参数独立传递；args 校验
   （禁 NUL、单参数 ≤4KB、总长 ≤16KB、个数 ≤16）。
4. **opt-in**：`EDGEFLOW_EDGED_TERM=on` 默认关（对齐 logcollector 模式）；云端
   term API 常驻注册，边缘关闭/离线时 exec 返回 503。
5. **双层审计**：①API 层——既有 audit middleware 全覆盖 term 端点（谁/何时/
   哪个路由/状态码）；②命令级——termgw 记详细审计 JSONL
   （`EDGEFLOW_CLOUDCORE_TERM_AUDIT_PATH`，默认 `data/term-audit.jsonl`：含
   cmd/args/exitCode/durationMs/sessionID/operator）；③边侧本地审计 JSONL
   （`EDGEFLOW_EDGED_TERM_AUDIT_PATH`）作为防篡改副账，v0.49.0 不上云（留后续）。
6. **RBAC**：create/exec/close = write（operator+，viewer 403）；audit 查询 =
   read（viewer+ 可读）。角色矩阵复用 v0.46 Allow 判定，不新增角色。
7. **同步等待**：HTTP exec 请求挂起等待 TermResult（chan + select timeout，
   `timeoutSec + 5s` 缓冲；timeoutSec 默认 30 上限 120）；超时未回 → 终止命令
   （边侧 context）+ 返回 timeout 结果，pending 条目清理。
8. **会话云端态**：created（唯一活动态）/closed/expired；不落盘（重启丢会话
   ——审计在，会话易失可接受，登记 KI）。exec 仅允许 created 态。
9. **输出截断**：stdout+stderr 合并捕获，64KB 上限（保留头部），truncated 标记。
10. **非目标（v0.49.0 不做）**：交互式 PTY 流、审批双签流、边侧审计上云、
    命令补传（运维命令时效性强，丢失即失败返回）。

## 3. 消息与端点

### 消息（17→19）

- `TermExec`（云→边）：`{execId, sessionId, cmd, args[], timeoutSec}`
- `TermResult`（边→云）：`{execId, sessionId, ok, exitCode, output, truncated,
  rejected, reason, durationMs, ts}`

### 端点（102→107）

| 端点 | 方法 | 权限 | 说明 |
|---|---|---|---|
| /api/v1/term/sessions | POST | write | 创建会话 {nodeID, reason}（reason 必填 ≤256B，入审计） |
| /api/v1/term/sessions | GET | read | 会话列表（?nodeID= 过滤） |
| /api/v1/term/sessions/{id}/exec | POST | write | 执行命令 {cmd, args[], timeoutSec} → TermResult |
| /api/v1/term/sessions/{id}/close | POST | write | 关闭会话（幂等） |
| /api/v1/term/audit | GET | read | 命令级审计查询（?nodeID=&limit=&offset=，X-Total-Count） |

## 4. env 清单

- `EDGEFLOW_EDGED_TERM`（默认 off）/ `EDGEFLOW_EDGED_TERM_ALLOWLIST` /
  `EDGEFLOW_EDGED_TERM_AUDIT_PATH`（默认 data/term-exec-audit.jsonl）
- `EDGEFLOW_CLOUDCORE_TERM_AUDIT_PATH`（默认 data/term-audit.jsonl）/
  `EDGEFLOW_CLOUDCORE_TERM_SESSION_TTL`（默认 10m）/ `EDGEFLOW_CLOUDCORE_TERM_MAX_PER_NODE`（默认 3）/ `EDGEFLOW_CLOUDCORE_TERM_MAX_GLOBAL`（默认 32）

## 5. 交付锚

- 交付（2026-10-20）：cloud/pkg/termgw（会话/同步等待/审计/归属校验）+ edge/pkg/termexec（白名单/超时/截断/并发闸/本地副账）+ pkg/protocol/term.go（TermExec/TermResult + Validate）+ cloudhub term_result.go（Source 方向身份比对）+ cmd/cloudcore/v0490_term_api.go（5 端点）+ 两侧 main.go 装配。单测 15 例（termgw 8 / termexec 7）+ e2e 六段（TestV0490TermTerminalE2E，run5 PASS 12.151s）。契约 102→107 端点/17→19 消息（routes/API-SPEC §1.1/API-COMPATIBILITY 矩阵+增量节/探测分支/扫描清单）。门禁：race ×6 包绿 + 契约全绿 + 全量 e2e exit=0。
- 复核处置（PASS→放行，P2×4 全修 + P3 快修 5 留档 5）：P2——命令解析固定目录单次解析（消除进程 PATH 依赖与 TOCTOU）/date 移出默认集（args 参数面失控，「全只读」不成立）/同步面 timeoutSec 钳 10s（WriteTimeout 15s 约束，异步化留后续）/参数校验失败分支补命令级审计。「同步等待」裁决修订：同步面超时上限 10s（原 120s 上限保留给协议层与未来异步面）。e2e 修复留痕（run1-5）：①裸 startProcess 不等 healthz（补 30s 就绪等待）；②RBAC on 下节点等待需带 token（waitNodeRegistered 无认证版不适用）；③TermResult 身份比对方向错误（误用 m.Target——边→云身份在 m.Source，run3 全部结果被拒的根因）；④US-6 断言未计 List 历史保留语义。
