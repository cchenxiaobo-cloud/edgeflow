# RELEASE-NOTES v0.28.0（2026-09-01）

## 1. 版本主题：OPC-UA 安全策略框架（Basic256Sha256 · 分段第一段）

按《开发规范》FR-S1-04 分段交付。本段把 OPC-UA 安全通道的「策略门禁 + 密码学原语 + 证书指纹校验 + 服务端显式拒绝」骨架钉死并全量测试；OPN 体加密与 MSG 对称覆盖按分段计划续作（见 §4）。

## 2. 新增能力

| # | 能力 | 落点 | 要点 |
|---|---|---|---|
| 1 | 安全策略选项 | `OpenSecureChannelOptions{SecurityPolicyURI, ClientCert, ClientKey, ServerCert}` + `(*Conn).OpenSecureChannelWith` | 零值 = v0.27.0 None 行为逐字一致；Basic256Sha256 要求三证书字段齐备，缺失即拒（PROT-1 防半加密）；未知策略（Basic128Rsa15/Basic256 等）本地拒绝不拨号 |
| 2 | 客户端入口 | `opcua.OpenWithOptions(endpoint, timeout, opts)` | 与 `Open` 共享实现；None 路径线上行为不变 |
| 3 | 密码学原语（纯标准库） | `pkg/opcua/security_basic256sha256.go` | Part 6 §6.7.5 链式 SHA-1 密钥派生（AES-128 key 16B 截断 + MAC key 32B 补零、确定性）；RSA-OAEP-SHA1 封包/解封；RSA-PKCS1v1.5-SHA1 签名验证；AES-128-CBC（PKCS#7）；HMAC-SHA1 签名/验证（`hmac.Equal` 常时比较）；SHA-1 证书指纹（DER） |
| 4 | OPN 非对称头协商 | sendOPN/recvOPN 策略分支 | B256 路径发送 SenderCertificate + ReceiverCertificateThumbprint（服务端证书 SHA-1 指纹）；响应校验：策略 URI 匹配 + 服务端证书与 pin 逐字节一致 + thumbprint 等于本端客户端证书指纹；None 路径 PRT-08 语义与错误文案逐字保留 |
| 5 | 服务端显式拒绝 | pkg/opcuasim sim.go | 非 None 策略 OPN → ERR（0x80550000 Bad_SecurityPolicyRejected）后断开，绝不静默降级为 None |
| 6 | CLO 策略感知 | sendCLO | 沿用协商策略与证书字段 |

## 3. 测试与兼容性

- 新增 v0280 测试 **15 例全绿**（策略门禁 5、密码学原语 5、OPN 响应校验 2、e2e 拒绝链路 2 + 拨号前拒绝 1）；受影响三包（pkg/opcua、pkg/opcuasim、mappers/opcua）`-race` 绿。
- 冻结测试带（v0240/v0250/v0260/v0270 全套）**零改动全绿**；契约 42 端点不变；零第三方依赖（仅标准库 crypto/*）。
- 全仓 `go test ./...` EXIT=0（39 包 + e2e）。

## 4. 分段计划（如实登记，非承诺）

- **v0.28.1**: OPN 体加密——`OpenSecureChannelRequest` 扩展 RequestHeader/ClientNonce/MessageSecurityMode（线上格式扩展需与冻结测试协调）+ RSA-OAEP 封 ClientNonce + sim 对等解密 + 端到端 B256 OPN 互通。
- **v0.29.0**: MSG/CLO 对称覆盖（AES-128-CBC + HMAC-SHA1 全帧）+ 密钥续期（Renew）。
- 本段 B256 通道**未达端到端可用**（sim 显式拒绝），仅供策略协商与校验框架先行验证——生产请继续使用 None + 传输层 TLS 替代方案。

## 5. 场景归属

S1 物联网数据采集 / FR-S1-04（开发规范 docs/DEVELOPMENT-SPEC.md §3.1）。
