# EdgeFlow v0.59.0 发布说明（转发出口增强：测试投递与 MQTT sink）

> 版本：v0.59.0 ｜ 日期：2026-10-22 ｜ spec：0032-forward-enhance ｜ 缺口：KI §54#2 反转 + 通知渠道扩展
> 基线：v0.58.0（42bb764）。**本版为纯云端管理面版本：零新增消息（19 不变），端点 133→134（+1）。**

## N1 能力

- **同步测试投递**（POST /api/v1/forwards/{id}/test，operator+）：连通性验证——
  合成事件（event=test, alarmId=test-<id>）绕过事件过滤同步投递；结果
  （delivered/attempts/status/error/deliveryId）同步返回并入台账（Event=test
  与真实事件可区分）；禁用规则 400、未知 404。（KI §54#2 处置）
- **sinkType 扩展**：Rule 增 sinkType（webhook 默认|mqtt）+ topic（mqtt 主题，
  空=edgeflow/forward/<id>）；地址语义分叉——webhook=http(s) 绝对地址（既有），
  mqtt=[tcp://]host:port；schema 只增字段（旧快照 Load 默认 webhook 向后兼容）。
- **mqtt sink 投递**：pkg/mqtt 短连接（每次投递 dial→PUBLISH QoS1→等
  PUBACK→DISCONNECT；ClientID=edgeflow-forward-<ts 随机>）；成功 = QoS1 确认
  完成；重试沿用 ≤3 次退避；HMAC 不适用（broker 侧认证登记 KI）。
- **测试投递双路**：sinkType=webhook → HTTP POST（含签名）；mqtt → QoS1 发布；
  台账统一 Event=test。

## N2 兼容

- 端点 133→134（+1）：见 API-COMPATIBILITY §1 矩阵 134 与 v0.59.0 增量节。
- 消息 19 不变（mqtt sink 为出站外部协议，非云边消息面）。
- Rule schema 只增字段：旧快照 Load 默认 sinkType=webhook 向后兼容；既有
  webhook 语义逐字节不变。

## N3 测试（新增 4 例 + e2e 两段）

- `cloud/pkg/forwardstore/v0590_test.go` 4 例：测试投递（webhook delivered +
  payload event=test + 台账）/禁用与未知守卫/mqtt sink（mqttsim broker +
  pkg/mqtt 订阅者 QoS1 收帧 + payload 断言 + 台账 delivered 无 respCode）/
  sinkType 校验（mqtt 无端口/含路径/非法形态拒绝 + 合法 mqtt 创建）。
- e2e `TestV0590ForwardsTestE2E` 两段：US-1 创建 webhook 规则 → POST test
  同步 delivered:true → 接收端收到合成事件 → US-2 台账 test 条目 + 禁用后 400。

## N4 边界（已知限制，详见 KI §60）

- mqtt sink 短连接（每次投递独立 dial；连接池留需时立项）。
- mqtt broker 认证（用户名密码/TLS）未接入（登记 KI）。
- 测试投递同样计入台账环形 100（与真实事件共享配额）。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race -count=1 ./cloud/pkg/forwardstore/`：**6.117s** 绿（新增 4 例
  + 既有全过——sinkType 默认 webhook 兼容锚）。
- 契约：**134 条复验**（133→134 + test 端点探测）。
- 定向 e2e：`TestV0590ForwardsTestE2E`（见交付锚回填）。

## N6 复核

- R1/R2 已处置收口；**R3（v0.57+v0.58+v0.59 合并复核）**随本版推送后执行。
- 附：RN-v0580 N5 门禁数字补录（上一版文档批漏回填）：契约 **133 条 16.320s**
  绿 + 定向 e2e **run2 10.84s** PASS。
