# RELEASE-NOTES v0.28.1（2026-09-03）

## 1. 版本主题：OPC-UA OPN 体加密与 Basic256Sha256 端到端互通（分段第二段）

按《开发规范》FR-S1-04 分段计划与 ROADMAP §23 F-5 归属推进。本段把 v0.28.0 的安全策略框架接通为**端到端可用**：客户端 OPN 请求体加密+签名，服务端（opcuasim opt-in）对等解封验签并回送加密响应，双侧完成对称密钥协商。规格见 specs/0001-opn-body-encryption/spec.md（spec-kit 首个 SDD 特性）。

## 2. 新增能力

| # | 能力 | 落点 | 要点 |
|---|---|---|---|
| 1 | 客户端加密 OPN | securechannel.go sendOPN B256 分支 | 线格式 = AsymHeader(证书+指纹) + SignatureData{rsa-sha1, RSA-PKCS1v15-SHA1(AsymHeader线编码‖legacyBody)} + EncryptedData(RSA-OAEP-SHA1(serverPub, ClientNonce(32B)‖legacyBody))；legacyBody 与 None 路径同编码（SequenceHeader+旧三字段） |
| 2 | 服务端对等处理 | opcuasim `WithIdentity(cert, key)` opt-in | 解封（服务端私钥）→ 验签（客户端证书公钥）→ 校验 thumbprint 指向/RequestType=Issue → ServerNonce(crypto/rand 32B) → 派生密钥 → 加密 OPN 响应（含 ServerNonce 前缀+响应体，服务端私钥签名）；失败逐路径回 ERR（Bad_DecodeError/BadSecurityChecksFailed/BadApplicationSignatureInvalid）断连，不静默降级 |
| 3 | 客户端响应解封 | recvOPN B256 分支 | 解 SignatureData → Unwrap（客户端私钥）→ ServerNonce 前缀拆包（长度=32 且与响应字段逐字节一致双校验）→ 验签（服务端公钥）→ 复用 v0.28.0 pin/thumbprint 校验 → 派生存储 |
| 4 | 密钥协商 | DeriveKeys/DerivedKeys 导出 | 客户端与服务端用同一链式 SHA-1 函数、同输入（双 nonce+双证书）→ 六段密钥逐字节一致（测试双向核对）；存 SecureChannel.keys / connSession.b256Keys（v0.29.0 MSG 对称覆盖接线） |
| 5 | 导出编解码 | EncodeSignatureData/DecodeSignatureData | server_api 薄包装，服务端互操作面 |

## 3. 兼容性与语义保持

- **None 路径逐字不变**：OPN 请求/响应、MSG、CLO 线上格式与代码路径零改动；`Open`/`OpenSecureChannel`/`OpenSecureChannelWith` 签名不变。
- **sim 不注入身份 = v0.28.0 行为**：非 None 策略显式拒绝（ERR Bad_SecurityPolicyRejected），v0280 冻结测试零改动全绿。
- 契约 42 端点不变；零第三方依赖（仅标准库 crypto/*）。

## 4. 测试

- 新增 v0281 测试 **6 例全绿**：包内 4（Wrap roundtrip+篡改、Sign/Verify+篡改+错钥、派生确定性+长度+nonce 敏感、客户端/服务端密钥双侧一致）+ e2e 2（**WithIdentity 端到端 B256 互通 + MSG 探针**、无身份仍显式拒绝）。
- 受影响三包 -race 绿（见 KNOWN-ISSUES §29 既有 flake 登记）；全仓 `go test ./...` EXIT=0。

## 5. 边界（如实登记，非承诺）

- MSG/CLO 对称覆盖（AES-128-CBC + HMAC-SHA1 全帧）留 **v0.29.0**——本段派生密钥已就绪但未用于 MSG 帧，B256 通道的 MSG 仍为明文对称头；CLO 沿用 v0.28.0 策略感知明文发送。
- 线格式偏差：ClientNonce 直接作为加密体前缀（未扩展 RequestHeader/MessageSecurityMode 字段）——避免双重请求格式与冻结带冲突，登记 KNOWN-ISSUES §29。
- 互通性验证方式为**自研栈双向交叉验证**（client ↔ sim 镜像实现）；OPC 基金会互操作样例向量比对留 v0.29.0。

## 6. 场景归属

S1 物联网数据采集 / FR-S1-04（docs/DEVELOPMENT-SPEC.md §3.1）。
