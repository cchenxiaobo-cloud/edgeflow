# EdgeFlow v0.60.0 发布说明（OPC-UA 安全收口：D4 TokenID + D5 PSHA）

> 版本：v0.60.0 ｜ 日期：2026-10-22 ｜ spec：0033-opcua-security ｜ 缺口：持续项 D4/D5
> 基线：v0.59.0（966cff5）。**本版为 OPC-UA 安全面库修复：零 API 变更（契约 134 维持、消息 19 不变）。**

## N1 能力

- **D4 TokenID 严格比对（KI §30 处置）**：openIncoming 按 OpenMSGFrame 返回的
  帧内 TokenID 与所试密钥组令牌校验——cur 通过须 ==cur.tokenID、回退 prev 通过
  须 ==prev.tokenID，不匹配显式拒绝（构造异常 fail-loud，不再仅靠 HMAC 兜底
  试钥）；换钥窗口（prev）仍接受在途帧；未知密钥帧两组试钥均失败保持拒绝。
- **D5 PSHA 规范化（KI §28/§29 处置）**：deriveKeys 从 SHA-1 链式近似升级为
  规范 PSHA——A1=HMAC-SHA256(secret, seed)、An=HMAC-SHA256(secret, A(n-1))、
  块 Bi=HMAC-SHA256(secret, Ai||seed) 级联取 128 字节；secret=serverNonce、
  seed=clientNonce（固定角色，双侧同角色派生同一密钥组）；**证书退出对称密钥
  派生**（nonce 是唯一密钥材料输入；证书的签名/加密用途不变）。
- **密钥切片顺序**（Part 6 Table 26）：每方向 EncryptionKey(16)→MessageKey/MAC(32)→
  IV(16)，客户端三元组后服务端三元组（Basic256Sha256 参数）。
- **Golden 锚**：固定 nonce 输入六段密钥 hex 内联（防 KDF 漂移）。

## N2 兼容

- 端点 134 不变、消息 19 不变；契约四件套零改动。
- **DeriveKeys 导出签名变更**（去 cert 参数——证书退出派生的必然结果）：调用面
  client.go/securechannel.go/opcuasim 全量更新；client↔sim 互通由既有 renew/
  e2e 测试保障（同函数同输入）。
- 密钥流与旧 SHA-1 近似**不兼容**（对称密钥完全不同）——通道为会话态（不持久），
  重连即重新协商，无存量兼容负担。

## N3 测试（新增 2 例 + 既有 8 处适配）

- `pkg/opcua/v0600_test.go` 2 例：PSHA golden 锚（固定 nonce 六段 hex）/
  TokenID 严格比对三态（正确令牌通过 / 错误令牌显式拒绝 / 未知密钥拒绝）。
- 既有适配：v0280 确定性 / v0281 双侧一致与换 nonce 变化 / v0290 helper——
  去 cert 参数后语义不变（确定性/双侧一致性断言保留）。
- e2e 轨迹：run1 golden 采集（打印模式）+ TokenID 测试帧方向反了（Seal
  fromClient=true 应为 false——模拟服务端帧）→ 修正后全绿。

## N4 边界（已知限制，详见 KI §61）

- 实现规范化 ≠ 互操作验证完成：与 OPC 基金会官方向量/真实服务器的互通验证
  仍为前提（诚实登记，不以自洽替代外部验证）。
- 服务端侧（opcuasim）入站 TokenID 镜像校验不在本版（sim 为测试仿真器）。
- PSHA 无退避/缓存语义（密钥流即时派生，会话态无持久化负担）。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race -count=1`：pkg/opcua **13.867s** / pkg/opcuasim **8.846s** 全绿
  （D4/D5 改造后 client↔sim 互通正常——renew/e2e 既有用例全过）。

## N6 复核

- R1/R2 已处置收口；**R4（v0.60+v0.61 合并复核 + 全量 e2e 终验 -timeout 30m）**
  随 v0.61 执行（R3 = v0.57–v0.59 合并复核已随 v0.59 推送启动）。
