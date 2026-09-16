# EdgeFlow v0.38.0 发布说明（边缘时序存储）

- 发布日期：2026-09-16
- 基线：v0.37.0（2c45c85）
- 主题：边缘轻量时序库（自研零依赖，发展规划 G17）——序列/段存储模型、时间窗查询与窗口聚合、段粒度保留与降采样、容量水位泄洪与背压、崩溃恢复；边缘装配 opt-in（采集管道 sink）；性能验证（720 条/s 场景）。零新依赖；默认零行为（TSDB 关闭时全链与 v0.37.0 逐字节兼容）；契约零改动。

## N1 能力（新增）

### 1. 存储模型与写入（pkg/tsdb）
- `Point{Metric, Tags, Ts, Value}`：Metric 为序列键（装配约定 `device/namespace/property`）；Tags 有序归一化杂凑为序列身份（同 Metric 不同 Tags 独立序列）。
- 序列 = 目录 `series/<hash16>/`（含 name 映射文件）；段（segment）= 追加写文件，满 `SegmentPoints`（默认 4096）封段滚动；段名 `seg-<创建毫秒>-<进程序号>`（同一毫秒连续建段不撞名）。
- 写入支持单条/批量；不强制时间单调（乱序允许，查询按时间稳定排序）。

### 2. 查询与聚合（pkg/tsdb）
- `Query`：时间窗 `[from,to)`（from/to=0 为开区间）；降采样段返回桶代表点（Ts=桶起点、Value=桶内均值）。
- `Aggregate`：窗口聚合 `avg/min/max/count/sum`（窗口自 from 对齐；from=0 用绝对网格）；降采样段上正确重聚合（avg 以 count 加权）。

### 3. 保留与降采样（pkg/tsdb）
- `RunRetention(now)` 逐段判定：超保留期 → 删除；raw 段超降采样期 → 重写为 rollup 段（桶：min/max/sum/count/last；**原子替换**——临时文件写完后 rename 覆盖，崩溃安全）。
- `RunRetentionLoop(interval)` 周期执行；活动段不参与（段粒度，封段后生效）。

### 4. 容量水位与背压（pkg/tsdb）
- `MaxBytes` 超限时泄洪（删最老封段）→ 仍超 → `ErrBackpressure` 拒绝写入并计数；已有数据不受影响；`Stats()` 提供序列/点/段/字节/丢弃计数。

### 5. 持久化与崩溃恢复（pkg/tsdb）
- 段文件：32B 头（magic/kind/bucketMs/createdMs）+ raw 记录 16B/点 + rollup 记录 48B/桶（小端）。
- 崩溃一致性：记录定长、尾部残缺记录自动忽略；Open 扫描重建序列清单（损坏段 Warn 跳过，不阻断）；重开时原活动段一律按封段处理（新写入从新段开始，残尾不被续写污染）。
- flush：默认 1s 周期刷盘；`Flush()`/`Close()` 显式刷盘（断电窗口 ≤ 周期，登记 KI §39）。

### 6. 边缘装配（cmd/edgecore）
- 开关（全部 opt-in，默认关闭零行为）：`EDGEFLOW_EDGECORE_TSDB=on`；`EDGEFLOW_EDGECORE_TSDB_DIR`（默认 `<db 同目录>/tsdb`）；`TSDB_RETENTION`（默认 72h，"0" 禁用）；`TSDB_MAX_MB`（默认 256，"0" 不限）；`TSDB_DOWNSAMPLE=off` 可关（默认 on：after=1h/every=1m）；非法值告警回退默认。
- 采样管道 sink：accepted 值（与影子同源同值）落时序库，metric=`device/namespace/property`；写入失败告警限频（首失败 + 每 256 次）并在恢复时提示。
- 装配：Open 失败降级（不阻断启动，采集零影响）→ 启动保留轮 → 周期保留循环（随退出停止）→ 优雅关闭刷盘。

## N2 兼容与冻结
- **默认零行为**：TSDB 关闭（默认）时启动链路零新日志、数据路径零变化——采集/影子/上报/规则链与 v0.37.0 逐字节等价（tsSink=nil 零行为分支）。
- 契约零改动（无新端点/消息，51 端点/14 消息维持）；v0240–v0350 冻结测试零改动；MQTT/OPC-UA/视频/模型面代码零触碰；零新依赖（纯标准库）。

## N3 性能（本版验收核心）
环境：Apple M1 Pro / macOS（Darwin 25.1.0）/ Go 1.26.2；本机基准（`go test ./pkg/tsdb/ -bench`；两轮复跑波动 ≤2%，下表取首轮原始输出，完整输出归档工作台 `.cluster/edgeflow-v0380/bench.log`）。

| 基准 | 场景 | 实测 | 换算 |
|---|---|---|---|
| WriteSingleSeries | 单序列顺序写 | 139.8 ns/条 | ≈ 715 万条/s |
| WriteParallelSeries | 8 并发 × 16 序列 | 590.0 ns/条 | ≈ 169 万条/s |
| **Write720Scenario** | **200 序列 × 36 点/批（=7200 条 ≈ 10s 数据 @720/s）** | **1.046 ms/批** | **≈ 688 万条/s** |
| QueryWindow | 10 万点中查 1 万点窗 | 52.7 ms | 全段扫描（优化候选） |
| AggregateWindow | 5 万点 → 83 桶 avg | 27.3 ms | — |

**对照规划验收**：方案优化场景 720 条/s 目标——本版实测吞吐余量约 9500×；维护场景高频通道（波形 10kHz）依赖 v0.41 采集扩展，届时复用本库段模型扩展。数字同步登记 PERFORMANCE-BASELINE.md。

## N4 测试（grep 口径）
- pkg/tsdb 29 例（写入/查询/聚合/保留/降采样/水位背压/崩溃恢复/并发/边界）+ 5 基准。
- cmd/edgecore +10 例（开关解析/Options 默认与回退/sink 语义/装配/降级）。
- e2e 1 例（真实进程：断言序列 ≥2 / 点 ≥4 / 重启后点增长；实测观测 14 → 16 点，原始日志归档工作台）。
- 契约：零改动（51 端点/14 消息维持）；全量契约测试与 e2e 套件结果见门禁。

## N5 边界（登记 KNOWN-ISSUES §39）
- 段粒度保留/降采样（活动段内旧点封段后生效）；断电窗口 ≤ flush 周期（默认 1s）；
- rollup 桶不保留 first（降采样后 Query 代表点=avg）；无压缩；无跨序列 join/标签过滤；
- 同目录单实例写（跨实例并发写不承诺）；
- e2e 基建发现：edgeEnv 的 300ms 周期低于合法下限（1s）被区间校验回退 30s（历史现象；
  本版 e2e 局部替换为 1s 规避；统一修 helper 待后续）。

## N6 门禁（2026-09-16 全绿）
- vet（全仓）/ 全仓回归（除 e2e/契约，41 包全绿：pkg/tsdb 1.2s、cmd/edgecore 8.5s、cloud/rulestore 7.5s）/ race（pkg/tsdb 3.7s + cmd/edgecore 5.8s）/ 契约 13.9s / e2e 全量 447.0s：**全绿（0 FAIL）**。
- 处置后复验：query.go 告警补齐后 race 复跑绿（pkg/tsdb 1.3s）；基准两轮复跑波动 ≤2%；e2e 单例重跑复现 14→16。
- 原始日志归档：工作台 `.cluster/edgeflow-v0380/`（gates.log / bench.log / e2e.log）。
