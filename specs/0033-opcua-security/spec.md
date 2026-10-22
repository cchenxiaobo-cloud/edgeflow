# spec 0033 — OPC-UA 安全收口：D4 TokenID 严格比对 + D5 PSHA 规范化（v0.60.0）

> 版本：v0.60.0 ｜ 缺口：replan 持续项 D4（MSG 入站 TokenID 严格校验，KI §30）
> + D5（deriveKeys KDF 规范化，KI §28/§29）｜ 上游：spec 0028/0029
> 说明：本版为 OPC-UA 安全面库修复：零 API 变更（契约 134 维持、消息 19 不变）。

## 背景与范围裁决

KI 登记三项 OPC-UA 安全面遗留：D4「MSG 入站 TokenID 未严格校验」（HMAC 兜底、
TokenID 不比对）、D5「deriveKeys 链式 SHA-1 近似，与规范 KDF 逐字节一致性待
验证」。本版一并收口：

**裁决：D4 = 入站帧 TokenID 与所试密钥组令牌严格比对（不匹配显式拒绝）；
D5 = deriveKeys 从 SHA-1 链式近似升级为规范 PSHA（HMAC-SHA256，Part 6
§6.7.5.2），证书退出对称密钥派生。**

## 裁决

1. **D4 TokenID 严格比对**：openIncoming 按 OpenMSGFrame 返回的帧内 TokenID
   与所试密钥组令牌校验——cur 通过则须 ==cur.tokenID，回退 prev 通过则须
   ==prev.tokenID，不匹配显式拒绝（构造异常 fail-loud）；换钥窗口（prev）仍
   接受在途帧；未知密钥帧两组试钥均失败保持拒绝。
2. **D5 PSHA 规范化**：deriveKeys 按规范实现 PSHA——A1=HMAC-SHA256(secret,
   seed)、An=HMAC-SHA256(secret, A(n-1))、块 Bi=HMAC-SHA256(secret, Ai||seed)
   级联取 128 字节；secret=serverNonce、seed=clientNonce（客户端视角固定角色，
   双侧同角色派生同一密钥组）；**证书退出派生**（nonce 是唯一密钥材料输入，
   签名/加密的证书用途不变）。
3. **密钥切片顺序**（Part 6 Table 26，Basic256Sha256）：每方向
   EncryptionKey(16)→MessageKey/MAC(32)→IV(16)，客户端三元组后服务端三元组。
4. **Golden 锚**：固定 nonce 输入的六段密钥 hex 内联锚（防 KDF 漂移）；
   结构正确性由 PSHA A 链实现保证；**与 OPC 基金会官方向量/真实服务器的
   外部互通验证仍登记为前提**（诚实性：本版实现规范化 ≠ 已完成互操作验证）。
5. **只增不改**：零 API 变更（契约 134 维持）；DeriveKeys 导出签名去 cert 参数
   （证书退出派生的必然结果，调用面 client/sim 全量更新）；client↔sim 互通由
   既有 renew/e2e 测试保障（同函数同输入）。
6. **非目标**：服务端侧（opcuasim）入站 TokenID 镜像校验（sim 为测试仿真器，
   D4 面在客户端 SecureChannel）、Basic128Rsa15 策略、PSHA 官方向量外部比对
   立项、OPC-UA 帧级限流。

## 端点

无新增（134 维持）。

## 交付锚

- 实际交付（2026-10-22）：单测 2 例（PSHA golden 锚 / TokenID 严格比对三态）
  + 既有 8 处适配（v0280/v0281/v0290 去 cert 参数，确定性/双侧一致性断言
  保留）；race 两包全绿（opcua 13.867s / opcuasim 8.846s——D4/D5 改造后
  client↔sim 互通正常）；零 API 变更（契约 134 维持）。
- e2e 轨迹：run1 golden 采集 + TokenID 测试帧方向反了（Seal fromClient 应为
  false——模拟服务端帧）→ 修正后全绿。
- R4（v0.60+v0.61 合并复核 + 全量 e2e 终验 -timeout 30m）随 v0.61 执行。
