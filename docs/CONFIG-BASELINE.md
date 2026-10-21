# 分级配置基线（轻 / 标 / 重）

> v0.52.0（spec 0025 裁决 6，G33）：DR-ASSESSMENT 差距 #3「治理面快照依赖显式
> env——漏配则 RPO=全量丢失」承接：把持久化/安全/备份配置按部署档位固化。
> 三档面向真实部署形态；漏配任何「强制」项视为部署不合规（上线检查单口径）。

## 档位定义

| 档位 | 形态 | 节点量级 | 可用性目标 |
|---|---|---|---|
| 轻（single-demo） | 单机单进程，内存态可接受 | ≤5 | 演示/PoC |
| 标（site-standard） | 单机 + 全量持久化 + 认证 | 5–50 | 站点级 2 级（GB/T 20988） |
| 重（region-scale） | 外部 etcd 3 节点集群 + K8s 自愈 + DR 备份目录 + 异地副本 | 50–200 | 区域级 3 级起步 |

## 强制项（标准档及以上）

| 项 | env | 说明 |
|---|---|---|
| 治理面持久化 | EDGEFLOW_CLOUDCORE_REGION_FILE / _TAG_FILE / _LOGSTORE_FILE | 三者全配（差距 #3 强制项；漏配=重启即失） |
| 备份目录 | EDGEFLOW_CLOUDCORE_DR_DIR（+ DR_KEEP 可选，默认 7） | 落本地独立路径并纳入异地副本 |
| API 认证 | EDGEFLOW_CLOUDCORE_AUTH=on + EDGEFLOW_CLOUDCORE_API_TOKEN | 生产禁裸奔（启动横幅同口径） |
| 云边节点令牌 | EDGEFLOW_CLOUDCORE_NODE_TOKEN（边端同值） | 注册面/下发面防冒充 |

## 重档追加（区域/规模）

| 项 | 配置 | 说明 |
|---|---|---|
| 外部 etcd 集群 | EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS（3 节点）+ TLS/账号 | 差距 #2 承接：数据面去单点（v0.5+ 装配分叉已支持） |
| K8s 自愈 | liveness=/healthz + PV 固定数据目录 | 差距 #1 前两级（RTO 分钟级内） |
| 资源基线 | cloudcore 2C/4G 起；etcd 独立盘 | 100 节点压测前基线（待实测回填） |

## 演示档（轻）默认豁免说明

轻档允许全部持久化 env 缺省（纯内存运行）——启动横幅会列出风险提示；
仅限隔离/演示环境，不得承载唯一副本的生产数据。

## 待环境回填

- 麒麟/统信 OS 与 C86（海光）构建验证记录：待硬件环境（replan v0.52 原范围，
  如实登记未测，不以仿真结论替代）。
- 100 节点压测与 30min 自治长跑：scripts/load-test + hack/collect-bench 机制
  已就绪（`make bench-baseline` 可重复刷新），结果待环境回填本文档。
