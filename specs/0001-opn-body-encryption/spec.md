# Spec 0001: OPC-UA OPN 体加密与端到端 Basic256Sha256 互通（v0.28.1）

**状态**: implemented（本版本） ｜ **场景**: S1 数据采集 ｜ **FR**: FR-S1-04（开发规范 §3.1）
**分段**: v0.28.0 框架段 → **本段（体加密+互通）** → v0.29.0 MSG 对称覆盖

## User Stories

### P1 — 客户端加密 OPN（US-1）
作为 SDK 使用者，我要在 Basic256Sha256 策略下发起的 OpenSecureChannel 请求体被 RSA-OAEP-SHA1 加密并附客户端私钥签名，使通道凭据不落明文。
**Independent Test**: 抓 OPN 帧 → AsymHeader 之后不再有明文 RequestType 字段；sim 用服务器私钥可解封。
**Given** 完整证书三元组 **When** OpenSecureChannelWith(B256) **Then** OPN 体=EncryptedData(RSA-OAEP(server pub, ClientNonce||legacyBody)) 且 AsymHeader 后附 SignatureData(RSA-PKCS1v15-SHA1)。

### P1 — 服务端对等解密与互通（US-2）
作为边缘部署者，我要 opcuasim 在 opt-in 身份配置下接受 B256 OPN：解封、验签、按 ClientNonce+证书派生对称密钥，并回送加密的 OPN 响应，完成 RFC 级握手闭环。
**Independent Test**: e2e——OpenWithOptions(B256) 连接 sim 成功，channelId/tokenId 非 0，PubAck 探针通过。
**Given** sim WithIdentity(cert/key) **When** 客户端 B256 OPN **Then** 双方派生密钥逐字节一致、响应校验通过。

### P2 — 语义保持（US-3）
未配置身份的 sim 对非 None 策略保持 v0.28.0 显式拒绝（ERR Bad_SecurityPolicyRejected）；None 路径字节逐字不变；v0280 冻结测试零改动全绿。

### P3 — 篡改防护（US-4）
密文任一比特翻转 → 解封失败/验签拒绝，连接断开，不降级。

## Requirements
- FR-1 客户端 OPN 加密体组装（nonce 32B crypto/rand；legacyBody=SequenceHeader+旧三字段请求）
- FR-2 SignatureData 编码（算法 URI http://www.w3.org/2000/09/xmldsig#rsa-sha1）
- FR-3 sim 解封验签（私钥=opt-in；失败→ERR BadSecurityChecksFailed 断连）
- FR-4 双侧 deriveKeys 派生与存储（客户端存 sc.keys；sim 按连接存）
- FR-5 sim 加密 OPN 响应（serverNonce 32B + AES-CBC+HMAC 覆盖，密钥=serverEncrypt/IV/serverMAC）
- FR-6 客户端解密 OPN 响应并复用既有 pin/thumbprint 校验
- NFR: 零第三方依赖；42 端点不变；SHA-1 限规范强制处（nolint 登记）

## Out of Scope（登记 KNOWN-ISSUES）
MSG/CHA 对称覆盖、Renew 续期、证书链校验/CRL、互操作向量比对（用自研栈双向交叉验证替代）。
