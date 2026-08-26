# EdgeFlow v0.13.0 发布说明（模型生命周期与运维收尾）

- **发布日期**：2026-08-26
- **版本基线**：v0.12.0（2026-08-26）→ v0.13.0
- **主题**：A′ deployments 列表分页（L28 同族收官、v0.8.0 漏网项）+ B 删除级联收官（L25，GC 开启下删模型级联清终态发布）+ C offlineAt 精确展示（L16 残余 DTO 部分）
- **兼容性**：**零新增端点**（总数维持 32）、**零新增 env**、升级零迁移、老边缘零动作
- **设计**：设计 Agent 定稿 design.md（347 行六节；实测推翻 S1 候选 A——releases 分页 v0.8.0 已闭环，发现同族漏网项 A′）

## 一、核心能力

### 1. deployments 列表分页（A′）

v0.8.0 给 models/versions/releases 做了 limit/offset 分页，**漏了 deployments**。本轮补齐：

- `GET /api/v1/models/{modelName}/deployments` 新增 `limit`(1-1000)/`offset`(≥0) query 参数 + 响应头 `X-Total-Count`
- 与 releases 完全同构（parsePageParams→slicePage→writePageHeaders）；分页在 API 层完成（存储层 ListDeployments 已按 NodeID 排序）
- **缺省全量零破坏**：不传参数 = 旧行为逐字节一致；非法参数才 400

### 2. 删除级联收官（B，L25）

现状（实测）：guard 已随 DeleteModel 清理、元数据最后删已消除"不可见孤儿"窗口；**缺口**——GC 开启下已删模型的终态发布既不被 DeleteModel 清理、也不被控制器 GC 触及（`gcIfEnabled` 只对活模型触发）→ 成永久累积点。

- `WithReleaseGC(enabled, keep)` store 构造选项（variadic，向后兼容既有调用；`NewMemoryModelStore`/`NewEtcdModelStore` 均支持）
- DeleteModel 在 **GC 显式开启**时级联清理该模型全部终态发布：头键 + `releases/<id>/` 前缀（nodes/、lock/）+ 内存缓存；etcd 侧 `deleteModelReleases` best-effort（失败仅 Warn，不阻断删除，etcdctl 可手动清理兜底）
- **GC-off 默认 = L31 审计口径零变化**（删除模型保留终态发布，可查审计）
- **语义决策**：已删模型在 GC 开启下**全清**（不保留 keep 条）——模型已不存在，保留孤儿终态无审计上下文；文档明示
- **零新增 env**（复用 `EDGEFLOW_CLOUDCORE_RELEASE_GC_ENABLED/KEEP`，语义扩展）

### 3. offlineAt 精确展示（C，L16 残余）

- `NodeInfo` 新增 `offlineAt`（毫秒，omitempty）：`/api/v1/nodes` 双视图外露
- `EdgeNodeStatus` 新增 `lastOfflineTime`（RFC3339，omitempty）：`/api/v1/edgenodes` 外露
- Get/List/ListEdgeNodes 三入口一致填充（ListEdgeNodes 内部节点 OfflineAt 为 0，先填再映射）；三模式统一（wrapper 均委托内存 Registry）
- 在线/未知 = 字段省略；重复 MarkOffline 不刷新（首离时刻）；重新上线清除；Seed 播种 = 启动时刻（如实反映重启重置）
- **JSON 宽容论证**：老客户端忽略未知字段、新客户端读旧数据缺省省略；offlineSince 为瞬态内存数据不落盘 → 原 L16"改 JSON 影响兼容性"登记前提不成立，本轮闭环

## 二、升级注意

- **零迁移**：零新增端点、零新增 env、无持久化数据形态变化（offlineAt 瞬态、B 仅删不迁）、无新键前缀
- **老边缘零动作**：全部改动在 cloudcore 侧；edgecore/协议 DTO 无变化
- **行为差异面（需知悉）**：
  - GC-on 下删模型会一并清除该模型全部终态发布（审计痕迹消失）——运维需知悉：审计保留只存在于 GC-off 默认形态；GC-on 用户以 ops 台账/文档为审计依据（与 §8 L31 口径一致）
  - 重启后离线/Unknown 节点的 offlineAt 显示为重启时刻（L16 重启重置语义如实外露，非缺陷）
  - deployments 列表无参数时行为不变，仅多一个 X-Total-Count 头
- 回滚 v0.13.0 → v0.12.0：新参数/新字段消失、GC 级联行为消失，老数据兼容（JSON 宽容）；混跑禁令延续（全停再全起）

## 三、验证摘要（实测）

- 全量 `go test -race ./...` 35 包全绿；go vet 干净；openapi 产物重新生成
- 单测：A′ 分页 8 边界（缺省全量/limit 切片/offset 超界/X-Total-Count/4 种非法 400/模型 404/releases 回归）+ B 内存 GC-on/off/inflight 3 + etcd GC-on 键清理（头+前缀+无关模型保留）/GC-off 保留 2 + C offlineAt/Seed/ToEdgeNode/ListEdgeNodes 4（含 JSON omitempty 断言）
- **E2E 实测**：
  - S1（A′）：发布 2 轮 → deployments 缺省 2 条 + X-Total-Count:2 + limit=1→n1 + offset=2→空 + 4 非法参数 400 ✅
  - S2（C）：在线无 offlineAt/lastOfflineTime → 断开 → Offline + offlineAt(ms) + lastOfflineTime(RFC3339) → 重启 → 字段消失 ✅
  - S3（B GC-on 隔离实例）：发布 succeeded → 归档 → 删模型 200/404 → 重建同名模型 releases 空（级联清理生效，无残留影响）✅
  - S4（B GC-off 回归）：默认实例删模型 200/404 正常（键保留由单测断言）✅
  - digest 复核端点回归（v0.12.0 链路未触及，确认可用 + ghost 404）✅

## 四、文档同步

KNOWN-ISSUES（§6 L16/§7 L25/§8 L28/L31 四行标注 + 新增 §13）、API-SPEC（§1 状态行 v0.12.0/v0.13.0 + deployments 端点行 + §7.7 增量小节）、API-COMPATIBILITY（v0.13.0 增量三行）、DEPLOYMENT（§13 删除级联 GC 语义）、README（当前版本/版本历史）、Chart 0.13.0、用户手册 v0.13.0（tex/md/PDF）、方案手册 v1.7.0（md/latex/PDF）

## 五、遗留（非阻断，见 KNOWN-ISSUES §13）

- 非事务级联崩溃窗口（原子化需 etcd 跨前缀 txn，后续候选）；GC-on 下审计痕迹随删除清除（运维以 ops 台账为凭）
- 精确"最后在线"需持久化（后续候选）；Seed 后 Unknown 节点 offlineAt=启动时刻
