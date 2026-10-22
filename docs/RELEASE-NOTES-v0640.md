# EdgeFlow v0.64.0 发布说明（MQTT sink 认证与 TLS）

> 版本：v0.64.0 ｜ 日期：2026-10-22 ｜ spec：0037-mqtt-sink-auth ｜ 缺口：KI §60#2 处置
> 基线：v0.63.0（89267f6）。**本版为 mqtt sink 能力扩展：零新增端点（134 维持）、零新增消息（19 不变）；Rule schema 只增字段（旧快照向后兼容）。**

## N1 能力

- **mqtt sink 认证**：Rule 增 mqttUsername/mqttPassword（mqtt sink 专用；
  GET 面脱敏——hasMqttPass 代替回显；成对校验有密码无用户名 400）；拨号构造
  注入 mqtt.Options Username/Password。
- **mqttTLS**：Rule 增 mqttTLS 位（tls:// 前缀映射）——TLS 拨号
  InsecureSkipVerify（工业现场自签证书常态；CA 验证模式登记 KI）。
- **地址语义分叉**：tls:// 剥前缀后 TLS 拨号；tcp:///mqtt:// 剥前缀后明文拨号。

## N2 兼容

- 端点 134 不变、消息 19 不变；Rule schema 只增字段（旧快照 Load 默认
  mqttUsername=""/mqttPassword=""/mqttTLS=false 向后兼容）。
- webhook 路径零感知（认证/TLS 仅 mqtt sink 生效）。
- GET 面脱敏：mqttPassword 不回显（hasMqttPass 代替）——对齐 Secret 语义。

## N3 测试

- 既有适配：v0530/v0590/v0620 测试 Create 签名 mqttUsername/mqttPassword/
  mqttTLS 参数（默认空/false = 既有行为不变锚）。
- e2e 轨迹：签名适配后全绿。

## N4 边界（详见 KI §66）

- TLS InsecureSkipVerify（不验证服务端证书链——自签证书场景）。
- mqttUsername/mqttPassword 明文存快照（同 Secret 语义，文件 0600）。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race -count=1 ./cloud/pkg/forwardstore/`：**5.389s** 绿（既有全过
  ——认证/TLS 默认关闭兼容锚）。

## N6 复核

- R3/R4 已处置收口；**R5（v0.62–v0.64 合并复核）**随 v0.65 执行。
