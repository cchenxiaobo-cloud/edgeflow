# Spec 0011：边缘时序存储（G17）

- 版本归属：v0.38.0（基线 2c45c85 = v0.37.0）
- 规范依据：《EdgeFlow 后续开发计划（对齐智能边缘计算平台解决方案）》v0.38 定义（差距 G17 时序存储与边缘轻量分析）
  + 用户方向「继续开发后续功能，形成新的版本」
- 裁定结论（本 spec 前置决策）：
  - 全链交付：自研零依赖时序库（pkg/tsdb：写入/查询/聚合/保留/降采样/容量水位与背压/崩溃恢复）
    → 边缘装配（cmd/edgecore：采集管道 tap、opt-in 开关、优雅关闭刷盘）→ 性能验收（720 条/s 场景基准）
    → e2e 真实进程验证（采数→落盘→重启恢复）。
  - **不扩契约**：边缘本地能力，无新 HTTP 端点/消息类型（云端查询面与上送协同归 v0.39）；
    契约测试/文档本轮零改动。
  - 零第三方依赖（宪法 II）；默认零行为（TSDB 关闭时采集/影子/上报路径逐字节等价）；
    v0240–v0350 冻结测试零改动；MQTT 路径零触碰。
  - 分层声明（与影子/台账并列）：**影子 = 当前态**（覆盖写，上报唯一源）、**op_ledger = 操作流水**（SQLite）、
    **tsdb = 采样时序**（追加写，历史查询/聚合/保留/降采样）——三者职责不同，互不替代。

## 用户故事与验收

### US-1 时序模型与写入路径（pkg/tsdb）
- `Point{ Metric string; Tags map[string]string; Ts int64; Value float64 }`：Metric 为序列键
  （装配约定 `device/namespace/property`）；Tags 参与序列身份（有序归一化后杂凑）；Ts 毫秒。
- `Open(Options) (*DB, error)`；`(db *DB) Write(points ...Point) error`；`(db *DB) WriteOne(metric string, ts int64, v float64) error`。
- 段管理：每序列一个活动段（追加写）；段内点数达 `SegmentPoints`（默认 4096）→ 封段 + 开新段。
- 写入不强制时间单调（乱序允许；查询按时间升序返回，同 ts 保持写入序）。
- 验收：单条/批量写、序列隔离、Tags 身份区分（同 metric 不同 Tags 为不同序列）、段滚动、乱序查询有序。

### US-2 查询与聚合（pkg/tsdb）
- `Query(metric string, tags map[string]string, from, to int64) ([]Point, error)`：时间窗 `[from, to)`；
  from=0 表示不限起点、to=0 表示不限终点（至少一个非 0？——两者可同时为 0 = 全量）。
- `Aggregate(metric string, tags map[string]string, from, to int64, window int64, fn AggFn) ([]Bucket, error)`：
  窗口聚合，窗口自 from 起按 window 对齐切分（window>0 必填）；`fn ∈ {avg, min, max, count, sum}`；
  `Bucket{ Ts int64; Value float64; Count int64 }`（Ts=窗起点；count 时 Value=点数）。
- 读语义：查询前自动 flush 目标序列缓冲（正确性优先）；降采样段参与查询——Query 返回桶代表点
  （`Ts=桶起点、Value=avg、Tags=nil`），Aggregate 在 rollup 段上正确重聚合（avg 以 count 加权）。
- 验收：窗边界（含 from、不含 to）、五类聚合、跨段查询、降采样段查询/重聚合、空结果、未知序列。

### US-3 保留与降采样（pkg/tsdb）
- `(db *DB) RunRetention(now int64) (RetentionResult, error)`，顺序：
  1. **降采样**：封段且段最大 ts < now − `DownsampleAfter`（>0 启用）且 kind=raw → 聚合为 rollup 段
     （桶宽 `DownsampleEvery`，默认 1m；桶字段 min/max/sum/count/last）并原子替换
     （新段写入成功后才删旧段；失败保留原段并记错误）。
  2. **保留裁剪**：段最大 ts < now − `Retention`（>0 启用）→ 删除（启用降采样时对 rollup 段执行；
     未启用时对 raw 段执行）。
  3. 活动段永不裁剪/降采样（段粒度；活动段内旧点保留到封段——登记边界）。
- `(db *DB) RunRetentionLoop(interval time.Duration, stopCh <-chan struct{})`：周期执行（interval≤0 → 默认 1m）。
- 验收：无策略 no-op；降采样聚合值正确（对照手工计算）；替换原子性（失败不丢原段）；
  保留删除（rollup 二级）；活动段保留；多次执行幂等。

### US-4 容量水位与背压（pkg/tsdb）
- `MaxBytes`（>0 启用）：写入前若 `Stats().Bytes` 超水位 → **泄洪**：删除最老封段（逐个删，直到低于水位
  或仅剩活动段）→ 仍超 → 返回 `ErrBackpressure`（写入拒绝，丢弃计数 ++；已有数据不受影响）。
- `Stats() Stats`：`{ Series int; Points int64; SealedSegments int; Bytes int64; DroppedWrites int64 }`。
- 验收：水位触发泄洪（最老优先）、泄洪不足时背压拒绝、DroppedWrites 计数、水位内正常写。

### US-5 持久化与崩溃恢复（pkg/tsdb）
- 目录布局：`<dir>/series/<hash16>/name`（序列名映射文件）+ `seg-<createdMs>.dat`（段文件）。
- 段文件：头 32B（magic `EFTS0001`[8] + kind uint32 + reserved uint32 + bucketMs int64 + createdMs int64）
  → raw 记录 16B（ts int64 + value float64，小端）；rollup 记录 48B（ts + min + max + sum + count int64 + last）。
- 崩溃一致性：加载/读取时尾部残缺记录（文件长度非记录尺寸整数倍）忽略（截尾）；Open 扫描目录重建
  序列清单；单个损坏段 Warn 跳过（不阻断；其余序列可用）。
- flush：活动段缓冲按 `FlushInterval`（默认 1s）周期刷盘；`Flush()`/`Close()` 显式刷盘。
  断电窗口 ≤ flush 周期（登记边界）。
- `Close()`：刷盘 + 关闭标记（后续写返回 `ErrClosed`）；关闭后可重开（恢复一致）。
- 验收：写→Close→重开（点/序列/段计数一致）；尾部残缺忽略；损坏段跳过；Close 后写拒绝；Flush 后（未 Close）重开可见。

### US-6 边缘装配（cmd/edgecore）
- 开关与参数（环境变量，全部 opt-in）：
  - `EDGEFLOW_EDGECORE_TSDB=on` 启用（默认 off / 其它值 = off → 零行为）；
  - `EDGEFLOW_EDGECORE_TSDB_DIR` 数据目录（默认 `<db 文件同目录>/tsdb`，即 data/tsdb）；
  - `EDGEFLOW_EDGECORE_TSDB_RETENTION`（时长串，默认 72h；`0` = 禁用裁剪）；
  - `EDGEFLOW_EDGECORE_TSDB_MAX_MB`（整数 MB，默认 256；`0` = 不限水位）；
  - `EDGEFLOW_EDGECORE_TSDB_DOWNSAMPLE=on|off`（默认 on：after=1h / every=1m）。
- 装配（main.go）：on 时 `tsdb.Open`；失败 → Warn + 保持关闭（不阻断启动，采集零影响）；
  启动即 `RunRetention` 一轮 + 周期 `RunRetentionLoop`（随 stopCh 退出）；优雅关闭时 `Close()`（刷盘）。
- 采集挂钩（samplePipeline）：新增可选 `tsSink func(device, ns, prop string, value float64, ts int64)`
  （nil → 零行为）；在 accepted 分支与直通（gov/eng 为 nil）分支调用——写入 accepted 值
  （与影子同源同值）；metric = `device/namespace/property`。
- 验收（单测）：开关解析（on/off/缺省/坏值回退）、off 零行为、sink 写入正确（值/时间戳/序列名）、
  关闭刷盘、Open 失败降级不阻断。

## 冻结兼容
- 无新契约端点/消息；`tests/contract` 与 API 文档本轮零改动。
- TSDB off（默认）：采集/影子/上报/规则链逐字节等价；v0370 测试零改动。
- v0240–v0350 冻结测试零改动；pkg/mqtt 零触碰；pkg/rules 零改动（挂钩在 cmd/edgecore 装配层）。
- 零第三方依赖（宪法 II）。

## 测试锚（as-built，2026-09-16 grep 口径）
- pkg/tsdb 29 例 / 5 基准（写入/查询/聚合/保留/降采样/水位背压/崩溃恢复/并发/边界）。
- cmd/edgecore +10 例（开关解析/Options 默认与回退/sink 语义/装配/降级）。
- e2e 1 例（TestV0380TSDBE2E：断言序列 ≥2 / 点 ≥4 / 重启后增长；实测 14 → 16 点）。
- 契约：零改动（51 端点/14 消息维持）。
- 性能：写 ≈688 万条/s@720 场景（数字入 RELEASE-NOTES-v0380 N3 与 PERFORMANCE-BASELINE）。

## 开发期调整（as-built）
- 段文件名 `seg-<createdMs>-<seq>.dat`（进程内序号）：修复同毫秒连续建段撞名边界
  （小段/高频封段场景测试暴露）。
- 段排序/最老段选择用全序比较（createdMs + 路径）：同毫秒时确定性。
- e2e 局部替换上报周期为 1s：edgeEnv 的 300ms 低于配置合法下限（1s）被回退 30s
  （历史基建现象；统一修 helper 待后续，登记 KI §39）。
- 查询为全段扫描（无段级时间裁剪/索引），优化列后续候选（登记 KI §39）。
- 门禁：见 RELEASE-NOTES-v0380 N6（完成后回填）。

## 边界登记（随文档落 KI §39）
- 段粒度保留/降采样（活动段内旧点不裁，封段后生效）；
- 断电窗口 ≤ flush 周期（默认 1s，活动段缓冲丢失可接受）；
- rollup 桶不保留 first（降采样后 Query 代表点=avg，与 raw 语义差异登记）；
- 本轮无跨序列 join / 标签过滤（仅精确 metric+tags 查询）；无压缩；
- 崩溃时活动段尾部残缺数据丢弃（≤ flush 缓冲）。
