# EdgeFlow v0.57.0 发布说明（MQTT 出站修复与共享订阅 granted-QoS1）

> 版本：v0.57.0 ｜ 日期：2026-10-22 ｜ spec：0030-mqtt-share-qos1 ｜ 缺口：持续项 B4 + R2 P3 处置
> 基线：v0.56.0（0330a78）。**本版为库语义修复与增强：零 API 变更（契约 130 维持、消息 19 不变）。**

## N1 能力

- **QoS2 phase2 修复（R2 P3-1）**：phase2 等待改专用循环——迟到重复 PUBREC
  （重发启用时 phase1 的多余确认帧，可能在 PUBREL 之后到达）跳过继续等（计入
  有界额度 attempts+8）；单轮超时按重发语义重发 PUBREL；phase2 前保留非阻塞
  drain 清已滞留帧。phase1 语义不变（异常帧显式失败）。
- **B4 共享订阅 granted-QoS1**：shareSub 增 granted（SUBACK 回显请求 QoS）；
  下行 v5 && granted≥1 && qos≥1 → QoS1 直发（PacketID broker 分配）；v3.1.1 与
  granted=0 维持 QoS0 best-effort 冻结锚；**共享订阅不落离线暂存队列**
  （spec 0005 US-5 边界保留）；轮转路由语义不变。
- **断言增强（R2 P3-3）**：v0550 US-4 补 PUBLISH 帧数断言（PUBLISH=1）。

## N2 兼容

- 端点 130 不变、消息 19 不变；契约四件套零改动。
- 默认禁用重发时 phase2 行为等价（单轮等待 + 迟到帧跳过为纯防御）。
- 共享订阅 SUBACK 授予行为不变（回显请求 QoS）——仅下行 QoS 升级。

## N3 测试（新增 4 例 + 1 断言增强）

- `pkg/mqtt/v0570_test.go` 1 例：跨相 drain——PUBREC 延迟 60ms 触发一次重发，
  迟到重复 PUBREC 被 phase2 循环跳过，PUBLISH=2 帧 + PUBREL=1 帧闭环。
- `pkg/mqttsim/v0570_test.go` 3 例：共享 QoS1 下行（PacketID/Puback 容忍）/
  双成员轮转恰好各得一份 QoS1 / v3.1.1 降级锚（QoS0 冻结语义）。
- 断言增强：v0550 US-4 PUBLISH=1 帧（R2 P3-3 处置）。
- e2e 轨迹：run1 暴露 drain 时机不足（串行 broker 下 PUBREC#2 恒在 PUBREL 后
  到达）→ phase2 改专用等待循环 → run2 全绿。

## N4 边界（已知限制，详见 KI §58）

- 共享订阅 QoS1 无离线暂存（成员离线即丢，spec 0005 US-5 边界保留）。
- phase2 迟到帧跳过计入有界额度（attempts+8）——病态 broker 有界退出。
- broker 下行重发仍不在范围（QoS1 直发为 at-least-once 语义减重发）。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race -count=1`：pkg/mqtt **6.554s** / pkg/mqttsim **21.381s** 全绿
  （新增 4 例 + US-4 增强 + 既有全过）。

## N6 复核

- R1/R2 均已处置收口；**R3（v0.57+v0.58 合并复核）**随 v0.58 执行。
