# spec 0037 — MQTT sink 认证与 TLS（v0.64.0）

> 版本：v0.64.0 ｜ 缺口：KI §60#2 处置（mqtt broker 认证未接入）｜
> 上游：spec 0032（v0.59 转发出口增强）
> 说明：本版为 mqtt sink 能力扩展：零新增端点（134 维持）；Rule schema 只增字段
> （mqttUsername/mqttPassword/mqttTLS，旧快照向后兼容）。

## 背景与范围裁决

v0.59 mqtt sink 落地时 broker 认证登记为非目标（KI §60#2）。生产 mqtt broker
普遍要求用户名密码认证；工业现场自签 TLS 证书常态。pkg/mqtt 客户端已具备
Username/Password/TLSConfig 完整能力（v0.25.0 TLS 测试在案）——本版接线。

**裁决：Rule 增 mqttUsername/mqttPassword（mqtt sink 专用，GET 面脱敏）+
mqttTLS（TLS 拨号位）；拨号构造注入认证与 TLS。**

## 裁决

1. **Rule schema 扩展**：mqttUsername（≤128B）/mqttPassword（≤128B，GET 面
   脱敏同 secret——hasMqttPass 代替回显）/mqttTLS（tls:// 前缀映射位）；
   mqttPassword 与 mqttUsername 成对校验（有密码无用户名 400）。
2. **TLS 拨号**：MqttTLS=true 或 targetUrl 以 tls:// 开头 → InsecureSkipVerify
   TLS 拨号（工业现场自签证书常态；CA 验证模式登记 KI）。
3. **拨号构造**：mqttPublishQoS1 构造 mqtt.Options 注入 Username/Password/
   TLSConfig；地址语义分叉——tls:// 剥前缀后 TLS 拨号，tcp:///mqtt:// 剥前缀
   后明文拨号。
4. **只增不改**：既有 134 端点零改动；Rule schema 只增字段；webhook 路径
   零感知。
5. **非目标**：CA 证书验证模式（InsecureSkipVerify 先行）、mqttsim TLS 测试
   服务端（需 TLS broker 仿真）、双向 TLS（mTLS）、连接池。

## 端点

无新增（134 维持）；mutes POST schema 扩展（mqtt 认证字段）。

## 交付锚

（交付后回填：测试适配 / race 门禁 / R5 复核安排）
