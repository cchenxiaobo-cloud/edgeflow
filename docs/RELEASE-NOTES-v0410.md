# EdgeFlow v0.41.0 发布说明（采集扩展与性能）

- 发布日期：2026-10-01
- 基线：v0.40.0（24abd85）
- 主题：采集扩展与性能（发展规划 G19+G13）——Modbus RTU 协议通道（CRC16/帧编解码/客户端 + 模拟器 RTU-over-TCP 联调，串口硬件依赖登记）、REST 轮询采集器（形态评估落档）、高频波形通道（块采集/环形缓冲/FFT+包络谱特征前置，复用 v0.38 时序库）、2000 点/1s 采集压测与基线数字。零新依赖；契约零改动（61 端点/15 消息维持）；REST/波形默认零行为（opt-in）；Modbus TCP 路径零回归。

## N1 能力（新增）

### 1. Modbus RTU 通道（pkg/modbusrtu + pkg/modbussim + mappers/modbus）
- pkg/modbusrtu（零依赖）：CRC16（0xA001，标准向量 "123456789"→0x4B37）、RTU 帧 `[addr][PDU][CRC16-LE]` 编解码、客户端（FC 0x01/0x03/0x05/0x06，响应截止超时、从站/功能码/CRC 校验、Modbus 异常码帧 → ModbusError 语义对齐 goburrow）、RTU-over-TCP 拨号。
- modbussim RTU 模式：独立 RTU-over-TCP 监听（与 MBAP 共享寄存器模型/波动 goroutine/连接上限），CRC/从站校验、异常码帧、CRC 错断连（无帧边界重同步语义）。
- mappers/modbus：地址 scheme 分发——`host:port`/`tcp://`（goburrow TCP，既有路径零回归）；`rtutcp://`（RTU-over-TCP，串口-网关主流形态，与模拟器联调）；操作面统一为内部 transport 接口（读保持/写寄存器/写线圈/读线圈），重连/超时/台账语义保持。
- **串口硬件依赖登记（KI §42）**：`serial://` 真串口 transport 本版不做（联调待硬件），后续候选。

### 2. REST 采集器（mappers/rest，轮询形态）
- **形态评估落档**：轮询 vs 推送——取【轮询】：与 mapper 框架 Collect 周期语义一致、边缘不新增入站端口（安全面零变化）、确定性可测；「推送接收」属平台对接（G18 方向）登记后续候选。
- RESTMapper：env EDGEFLOW_REST_URL（空=不装配）/ _DEVICE（rest-01）/ _NAMESPACE（default）/ _TIMEOUT（3s）；Collect = GET → JSON `{"values":{...}}` → 属性映射；非 200/坏 JSON/超时 → 错误（Warn + 影子旧值）；HandleCommand 明确拒绝（只读采集面）。

### 3. 高频波形通道（pkg/waveform + cmd/edgecore 装配）
- pkg/waveform（零依赖）：模拟振动源（基频 50Hz + 谐波 150Hz + 高斯噪声，确定性 seed，仿真速率连续出块）、环形缓冲（count 语义覆盖最旧）、特征前置（RMS/Peak/Crest + FFT 幅值谱主频/幅值 + 包络谱主频——radix-2 迭代 FFT，2 的幂补零）。
- 装配（opt-in EDGEFLOW_EDGECORE_WAVEFORM=on）：周期出块（RATE_HZ 默认 10000、BLOCK 默认 4096、INTERVAL 默认 500ms）→ 特征作为虚拟设备（vibration-01/waveform ns）经既有采样管道（治理→规则→tsSink）进影子 → 随 DeviceReport 上云；原始波形落 tsdb 子开关（_RAW_TSDB=on 默认 off，直写 tsSink 绕过治理/规则）。
- e2e 演示：10kHz 模拟源连续采集 → domFreq ≈ 48.8Hz（bin 分辨率 2.44Hz 下 50Hz 注入的最近 bin）、rms=0.764，特征六键全部经真实进程上云。

### 4. 2000 点/1s 采集压测（hack/collect-bench）
- 脚本（零依赖）：200 设备 × 10 属性 × 1s 周期，经治理过滤→规则评估（20 条阈值规则）→tsdb 全管道；参数可调（-devices/-props/-interval/-duration）。
- 基线（Apple M1 Pro / macOS / Go 1.26.2，两轮复跑）：吞吐 **2000 点/s（达标）**；单轮处理延迟 **P50=11.9/12.3ms、P99=14.8/23.2ms**（远低于 1s 周期，波动登记）、max≈505ms（首轮预热含时序库初始化）；时序库 2000 序列 / 20000 点全落库、写延迟 0.031ms/点。数字入 PERFORMANCE-BASELINE.md。

## N2 兼容与冻结
- **默认零行为**：REST_URL 未配置 → REST Mapper 不装配；WAVEFORM 未开 → 波形链路零行为；Modbus TCP 路径（既有 addr 格式）与 v0.40.0 行为一致（transport 抽象为等价重构，PRT-21 测试零改动通过）。
- 契约零改动：61 端点 / 活跃 15 消息维持（本版为边缘采集面，无新端点/消息）。
- v0240–v0350 冻结测试零改动；MQTT/OPC-UA/视频/模型面代码零触碰；pkg/rules、pkg/tsdb 零改动（复用）；go.mod 零变化（RTU 为自研实现，goburrow/serial 间接依赖面未动用）。

## N3 测试（grep 口径）
- pkg/modbusrtu +4 例（CRC 向量/帧往返与篡改拒绝/客户端读写全链/异常帧）。
- mappers/modbus +1 例（RTU-over-TCP 联调：采集 + 写寄存器回读 + 线圈开关）。
- mappers/rest +5 例（成功映射/四类失败路径/超时/只读拒绝/生命周期幂等）。
- pkg/waveform +8 例（已知信号特征/谐波主导/包络谱/空输入/FFT×朴素 DFT 一致性/Ring 覆盖语义/空缓冲/源确定性与时间轴）。
- cmd/edgecore +3 例（波形 env 解析/关闭态零行为/启用态影子写入与幂等停）。
- e2e 1 例（REST 轮询注入值 + 波形特征提取经真实双进程上云）。
- 合计：单测 21 例 + e2e 1 例；既有 Modbus TCP/MQTT/OPC-UA 路径测试零改动全绿（零回归）。

## N4 边界（登记 KNOWN-ISSUES §42）
- 真串口 transport 未实现（串口硬件依赖，联调待硬件）；RTU-over-TCP 非标准 Modbus 封装（串口-网关主流形态，明确登记）；
- FFT 为 radix-2（2 的幂补零）+ 矩形窗（非整周期采样谱泄漏——主 bin 幅值低于真实值，测试用 bin 对齐采样率规避）；
- 包络谱为最小实现（|x| 去均值 → FFT，非完整 Hilbert 解调）；特征频率分辨率 = rateHz/nextPow2(N)（10kHz/4096 ≈ 2.44Hz）；
- 波形模拟源按仿真速率出块（墙钟块间隔与仿真块时长解耦——非实时采样承诺）；
- REST 采集器为单设备/单端点（多端点聚合为后续候选）；推送接收形态登记后续候选；
- 原始波形落 tsdb 默认 off（10k 点/s 量级，开启需评估容量水位）；波形特征频率搜索段排除 <1Hz 与 DC。

## N5 门禁（2026-10-01 全绿）
- [1] vet（全仓）/[2] 全仓回归（41 包）/[3] race ×6 包（pkg/modbusrtu 2.2s、pkg/modbussim 3.2s、
  pkg/waveform 3.6s、mappers/modbus 4.9s、mappers/rest 4.4s、cmd/edgecore 7.2s）/[4] 契约 11.9s/
  [6] e2e 全量最终复跑 **619.8s 全绿（0 FAIL）**。
- 最终 e2e 全量含修复后的 v0390（断网积压自适应木证）与新增 v0410 采集用例；完整输出归档
  工作台 e2e-final3.log。
- 门禁过程记录（诚实登记）：首轮 e2e 因包级 10m 超时 panic（与复核任务并发负载）遗留一个
  cloudcore 进程占用嵌入式 etcd 固定端口 12379/12380，致后续两轮复跑环境残缺（etcd 降级
  纯内存）失败；清理泄漏进程后，复跑暴露 v0390 用例对 mock 传感器随机穿越节奏的时序依赖
  （阵发穿越 vs 断网窗口空窗→无积压可重放），已改为「等待边侧积压木证（补传 drain 失败
  锚点，有界 120s）」后再恢复云端；最终全量复跑 619.8s 全绿。上述为 e2e 基建/用例稳健性
  问题，非产品缺陷。
- 复核：P0=0 / P1=2（已修）/ P2×10（全处置）；处置记录见工作台 review.md ⑦。
