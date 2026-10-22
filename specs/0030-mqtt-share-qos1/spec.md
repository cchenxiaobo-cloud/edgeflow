# spec 0030 — MQTT 出站修复与共享订阅 granted-QoS1（v0.57.0，持续项 B4）

> 版本：v0.57.0 ｜ 缺口：replan 持续项 B4「共享订阅 granted-QoS1」+ R2 P3 处置
> （P3-1 QoS2 跨相窗口 / P3-3 断言增强）｜ 上游：spec 0028（v0.55 出站重发）
> 说明：本版为库语义修复与增强：零 API 变更（契约 130 维持、消息 19 不变）。

## 背景与范围裁决

R2 复核（v0.54–v0.56）确认出站重发引入了一个窄窗口：QoS2 重发启用时 phase1 的
迟到重复 PUBREC 可能被 phase2 的 PUBCOMP 等待误取。同时持续项侦察确认 mqttsim
共享订阅下行恒为 QoS0 best-effort（v0.33.0 边界登记），而普通订阅 v5 已有
granted≥1 → QoS1 路径——B4 即为共享订阅补齐同一语义。

**裁决：QoS2 phase2 专用等待循环（跳过迟到 PUBREC + 超时重发 PUBREL）+ 共享
订阅 granted 入库与 QoS1 直发（v5 面），v3.1.1 与 granted=0 降级锚保持。**

## 裁决

1. **QoS2 phase2 修复（R2 P3-1）**：phase2 等待改专用循环——收到非 PUBCOMP 帧
   （迟到重复 PUBREC）跳过继续等（计入有界额度 attempts+8，防病态 broker 挂
   死）；单轮超时按重发语义重发 PUBREL；额度 resendAttempts（含首发）耗尽返回
   最后错误。phase2 前保留非阻塞 drain（清已滞留帧）。phase1 语义不变（迟到
   PUBCOMP 属 broker 异常，显式失败）。
2. **B4 共享订阅 granted-QoS1**：shareSub 增 granted 字段（SUBACK 口径回显请求
   QoS，既有行为）；下行分发 v5 && granted≥1 && qos≥1 → QoS1 直发（PacketID 由
   broker 分配）；v3.1.1 与 granted=0 维持 QoS0 best-effort（冻结语义）；
   **共享订阅不落离线暂存队列**（spec 0005 US-5 边界保留——恢复重放不覆盖共享
   面）；轮转路由语义不变。
3. **断言增强（R2 P3-3）**：v0550 US-4 补 PUBLISH 帧数断言（phase1 无超时即
   无重发，PUBLISH 恰 1 帧）。
4. **兼容**：零 API 变更（契约 130 维持）；默认禁用重发时 phase2 行为等价
   （单轮等待，迟到帧跳过为纯防御）。
5. **非目标**：共享订阅离线暂存（spec 0005 US-5 边界）、broker 下行重发、
   $queue 支持、共享订阅 NoLocal/RAP 扩展、SUBSCRIBE 出站重发。

## 端点

无新增（130 维持）。

## 交付锚

- 实际交付（2026-10-22）：单测 4 例（pkg/mqtt drain 1 + pkg/mqttsim 共享
  QoS1×3）+ US-4 断言增强（PUBLISH=1）；race 两包全绿（mqtt 6.554s /
  mqttsim 21.381s，含既有全过）；零 API 变更（契约 130 维持）。
- e2e 轨迹：run1 暴露 drain 时机不足（串行 broker 下 PUBREC#2 恒在 PUBREL 后
  到达）→ phase2 改专用等待循环（跳过迟到帧+超时重发 PUBREL+有界额度）→
  run2 全绿。
- R3（v0.57+v0.58 合并复核）随 v0.58 执行。
