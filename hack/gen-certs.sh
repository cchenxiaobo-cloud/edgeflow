#!/usr/bin/env bash
#
# EdgeFlow 云边通道 mTLS 证书一键生成脚本（WBS 7.1 证书管理，幂等）。
#
# 功能：在证书目录生成全套证书（与 pkg/certs 的布局与约定完全一致）：
#   ca.crt / ca.key            自签 CA（CN=edgeflow-ca，有效期 10 年，RSA 2048）
#   cloudcore.crt / cloudcore.key  cloudcore 服务端证书（CN=cloudcore，1 年，
#                             SAN: IP:127.0.0.1, DNS:localhost, DNS:cloudcore）
#   edgecore.crt / edgecore.key    edgecore 客户端证书（CN=edgeflow-edgecore，1 年，
#                             EKU: clientAuth）
#
# 幂等：目标文件已存在则跳过（不覆盖、不重新生成）。私钥权限 0600。
# 适用场景：手动部署、CI 预置证书（组件自身也会在首次启动时自动生成，
# 本脚本供需要在启动前预置证书的场景使用）。
#
# 用法：
#   ./hack/gen-certs.sh                    # 生成到默认目录 data/certs/
#   CERT_DIR=/path/to/certs ./hack/gen-certs.sh
#
# 依赖：openssl（macOS/Linux 自带）。
set -euo pipefail

CERT_DIR="${CERT_DIR:-data/certs}"
CA_DAYS=3650          # CA 有效期（天）= 10 年，与 pkg/certs 一致
LEAF_DAYS=365         # 叶子证书有效期（天）= 1 年，与 pkg/certs 一致
KEY_BITS=2048
CLIENT_CN="${CLIENT_CN:-edgeflow-edgecore}"   # edgecore 客户端证书 CN（可覆盖）

log() { printf '\033[1;32m[gen-certs]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[gen-certs]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[gen-certs]\033[0m %s\n' "$*" >&2; exit 1; }

# 统一清理临时文件（CSR/扩展文件），防止 EXIT trap 相互覆盖
# 注意：${TMP_FILES[@]} 在空数组 + set -u 下会报错，需先判长度
TMP_FILES=()
cleanup() { if ((${#TMP_FILES[@]} > 0)); then rm -f "${TMP_FILES[@]}"; fi; }
trap cleanup EXIT
tmpfile() { local f; f="$(mktemp)"; TMP_FILES+=("$f"); printf '%s' "$f"; }

command -v openssl >/dev/null 2>&1 || die "缺少依赖: openssl"

mkdir -p "$CERT_DIR"
CA_KEY="$CERT_DIR/ca.key"
CA_CRT="$CERT_DIR/ca.crt"
SERVER_KEY="$CERT_DIR/cloudcore.key"
SERVER_CRT="$CERT_DIR/cloudcore.crt"
CLIENT_KEY="$CERT_DIR/edgecore.key"
CLIENT_CRT="$CERT_DIR/edgecore.crt"
SERIAL_FILE="$CERT_DIR/ca.srl"

# 序列号文件：LibreSSL/openssl 的 -CAserial 要求文件已存在，先随机播种
# （16 位十六进制 = 64 位随机起点；后续每次签发自动递增，保证叶子证书序列号唯一）
if [[ ! -f "$SERIAL_FILE" ]]; then
  openssl rand -hex 8 > "$SERIAL_FILE"
  chmod 600 "$SERIAL_FILE"
fi

# ---------- 1. CA ----------
if [[ -s "$CA_CRT" && -s "$CA_KEY" ]]; then
  log "CA 已存在，跳过（$CA_CRT / $CA_KEY）"
else
  if [[ -e "$CA_CRT" || -e "$CA_KEY" ]]; then
    die "CA 证书/私钥不完整（只存在其一），请人工检查 $CERT_DIR"
  fi
  log "生成自签 CA（CN=edgeflow-ca，${CA_DAYS} 天，RSA ${KEY_BITS}）..."
  openssl req -x509 -newkey rsa:"$KEY_BITS" -nodes \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign,digitalSignature" \
    -keyout "$CA_KEY" -out "$CA_CRT" -days "$CA_DAYS" \
    -subj "/CN=edgeflow-ca"
  chmod 600 "$CA_KEY"
fi

# ---------- 2. cloudcore 服务端证书 ----------
if [[ -s "$SERVER_CRT" && -s "$SERVER_KEY" ]]; then
  log "cloudcore 服务端证书已存在，跳过（$SERVER_CRT / $SERVER_KEY）"
else
  if [[ -e "$SERVER_CRT" || -e "$SERVER_KEY" ]]; then
    die "cloudcore 证书/私钥不完整（只存在其一），请人工检查 $CERT_DIR"
  fi
  if [ -n "${TLS_SAN:-}" ]; then
    SAN_LIST="$TLS_SAN"
    log "签发 cloudcore 服务端证书（CN=cloudcore，${LEAF_DAYS} 天，SAN 自定义: $TLS_SAN）..."
  else
    SAN_LIST="IP:127.0.0.1,DNS:localhost,DNS:cloudcore"
    log "签发 cloudcore 服务端证书（CN=cloudcore，${LEAF_DAYS} 天，SAN 默认 127.0.0.1/localhost/cloudcore）..."
  fi
  SERVER_CSR="$(tmpfile)"
  SERVER_EXT="$(tmpfile)"
  openssl req -newkey rsa:"$KEY_BITS" -nodes \
    -keyout "$SERVER_KEY" -out "$SERVER_CSR" \
    -subj "/CN=cloudcore"
  cat > "$SERVER_EXT" <<EOF
subjectAltName=$SAN_LIST
extendedKeyUsage=serverAuth
EOF
  openssl x509 -req -in "$SERVER_CSR" \
    -CA "$CA_CRT" -CAkey "$CA_KEY" -CAserial "$SERIAL_FILE" \
    -out "$SERVER_CRT" -days "$LEAF_DAYS" \
    -extfile "$SERVER_EXT"
  chmod 600 "$SERVER_KEY"
fi

# ---------- 3. edgecore 客户端证书 ----------
if [[ -s "$CLIENT_CRT" && -s "$CLIENT_KEY" ]]; then
  log "edgecore 客户端证书已存在，跳过（$CLIENT_CRT / $CLIENT_KEY）"
else
  if [[ -e "$CLIENT_CRT" || -e "$CLIENT_KEY" ]]; then
    die "edgecore 证书/私钥不完整（只存在其一），请人工检查 $CERT_DIR"
  fi
  log "签发 edgecore 客户端证书（CN=${CLIENT_CN}，${LEAF_DAYS} 天，EKU clientAuth）..."
  CLIENT_CSR="$(tmpfile)"
  CLIENT_EXT="$(tmpfile)"
  openssl req -newkey rsa:"$KEY_BITS" -nodes \
    -keyout "$CLIENT_KEY" -out "$CLIENT_CSR" \
    -subj "/CN=${CLIENT_CN}"
  cat > "$CLIENT_EXT" <<EOF
extendedKeyUsage=clientAuth
EOF
  openssl x509 -req -in "$CLIENT_CSR" \
    -CA "$CA_CRT" -CAkey "$CA_KEY" -CAserial "$SERIAL_FILE" \
    -out "$CLIENT_CRT" -days "$LEAF_DAYS" \
    -extfile "$CLIENT_EXT"
  chmod 600 "$CLIENT_KEY"
fi

log "证书就绪：$CERT_DIR"
ls -l "$CERT_DIR"

# ---------- 4. 跨主机分发包（WBS 7.3/B8 跨主机 CA 分发自动化）----------
# 设置 CERT_DIST_DIR 后生成可分发目录（不打包，便于 inspect；scp -r 或 tar 即可搬运）：
#   <CERT_DIST_DIR>/cloud/    → cloudcore 主机：ca.crt + cloudcore.crt + cloudcore.key + README
#   <CERT_DIST_DIR>/edge/<CN>/ → edgecore 主机：ca.crt + edgecore.crt + edgecore.key + README
# 每生成一个边缘分发包，指定 CLIENT_CN（默认 edgeflow-edgecore）与独立 CERT_DIR/CERT_DIST_DIR。
# 安全提示：分发包含私钥，传输必须走加密通道（scp/rsync 或先 tar 再加密）；
# 部署后私钥权限须为 0600（edgecore 启动要求）。
if [ -n "${CERT_DIST_DIR:-}" ]; then
  mkdir -p "$CERT_DIST_DIR/cloud" "$CERT_DIST_DIR/edge/$CLIENT_CN"
  cp "$CA_CRT" "$CERT_DIST_DIR/cloud/"
  cp "$SERVER_CRT" "$SERVER_KEY" "$CERT_DIST_DIR/cloud/"
  cp "$CA_CRT" "$CERT_DIST_DIR/edge/$CLIENT_CN/"
  cp "$CLIENT_CRT" "$CLIENT_KEY" "$CERT_DIST_DIR/edge/$CLIENT_CN/"
  chmod 600 "$CERT_DIST_DIR/cloud/"*.key "$CERT_DIST_DIR/edge/$CLIENT_CN/"*.key
  cat > "$CERT_DIST_DIR/cloud/README.txt" <<EOF
EdgeFlow 云端证书分发包（由 hack/gen-certs.sh 生成）

部署目标：cloudcore 主机
部署路径（与 keadm join 部署约定一致）：
  /etc/edgeflow/certs/ca.crt
  /etc/edgeflow/certs/cloudcore.crt
  /etc/edgeflow/certs/cloudcore.key   (chmod 600)

cloudcore 启用 mTLS 与证书目录（env）：
  EDGEFLOW_CLOUDCORE_TLS=on
  EDGEFLOW_CLOUDCORE_CERT_DIR=/etc/edgeflow/certs

轮换：重新执行 gen-certs.sh 后重新分发；私钥泄露时全量重签（删除 CERT_DIR 后重跑）。
EOF
  cat > "$CERT_DIST_DIR/edge/$CLIENT_CN/README.txt" <<EOF
EdgeFlow 边缘节点证书分发包（节点 CN=$CLIENT_CN，由 hack/gen-certs.sh 生成）

部署目标：edgecore 主机（本包只含本节点证书）
部署路径（与 keadm join 部署约定一致）：
  /etc/edgeflow/certs/ca.crt
  /etc/edgeflow/certs/edgecore.crt
  /etc/edgeflow/certs/edgecore.key   (chmod 600)

edgecore 启用 mTLS 与证书目录（env）：
  EDGEFLOW_EDGECORE_TLS=on
  EDGEFLOW_EDGECORE_CERT_DIR=/etc/edgeflow/certs

分发方式：scp -r "$CERT_DIST_DIR/edge/$CLIENT_CN" user@edge-host:/etc/edgeflow/
轮换：同云端分发包；多节点各自生成（CLIENT_CN 不同）后分发。
EOF
  log "分发包已生成：$CERT_DIST_DIR（cloud/ 与 edge/$CLIENT_CN/，含 README 部署说明）"
fi
