# EdgeFlow v0.1.0 发布核对清单（RELEASE CHECKLIST）

> 用途：M5 发布制品的独立复核清单。每条均给出**如何验证**，复核人可逐条执行确认。
> 适用版本：v0.1.0（不可变 tag，发布后禁止改写）。
> 环境说明：本机无远程制品/镜像仓库凭据，M5 在本地 registry 完成闭环；
> 远程仓库推送步骤见文末「远程发布步骤（待凭据）」，届时按此执行并回填台账。

## 一、制品完整性（release/v0.1.0/）

- [ ] **6 个二进制齐全**：cloudcore/edgecore/keadm × darwin-arm64 + linux-amd64
  - 如何验证：`ls -la release/v0.1.0/`，应有
    `cloudcore-darwin-arm64 / cloudcore-linux-amd64 / edgecore-darwin-arm64 / edgecore-linux-amd64 / keadm-darwin-arm64 / keadm-linux-amd64`
- [ ] **Helm Chart 包齐全**：`edgeflow-0.1.0.tgz`
  - 如何验证：`ls release/v0.1.0/edgeflow-0.1.0.tgz`
- [ ] **版本注入正确**（每个二进制 --version 输出 version=v0.1.0）
  - 如何验证：
    - darwin 本机：`release/v0.1.0/cloudcore-darwin-arm64 --version`（edgecore 同；keadm 用 `keadm-darwin-arm64 version`）
    - linux 交叉产物（需 qemu/容器）：`docker run --rm --platform linux/amd64 -v $PWD/release/v0.1.0:/r:ro alpine:3.20 /r/cloudcore-linux-amd64 --version`（edgecore/keadm 同）
    - 期望形如：`version=v0.1.0 gitCommit=<sha> buildTime=<ts> goVersion=go1.x`
- [ ] **Chart 包内容与源码一致**
  - 如何验证：`helm show chart release/v0.1.0/edgeflow-0.1.0.tgz`，version=0.1.0、appVersion=v0.1.0；
    `tar -xzf edgeflow-0.1.0.tgz -O edgeflow/values.yaml | diff - build/charts/edgeflow/values.yaml` 无差异

## 二、校验和（checksums.txt）

- [ ] **checksums.txt 覆盖全部 7 个制品**（6 二进制 + 1 Chart 包）
  - 如何验证：`wc -l release/v0.1.0/checksums.txt` 应为 7
- [ ] **校验和可验证通过**
  - 如何验证：`cd release/v0.1.0 && shasum -a 256 -c checksums.txt`（或 sha256sum -c），全部输出 `OK`

## 三、SBOM（sbom.json）

- [ ] **JSON 合法且字段完整**：组件清单（go list -m all 依赖）+ 制品 sha256 + Go 版本 + 构建参数
  - 如何验证：`python3 -m json.tool release/v0.1.0/sbom.json > /dev/null`；
    `jq '.components | length'` ≥ 33（依赖数随 go.mod 变化）；`jq '.artifacts[].sha256'` 与 checksums.txt 对应条目一致
- [ ] **依赖清单与 go.mod 一致**
  - 如何验证：`go list -m all` 与 sbom.json components 逐条比对（版本号一致）

## 四、镜像与 digest（本地 registry 闭环）

- [ ] **镜像不可变 tag 推送**：localhost:5001/edgeflow/{cloudcore,edgecore}:v0.1.0
  - 如何验证（需先起 registry：`docker run -d -p 127.0.0.1:5001:5000 --name release-reg registry:2`）：
    `docker pull localhost:5001/edgeflow/cloudcore:v0.1.0` 输出的 Digest 与 images.json 一致
- [ ] **digest 真实记录**：images.json 含 tag/digest/size/arch
  - 如何验证：`curl -sI -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" http://127.0.0.1:5001/v2/edgeflow/cloudcore/manifests/v0.1.0 | grep -i Docker-Content-Digest`，与 images.json 中 digest 一致
- [ ] **digest 指向内容可运行且版本正确**
  - 如何验证：`docker run --rm localhost:5001/edgeflow/cloudcore@sha256:<digest> --version` 输出 version=v0.1.0（edgecore 同）

## 五、回滚方案

- [ ] **镜像可按 digest 回滚**（不可变引用，不依赖 tag 覆写）
  - 如何验证：回滚时用 images.json 中 `reference`（`name@digest`）拉取即可复现发布时内容；Chart 回滚用 `edgeflow-0.1.0.tgz` 重新 `helm upgrade --version` 或 `helm rollback`
- [ ] **二进制可回滚**：release/v0.1.0/ 归档 + checksums.txt 保证产物可校验复现
  - 如何验证：任取一文件 `shasum -a 256` 与 checksums.txt 比对

## 六、台账

- [ ] **台账记录本清单核验结果**（docs/RELEASE-LEDGER.md）
  - 如何验证：台账含 v0.1.0 制品清单、digest、核验人/日期；本清单逐项勾选后回填

---

## 远程发布步骤（待凭据，M5 未执行）

本地闭环已通过；获得远程仓库（如 Docker Hub / Harbor）与制品仓库（如 GitHub Releases）凭据后：

1. 镜像：`docker tag localhost:5001/edgeflow/cloudcore:v0.1.0 <remote>/edgeflow/cloudcore:v0.1.0 && docker push <remote>/edgeflow/cloudcore:v0.1.0`（edgecore 同；勿变 tag，digest 以 push 后返回值为准，回填 images.json）
2. 二进制 + Chart 包 + checksums.txt + sbom.json：上传至制品仓库 release/v0.1.0/ 目录（GitHub Releases 等）
3. 推送后复核：远程 digest 与本地一致（远程仓库 API 查询）；checksums.txt 独立验签
4. 回填台账：远程 digest、制品 URL、发布时间
