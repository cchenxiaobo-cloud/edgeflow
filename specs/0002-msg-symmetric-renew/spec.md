# Spec 0002: OPC-UA MSG/CLO 对称覆盖与安全令牌续期（v0.29.0）

**状态**: implemented（本版本） ｜ **场景**: S1 数据采集 ｜ **FR**: FR-S1-04 续段（开发规范 §3.1）
**分段**: v0.28.0 框架段 → v0.28.1 OPN 体加密段 → **本段（MSG/CLO 对称覆盖 + Renew）** → 后续（互操作向量比对 / 自动续期）

## User Stories

### P1 — MSG 帧机密性与完整性（US-1）
作为 SDK 使用者，我要 Basic256Sha256 通道上的全部 MSG 服务帧（请求与响应）以 AES-128-CBC 加密并以 HMAC-SHA1 足迹封尾，使序列号、请求 ID 与服务体不再明文上网。
**Independent Test**: 抓 MSG 帧 → TokenID 之后到帧尾前无明文序列头/服务体；篡改密文 1 bit → 对端 ERR Bad_SecurityChecksFailed 断连（e2e 传输级用例）。
**Given** B256 通道 **When** 任意 MSG 帧 **Then** 帧 = Header(12B, 含 ChannelID) ‖ TokenID(4B) ‖ CT ‖ Footer(HMAC-SHA1 20B)；明文 = SequenceHeader ‖ 服务体 ‖ PadSize(1B) ‖ Padding×PadSize（Part 6 §6.7.4 尾垫，(len+1+PadSize) ≡ 0 mod 16）；CT = AES-128-CBC(发送方 EncryptKey/IV)；Footer = HMAC-SHA1(发送方 MACKey, Header ‖ TokenID ‖ CT)（覆盖含 MessageSize 的完整帧头；常时比较）。

### P1 — 服务端对等解封与异步帧覆盖（US-2）
作为部署者，我要 opcuasim（WithIdentity）对 B256 通道的 MSG 入站帧按「新钥优先 / 旧钥回退」解封，响应帧（同步响应、悬挂 Publish、KeepAlive）全部密封出站。
**Independent Test**: e2e——加密通道上 Read / Write / Publish 订阅通知全链路可用；Renew 后新钥帧可达。
**Given/When/Then**: 解封失败 → ERR Bad_SecurityChecksFailed + 断连，无静默降级；方向密钥严格分离（各端发送用本方向 EncryptKey/IV/MACKey，接收用对方向）。

### P1 — CLO 对称覆盖（US-3）
B256 下 CLO 帧体（SequenceHeader）同样密封；sim 验封失败 → ERR + 断连，成功 → 关连接。None CLO 逐字不变。

### P2 — 显式安全令牌续期（US-4）
作为 SDK 使用者，我要 Client.Renew(timeout)：在现有加密通道内发送 ClientNonce(32B) ‖ OpenSecureChannelRequest{RENEW}（44B 形状指纹网关），sim 以旧出站组密封响应（新 TokenID + 新 ServerNonce），客户端收到后原子换组（旧组转在途回退）；sim 收到首个新钥帧后出站才切新组（TCP 单通道序确定性收敛，杜绝换钥窗口竞态）。
**Independent Test**: e2e——Renew 后 TokenID 变化、Read 仍通、订阅通知跨续期继续；Renew 拒绝 None 通道。

### P2 — 冻结兼容（US-5）
None 路径（HEL/ACK/OPN/MSG/CLO）逐字不变；v0240–v0281 冻结测试零改动全绿；契约 42 端点不变；零第三方依赖（仅 crypto/* stdlib）。

## Requirements
- FR-1 SealMSGFrame/OpenMSGFrame 导出原语（密封/解封完整帧，Part 6 §6.7.5 布局 + §6.7.4 尾垫）
- FR-2 客户端 sendSecure/recvSecure/pump 三路径 B256 分支（None 逐字保留）
- FR-3 sim 入站解封网关（新钥优先/旧钥回退 + 首个新钥帧出站切组）与出站密封（同步 writeResp 快照 + 异步 writeServerFrame）
- FR-4 CLO 密封（客户端）+ 验封（sim）
- FR-5 Client.Renew：roundTrip 复用 + 响应校验（ServiceResult/Lifetime/TokenID/ServerNonce 长度）+ 原子换钥
- FR-6 sim handleB256Renew：新 TokenID/ServerNonce 生成、DeriveKeys(新 ClientNonce+新 ServerNonce) 派生、旧出站组回响应、入站换组
- NFR: 零第三方依赖；HMAC 常时比较；42 端点不变；SHA-1 限规范强制处（nolint 登记）

## Out of Scope（登记 KNOWN-ISSUES §30）
自动续期定时器（75% 寿命触发）、OPC 基金会互操作向量比对（沿用 §29 登记）、多 chunk 分片、Basic128Rsa15、MSG 入站 TokenID 严格校验（HMAC 已提供帧认证，登记）。

## 实现偏差登记（as-built）
- **冻结带 None 路径代码文本口径（复核 P2-6）**：sendSecure（seqE 前置 + ks 分支插入）与 pumpLoop（if/else 包裹 parseSecureFrame）的 None 代码文本被重排；线上输出经逐字节比对等价，US-5 "逐字不变"按**行为口径**执行，文本口径以本条为准（sendCLO/handleService/recvSecure-None 等其余路径代码文本逐字未动）。
- **prev 密钥组生命周期（复核 P2-2）**：换钥后旧组保留至下次 Renew（不主动清除），覆盖对端出站尚未切换窗口的在途帧；帧认证仍由 HMAC 把关，不构成伪造通道。
- 规格初稿曾按「Renew 响应以新组密封」推演；实现裁定为**旧出站组密封 + 首个新钥帧切组**（消除客户端换钥窗口竞态，理由见 US-4），与 US-4 描述一致。
- Renew 网关采用 44B 形状指纹（ver=0 ‖ RENEW ‖ 600000）而非规范 RequestHeader 形态——本仓服务分发为形状匹配制，与 spec 0001 的 ClientNonce 前缀偏差同源，登记 §30。
