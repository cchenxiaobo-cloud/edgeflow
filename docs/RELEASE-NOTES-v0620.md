# EdgeFlow v0.62.0 发布说明（静默联动通知抑制）

> 版本：v0.62.0 ｜ 日期：2026-10-22 ｜ spec：0035-mute-forward-link ｜ 缺口：spec 0031 裁决 6 承接
> 基线：v0.61.0（837077d）。**本版为语义联动版本：零新增端点（135 维持）、零新增消息（19 不变）；mutestore schema 只增字段（severity，旧快照向后兼容）。**

## N1 能力

- **静默联动通知抑制**（spec 0031 裁决 6 承接）：forwardstore 增本地 MuteSource
  接口（MutedEvent(nodeID, severity, nowMs)——与 hierarchyview.MuteSource 同型
  但含 severity 维度，避免跨包依赖）+ SetMutes 装配；Dispatch 投递前判定，命中 →
  该事件**不投递、不记台账**（通知未发生即无事件——台账只记录真实投递尝试）；
  未装配时行为不变。
- **severity 维度**：Silence 增 severity 可选字段（空=全部级别；
  critical|warning|info）；MatchesEvent(nodeID, severity) = Active && nodeID
  匹配 &&（窗口未指定级别 || 级别相等）。
- **口径差异显式登记**：**层级活跃口径维持节点级**——severity 窗口不参与
  节点级 Muted 判定（v0.58 Muted 语义不变，mutestore.Muted 跳过 severity 窗口，
  避免 critical 静默误伤 warning 活跃计数）；转发过滤走 MutedEvent（severity
  感知）。两个消费方两种口径，均显式声明。
- **schema 扩展**：POST /api/v1/alerts/mutes body 增 severity 可选字段；
  快照只增字段（旧快照 Load 默认空 = 全级别，向后兼容）。

## N2 兼容

- 端点 135 不变、消息 19 不变；契约四件套同步（API-SPEC mutes POST 行 schema
  更新 + COMPATIBILITY 增量节登记 Dispatch 语义变更与 audit 口径）。
- mutestore.Create 签名增 severity 参数（内部 API，调用面 main.go/测试全量适配）；
  forwardstore.SetMutes 为纯新增（未装配行为不变）。
- **Dispatch 语义变更**：v0.62 前「静默」概念不存在，无行为回归；v0.62 起静默
  窗口内事件不投递不记台账（增量节显著声明）。

## N3 测试（新增 8 例 + e2e 两段）

- `cloud/pkg/mutestore/v0620_test.go` 4 例：severity 校验（合法/非法）/
  MatchesEvent（同级命中/异级不命中/全级别命中）/Muted 忽略 severity 窗口
  （口径差异锚）/持久化 roundtrip 含 severity。
- `cloud/pkg/forwardstore/v0620_test.go` 1 例：Dispatch 静默抑制——静默节点
  事件零收包零台账，未静默节点正常送达。
- `cloud/pkg/hierarchyview`：既有 3 例静默排除测试全过（MuteSource 接口语义
  未变——节点级）。
- e2e `TestV0620MuteForwardLinkE2E` 两段（真实告警管线）：US-1 静默中真告警
  入库（历史不动）→ summary 活跃 0 → webhook 零收包 → US-2 admin 删除窗口 →
  新告警（第二阈值规则）正常送达。

## N4 边界（已知限制，详见 KI §63）

- 静默事件不记台账（抑制无副作用语义）——「已抑制」审计留后续需求立项。
- severity 维度不参与层级活跃口径（节点级口径差异维持，spec 0035 裁决 4）。
- hierarchy 活跃口径不受 severity 窗口影响（口径差异在两处包注释与 spec 显式声明）。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race -count=1`：mutestore / forwardstore / hierarchyview 全绿。
- 契约：**135 条复验**（端点零新增；mutes POST schema 更新）。
- 定向 e2e：`TestV0620MuteForwardLinkE2E`（见交付锚回填）。

## N6 复核

- R1–R4 均已处置收口；**R5（v0.62+v0.63 合并复核）**随 v0.63 执行。
