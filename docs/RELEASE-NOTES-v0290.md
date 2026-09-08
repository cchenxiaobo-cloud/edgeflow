# Release Notes — v0.29.0（2026-09-08）

**主题：OPC-UA MSG/CLO 对称覆盖与显式安全令牌续期**——Basic256Sha256 安全栈分段第三段（完整闭环）：v0.28.0 策略框架 → v0.28.1 OPN 体加密 → **本段 MSG/CLO 全帧对称覆盖 + Renew**。

开发规范：本版按《EdgeFlow 开发规范 v1.0.0》SDD 流程交付（spec-kit：specs/0002-msg-symmetric-renew/spec.md，规格先行 → 实现 → 门禁 → 复核 → 文档 → 发布）。

---

## 1. 新特性

### 1.1 MSG/CLO 对称覆盖（FR-S1-04 分段，US-1/2/3）
Basic256Sha256 通道上全部 MSG 与 CLO 帧加密 + 签名，序列号/请求 ID/服务体不再明文上网：

- **线格式**（Part 6 §6.7.5 对称布局，大端与仓库 UA Binary 编码器一致）：
  `Header(12B, 含 ChannelID) ‖ TokenID(4B) ‖ CT ‖ Footer(HMAC-SHA1, 20B)`
- **明文** = `SequenceHeader ‖ 服务体 ‖ PadSize(1B) ‖ Padding×PadSize`（§6.7.4 尾垫，`(len+1+PadSize) ≡ 0 mod 16`）
- **CT** = AES-128-CBC(发送方 EncryptKey/IV)；**Footer** = HMAC-SHA1(发送方 MACKey, Header‖TokenID‖CT)，覆盖含 MessageSize 的完整帧头，**常时比较**
- 方向密钥严格分离：客户端收发用 Client*，服务端用 Server*
- 新原语：`SealMSGFrame` / `OpenMSGFrame`（导出）；`Conn.WriteFrameRaw`（限长校验与 WriteMessage 一致）

### 1.2 显式安全令牌续期（US-4）
- `Client.Renew(timeout)`：在现有加密通道内发送 `ClientNonce(32B) ‖ OpenSecureChannelRequest{RENEW}`（44B 形状指纹网关），服务端生成新 TokenID/新 ServerNonce、按 renew 交换的 nonce 对派生新密钥组、以**旧出站组**密封响应；客户端校验（ServiceResult/Lifetime/TokenID/ServerNonce 长度）后原子换组（`SecureChannel.swapKeys`，旧组保留作在途回退）；sim 收到**首个新钥帧**才切换出站组——换钥窗口两侧收敛由 TCP 单通道序保证，无竞态
- 复用 roundTrip 泵机制：订阅活跃时续期安全
- None 通道 Renew 显式拒绝（不静默）

### 1.3 悬挂 Publish 竞态修复 + KeepAlive 长轮询自愈（既有缺陷，续期时序放大暴露）
- **所有权校验**：武装 goroutine 只服务自己的 reqID（`cs.pubReqID != sh.RequestID` 即退出）——修复陈旧 goroutine 以旧 reqID 抢答 KeepAlive 压制新悬挂请求（订阅通知永久静默）
- **KeepAlive 自动重挂**：客户端识别空通知 seq=0（PRT-23 sim 约定）后自动重挂发布窗口（不投递、不推进 lastPubSeq，pubCh 行为不变）——修复「KeepAlive 被防重放丢弃后无人重挂」的长轮询死循环

### 1.4 传输级篡改 e2e（补 v0.28.1 复核 US-4 缺口）
- 密封帧密文翻转 1 bit → sim `ERR Bad_SecurityChecksFailed` 断连，无静默降级

### 1.5 整体功能架构图
- `docs/architecture-overview.svg` 入库并嵌入 README（GitHub 主页）：六层（接入与部署 / 云端 CloudCore / 边缘 EdgeCore / 协议映射 / 共享库 / 质量基建）+ 设备数据流与模型分发流双泳道

## 2. 兼容性

- **None 路径逐字不变**：HEL/ACK/OPN/MSG/CLO、sendSecure/recvSecure/pump 的 None 分支、handleService 明文路径全部保留（dispatchService 为纯抽取重构，线上行为零变化）
- **冻结测试带零改动**：v0240–v0281 全部冻结测试原样全绿
- **契约 42 端点不变**；零第三方依赖（仅 crypto/* + stdlib）
- 公开 API 增量：`Client.Renew` / `Client.TokenID` / `Client.ProbeKeyMaterial` / `Client.ProbeWriteRaw`（后两者为测试探针）/ `SealMSGFrame` / `OpenMSGFrame` / `Conn.WriteFrameRaw`

## 3. 质量证据

| 门禁 | 结果 |
|---|---|
| v0290 新测试 | 7/7 全绿（包内 5：密封往返/篡改/尾垫律/换组语义/Renew 拒绝 None；e2e 2：跨续期订阅全链路 + 传输级篡改） |
| 三包 -race | pkg/opcua 12.1s / pkg/opcuasim 9.4s / mappers/opcua 12.0s 全绿 |
| gofmt / build / vet | 净 |
| 全仓 `go test ./...` | **EXIT=0**（39 包，e2e 353.97s） |
| 契约 | 42 端点冻结不变 |

## 4. 边界与登记（KNOWN-ISSUES §30）

- Renew 44B 形状指纹网关为本仓自研栈约定（规范为 RequestHeader 形态）；互操作时需重评（与 §29 同源）
- MSG 入站 TokenID 不做严格比对（HMAC 承担帧认证，换钥窗口内避免误杀）——设计取舍登记
- 自动续期定时器（75% 寿命触发）未实现，需显式调用 `Client.Renew`
- OPC 基金会互操作向量比对仍 pending（接真实第三方服务器前必须补）
- mappers race flake 本轮未触发，沿用 §29 登记

## 5. 下一版本候选（v0.30.0+）

- OPC 基金会互操作向量比对（KDF/线格式）+ 真实服务器互通验证
- 自动续期（75% 寿命触发器）+ 令牌过期前平滑切换
- MQTT 5.0 阶段一（版本参数化 + 原因码 + 流控，非承诺）
- mapper 自动 Resume 与 opcua 客户端 Renew 接线（断线重连后直接续期而非重开通道）
