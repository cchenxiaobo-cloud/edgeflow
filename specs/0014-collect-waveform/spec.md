# Spec 0014：采集扩展与性能（G19+G13）

- 版本归属：v0.41.0（基线 24abd85 = v0.40.0）
- 规范依据：《EdgeFlow 后续开发计划（对齐智能边缘计算平台解决方案）》v0.41 定义
  （差距 G19 大点位/高频采集性能 + G13 多协议接入 Modbus RTU/REST）+ 用户方向
  「继续开发后续功能，形成新的版本」
- 裁定结论（本 spec 前置决策）：
  - 全链交付：pkg/modbusrtu（CRC16/RTU 帧编解码/客户端，零依赖）→ modbussim RTU
    over-TCP 模式（联调）→ mappers/modbus 地址 scheme 分发（TCP 路径零回归）→
    mappers/rest REST 采集器（轮询形态，评估结论落档）→ pkg/waveform 高频波形通道
    （块采集/环形缓冲/FFT+包络谱特征）→ edgecore 装配（REST/波形 opt-in，特征经
    既有采样管道进影子/规则/告警/tsdb）→ hack/collect-bench 2000 点/1s 压测脚本
    与基线数字。
  - REST 形态评估：取「轮询」——与 mapper 框架 Collect 周期语义一致、边缘不新增
    入站端口（安全面零变化）、确定性可测；「推送接收」属平台对接（G18 方向），
    登记后续候选。
  - RTU 传输形态：本版交付 RTU 协议（帧编解码/CRC16/客户端）+ RTU-over-TCP 联调
    （模拟器 + mapper，覆盖串口-网关主流部署形态）；**真串口传输登记 KI §42**
    （串口硬件依赖，联调待硬件；termios transport 为后续候选）。
  - 默认零行为：REST_URL 未配置 → REST Mapper 不装配；WAVEFORM 未开 → 波形链路
    零行为；Modbus TCP 路径（既有 addr 格式）逐字节回归。
  - 契约零改动：本版为边缘采集面，无新端点/消息。
  - 零第三方依赖（宪法 II）；冻结测试（v0240–v0350）零改动；MQTT/OPC-UA/视频/
    模型面零触碰；go.mod 零变化（RTU 复用既有 goburrow/serial 间接依赖面之外的
    自研实现，不新增模块）。

## 用户故事与验收

### US-1 Modbus RTU 协议与模拟器联调（pkg/modbusrtu + pkg/modbussim + mappers/modbus）
- pkg/modbusrtu：CRC16（Modbus 0xA001）计算与校验；RTU 帧
  `[addr(1)][PDU][CRC16(2,LE)]` 编解码；客户端（io.ReadWriteCloser 字节流 + 响应
  截止超时 + 从站/功能码/异常码校验），FC 0x01 读线圈 / 0x03 读保持寄存器 /
  0x05 写单线圈 / 0x06 写单寄存器；RTUOverTCP 拨号助手。
- modbussim：WithRTU() —— 新监听循环（独立端口），收 RTU 帧 → CRC/从站校验 →
  复用既有寄存器模型处理 PDU → RTU 帧应答（含 Modbus 异常码帧）。
- mappers/modbus：地址 scheme 分发——`host:port`/`tcp://`（goburrow TCP，既有路径
  零回归）；`rtutcp://`（RTU over TCP，pkg/modbusrtu 客户端）；操作接口统一为内部
  client 接口（读保持寄存器/写单寄存器/写单线圈/读线圈），TCP 与 RTU 适配器分别
  实现；台账/重连/超时语义保持。
- 串口边界：`serial://` scheme 本版不做（真串口联调待硬件）——登记 KI §42，
  transport 为后续候选。
- 验收（单测）：CRC16 标准向量（"123456789"→0x4B37）、帧往返、异常帧拒绝、
  客户端×模拟器 RTU 联调（读/写/异常应答）、TCP 路径既有测试零改动全绿。

### US-2 REST 采集器（mappers/rest，轮询形态）
- RESTMapper（DeviceMapper 完整实现）：env EDGEFLOW_REST_URL（默认空=不装配）/
  EDGEFLOW_REST_DEVICE（默认 rest-01）/ EDGEFLOW_REST_NAMESPACE（默认 default）/
  EDGEFLOW_REST_TIMEOUT（默认 3s）。
- Collect：GET URL → JSON `{"values":{"<prop>":<num>,...}}` → map[string]float64；
  非 200/解析失败/超时 → 错误（Warn + 影子旧值，与 modbus 容错语义一致）；
  HandleCommand → 明确报错（只读采集器）。
- 形态评估结论落档（RELEASE-NOTES + spec）：轮询 vs 推送对比与取一理由。
- 验收（单测）：httptest 服务端 × 正常/非 200/坏 JSON/超时/缺失字段路径；
  mapper 注册与命名空间。

### US-3 高频波形通道（pkg/waveform + cmd/edgecore 装配）
- pkg/waveform（零依赖）：
  - `Source`：模拟振动源（基频+谐波+高斯噪声，确定性 seed），按 rateHz 连续产出
    定长块（块采集）；`Ring` 环形缓冲（定容覆盖最旧 + 快照）。
  - `Features(samples []float64, rateHz float64) map[string]float64`：rms / peak /
    crest（peak/rms）/ domFreq（FFT 幅值谱主频，排除 DC）/ domAmp / envFreq
    （包络谱主频：|x| 去均值 → FFT → 低频段主频——包络谱最小实现）。
  - `FFT`：radix-2 迭代（2 的幂补零），零依赖。
- 装配（opt-in EDGEFLOW_EDGECORE_WAVEFORM=on）：周期产出块（RATE_HZ 默认 10000、
  BLOCK 默认 4096、INTERVAL 默认 500ms 墙钟——模拟源按仿真速率连续出块）→
  Features 作为虚拟设备（EDGEFLOW_EDGECORE_WAVEFORM_DEVICE 默认 vibration-01，
  ns 默认 waveform）经既有 sampleProcessor 进影子/规则/告警/tsdb（tsSink）；
  原始波形落 tsdb 子开关（_RAW_TSDB=on 默认 off——10k 点/s 量级，登记）。
- 验收（单测 + e2e）：注入 50Hz 基波+150Hz 谐波 → FFT 主频 = 50Hz±1Hz、
  domAmp 合理；AM 调制信号 → 包络谱主频 = 调制频率±1Hz；正弦 RMS = A/√2；
  Ring 覆盖语义；e2e（真实双进程）波形开启 → vibration rms 属性出现在云端
  设备面（/api/v1/nodes/{nodeID}/devices 或影子查询）。

### US-4 2000 点/1s 采集压测（hack/collect-bench + PERFORMANCE-BASELINE）
- 脚本（零依赖，hack/collect-bench/main.go）：200 设备 × 10 属性 × 1s 周期 × 10s，
  经 治理过滤→规则评估→tsdb 写入 全管道；输出：实际吞吐（点/s）、单批处理延迟
  P50/P99、tsdb 写延迟；参数可调（-devices/-props/-interval/-duration；-points 由 devices×props 推导，as-built 口径）。
- 基线数字入 docs/PERFORMANCE-BASELINE.md（环境表 + 方法 + 数字 + 结论），并同步
  RELEASE-NOTES。
- 验收：脚本可重复运行（两次波动登记）；数字满足 2000 点/1s 目标（吞吐 ≥2000
  点/s，P99 处理延迟 < 周期）。

## 冻结兼容
- Modbus TCP 路径（既有 addr 格式与 goburrow 客户端）行为零回归（既有测试零改动
  全绿）；MQTT/OPC-UA/视频/模型面零触碰；REST_URL/ WAVEFORM 未配置时零行为
  （Mapper 不注册/波形循环不启动）；契约零改动（53→61 端点、活跃 15 消息维持）；
  v0240–v0350 冻结测试零改动；go.mod 零变化。

## 测试锚（as-built 回填，2026-10-01）

- pkg/modbusrtu +4：TestCRC16Vectors / TestFrameRoundTrip /
  TestClientReadHoldingRegisters（FC 03/06 全链）/ TestClientExceptionFrame。
- mappers/modbus +1：TestModbusMapperRTUOverTCP（rtutcp:// ↔ 模拟器 RTU 模式全链：
  采集 + 写寄存器回读 + 线圈开关）。
- mappers/rest +5：TestRESTCollectSuccess / TestRESTCollectErrors /
  TestRESTCollectTimeout / TestRESTHandleCommandUnsupported / TestRESTStartStop。
- pkg/waveform +8：TestFeaturesKnownSignal / TestFeaturesHarmonicDominant /
  TestFeaturesEnvelope / TestFeaturesEmpty / TestFFTAgainstDFT / TestRingOverwrite /
  TestRingEmpty / TestSourceDeterministicAndContinuous。
- cmd/edgecore +3：TestParseWaveformOptionsFromEnv / TestWaveformLoopDisabled /
  TestWaveformLoopWritesShadow。
- tests/e2e +1：TestV0410CollectE2E（REST 轮询注入值 + 10kHz 波形特征提取经真实
  双进程上云：domFreq≈50Hz、rms>0.3、六特征键全量）。
- 合计：单测 21 例 + e2e 1 例（grep 口径，与 RELEASE-NOTES-v0410 N3 一致）。

## 边界登记（随文档落 KI §42）
- 真串口传输未实现（串口硬件依赖，联调待硬件；RTU-over-TCP 为串口-网关主流形态）；
- RTU-over-TCP 非标准 Modbus 封装（主流网关形态，明确登记）；
- FFT 为 radix-2 2 的幂（非 2 幂补零）；包络谱为最小实现（|x| 去均值 → FFT），
  非完整 Hilbert 解调；
- 波形模拟源按仿真速率出块（非实时采样——墙钟块间隔与仿真块时长解耦）；
- REST 采集器为单设备/单端点（多端点聚合为后续候选）；轮询形态取一（推送接收
  登记后续候选）；
- 波形特征频率分辨率 = rateHz/块长（10kHz/4096 ≈ 2.44Hz）；原始波形落 tsdb 默认
  off（10k 点/s 量级，开启需评估容量水位）。
