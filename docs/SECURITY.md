# 云边通道安全（mTLS）设计文档

> WBS 7.1 证书管理 + 7.4 云边认证 · 基础版
> 关联代码：`pkg/certs`（证书管理）、`cloud/pkg/cloudhub`（服务端 TLS 接入）、
> `edge/pkg/edgehub`（客户端 TLS 接入）、`hack/gen-certs.sh`（一键生成）

## 1. 架构

```
                    ┌────────────────────── 云（CloudCore）──────────────────────┐
                    │  CloudHub 监听 :10000（HTTP/WS → TLS 层叠加）              │
                    │  tls.Config{                                            │
                    │    Certificates: cloudcore.crt/key   ← 向边缘证明云身份   │
                    │    ClientCAs:    ca.crt               ← 信任的边身份 CA   │
                    │    ClientAuth:   RequireAndVerifyClientCert  ← 强制双向   │
                    │  }                                                       │
                    └───────────────▲──────────────────────────────────────────┘
                                    │ wss:// 双向 TLS（TLS1.2+）
                                    │ 边→云：edgecore 客户端证书（EKU clientAuth）
                                    │ 云→边：cloudcore 服务端证书（EKU serverAuth）
                    ┌───────────────┴──────────────────────────────────────────┐
                    │  EdgeHub（边缘）                                          │
                    │  tls.Config{                                            │
                    │    Certificates: edgecore.crt/key  ← 向云证明边身份       │
                    │    RootCAs:      ca.crt            ← 校验云身份           │
                    │  }                                                       │
                    └──────────────────────────────────────────────────────────┘
```

- **单 CA 模型**：自签 CA（CN=edgeflow-ca）统一签发云、边两侧证书。信任根唯一，
  部署简单；后续如需分集群/分租户隔离，可扩展为多 CA（见 §5 生产注意事项）。
- **双向认证**：云端强制要求客户端证书（`RequireAndVerifyClientCert`），
  边缘侧校验服务端证书链与主机名（SAN 127.0.0.1/localhost/cloudcore）。
  任何一侧无法验证对方证书 → TLS 握手失败 → 连接在协议层之前被拒绝。
- **算法**：RSA 2048（兼容性最广，边缘设备生态与 openssl 工具链支持完备；
  握手为低频操作，性能开销可忽略；如需降低边侧开销可平滑迁移 ECDSA P256）。
- **协议**：强制 TLS 1.2+（拒绝 TLS1.0/1.1）。

## 2. 启用方式（开关与证书目录）

| 组件 | 开关（=on 启用） | 证书目录环境变量 | 默认目录 |
|------|------------------|------------------|----------|
| CloudHub | `EDGEFLOW_CLOUDCORE_TLS` | `EDGEFLOW_CLOUDCORE_CERT_DIR` | `data/certs/` |
| EdgeHub | `EDGEFLOW_EDGECORE_TLS` | `EDGEFLOW_EDGECORE_CERT_DIR` | `data/certs/` |

未设置开关时行为与历史版本完全一致（纯 `ws://`，无 TLS）。两侧开关需**同时**开启：
云端只开 TLS 会拒绝明文连接；边缘只开 TLS 无法通过明文云端的握手。

证书布局（`ca.crt / ca.key / cloudcore.crt / cloudcore.key / edgecore.crt / edgecore.key`）：

| 文件 | 用途 | 有效期 | 关键属性 |
|------|------|--------|----------|
| `ca.crt` / `ca.key` | 自签 CA | 10 年 | CN=edgeflow-ca，IsCA，CertSign |
| `cloudcore.crt` / `cloudcore.key` | 云服务端证书 | 1 年 | SAN: 127.0.0.1/localhost/cloudcore，EKU serverAuth |
| `edgecore.crt` / `edgecore.key` | 边客户端证书 | 1 年 | CN=edgeflow-<nodeID>，EKU clientAuth |

权限：全部私钥文件 `0600`，证书文件 `0644`。

## 3. 证书生命周期

### 3.1 生成（幂等）

- 组件首次以 TLS 启动时**自动生成**（`pkg/certs.EnsureCA/EnsureServerCert/EnsureClientCert`）：
  证书目录不存在自动创建；目标文件已存在则加载并校验，**绝不重新生成**。
- 也可用 `./hack/gen-certs.sh` 在启动前预置（CI/手动部署），与 Go 生成逻辑同布局
  同参数（openssl 实现）。
- 部分状态保护：只存在证书或私钥其一 → 报错并拒绝启动（防止误用残留密钥、
  防止静默轮换 CA 导致全部叶子证书失效）。

### 3.2 加载与校验

- 启动时完整解析：PEM 格式、证书链、CA 的 IsCA 标志、公私钥匹配
  （`tls.LoadX509KeyPair` 内置校验）。文件损坏 → 启动失败（fail-fast，不降级）。

### 3.3 轮换（基础版：人工编排）

1. 签发新证书（`hack/gen-certs.sh` 会因文件已存在而跳过——轮换前先备份并删除
   旧文件，或使用新的证书目录）；
2. 滚动重启组件（先云后边：云端加载新服务端证书，边缘侧重连时自动用新证书）；
3. 验证新连接建立后，清除旧证书文件。

轮换 CA 需**全量重签**所有叶子证书（旧叶子证书随 CA 更换立即失效）。

## 4. 故障与拒绝路径（预期日志）

| 场景 | 现象 |
|------|------|
| 明文 ws:// 连 TLS 端口 | 云侧 `http: TLS handshake error ... first record does not look like a TLS handshake`；边侧拨号失败 |
| 边侧无客户端证书 | 云侧 `tls: client didn't provide a certificate`；边侧 `remote error: tls: certificate required` |
| 边侧携带非本 CA 证书 | 云侧 `tls: unknown certificate authority`；握手失败，连接关闭 |
| 边侧不信任云证书 | 边侧 `x509: certificate signed by unknown authority` / 主机名校验失败 |
| 握手成功 | 云侧日志 `mTLS 连接已认证（peer CN=edgeflow-<nodeID>...）` + 正常注册流程 |

## 5. 生产注意事项与已知限制（基础版）

**安全基线（已实现）**：

- [x] 双向认证（云验证边证书、边验证云证书与主机名）
- [x] 私钥文件权限 0600
- [x] 强制 TLS 1.2+
- [x] 证书文件损坏/不完整时 fail-fast

**已知限制（后续版本计划）**：

- **CA 私钥保护**：本版本 CA 私钥仅靠文件权限保护。生产环境建议：
  1) 使用 `hack/gen-certs.sh` 离线签发后，将 CA 私钥移出节点（仅保留 ca.crt）；
  2) 或接入 KMS/HSM 等密钥管理服务；3) 云、边证书目录分离部署。
- **吊销未实现**：无 CRL/OCSP。一旦某节点证书泄露，当前只能通过轮换 CA +
  全量重签（或部署侧防火墙封禁该节点）处置。吊销列表（CRL）与 OCSP
  响应器接入在后续版本规划中。
- **CSR 流程未实现**：证书由本地直接生成签发（私钥不出节点），未提供
  中心化 CA + CSR 审批流（对标 KubeEdge certgen 的扩展点）。
- **CN 与 nodeID 未绑定校验**：云端验证的是「证书由可信 CA 签发」，
  不校验证书 CN 与消息中 nodeID 的映射关系。需要「证书↔节点」强绑定时，
  可在注册流程中增加 CN 校验（信息已在 TLS 层可用）。
- **边缘侧客户端证书为整机共享**：一台边缘节点的所有 edgecore 实例共用
  同一对证书（单证书目录）。多实例/多租户隔离需每实例独立目录。
- **CheckOrigin 仍全放行**：TLS 层已完成身份认证，HTTP Origin 校验作为
  纵深防御项（防浏览器侧 CSWSH）在后续版本收紧。

## 6. 测试覆盖

- `pkg/certs` 单元测试：生成→加载往返、幂等（二次调用不重签）、私钥权限、
  目录自动创建、损坏文件/不完整对报错；
- `pkg/certs` 握手集成测试：真实 TLS 握手——带证书成功、不带证书被拒、
  错误 CA 签发证书被拒；
- `cloudhub` 集成测试：mTLS 上完整 WebSocket 注册流程、明文拨 TLS 端口被拒、
  不可信客户端证书被拒；
- `edgehub` 集成测试：mTLS 上连接→注册→心跳全流程、不信任服务端证书被拒、
  地址归一化（ws:// → wss://）；
- 端到端：cloudcore + edgecore 双进程实测（见交付说明 §7）。

## 已知限制与跨主机部署（M4B P1-4）

服务端证书 SAN 默认仅含本机回环（127.0.0.1/localhost/cloudcore），**mTLS 通道默认只在本机可用**。跨主机/集群部署必须显式注入 SAN：

```bash
# Go 装配（cloudcore 启动前设置）
export EDGEFLOW_CLOUDCORE_TLS_SAN="IP:10.0.0.5,DNS:cloudcore.edgeflow.svc"

# 或预置证书脚本
TLS_SAN="IP:10.0.0.5,DNS:cloudcore.edgeflow.svc" ./hack/gen-certs.sh
```

- 边缘侧连接的地址（`EDGEFLOW_EDGECORE_CLOUD_ADDR=wss://<host>:<port>`）必须在服务端证书 SAN 内，否则握手失败。
- CA 私钥仅文件权限保护（0600）；生产建议离线签发或接入 KMS。
- 证书轮换需人工编排（备份→删除旧证书→滚动重启，先云后边）；吊销（CRL/OCSP）未实现。
