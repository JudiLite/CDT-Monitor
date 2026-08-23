#!/usr/bin/env bash
set -Eeuo pipefail

APP_NAME="cdt-monitor"
INSTALL_DIR="/opt/cdt-monitor"
PORT="43210"
DATA_DIR="${INSTALL_DIR}/data"
WORKERS="4"
TZ_VALUE="Asia/Shanghai"
INSTALL_DOCKER=1

usage() {
  cat <<'EOF'
CDT Monitor 一键部署脚本

用法：sudo bash install.sh [选项]
  --port PORT             Web 端口，默认 43210
  --data-dir DIR         数据目录，默认 /opt/cdt-monitor/data
  --workers N             Worker 数量，默认 4
  --timezone TZ           时区，默认 Asia/Shanghai
  --no-install-docker     未检测到 Docker 时不自动安装
  --dir DIR               安装目录，默认 /opt/cdt-monitor
  -h, --help              显示帮助
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --port) PORT="${2:?缺少端口}"; shift 2;;
    --data-dir) DATA_DIR="${2:?缺少数据目录}"; shift 2;;
    --workers) WORKERS="${2:?缺少 Worker 数量}"; shift 2;;
    --timezone) TZ_VALUE="${2:?缺少时区}"; shift 2;;
    --no-install-docker) INSTALL_DOCKER=0; shift;;
    --dir) INSTALL_DIR="${2:?缺少安装目录}"; DATA_DIR="${INSTALL_DIR}/data"; shift 2;;
    -h|--help) usage; exit 0;;
    *) echo "未知选项：$1" >&2; usage; exit 2;;
  esac
done

if [[ "${EUID}" -ne 0 ]]; then
  echo "请使用 root 运行：sudo bash install.sh" >&2
  exit 1
fi

command -v curl >/dev/null 2>&1 || { echo "正在安装 curl..."; if command -v apk >/dev/null; then apk add --no-cache curl; elif command -v apt-get >/dev/null; then apt-get update && apt-get install -y curl; elif command -v dnf >/dev/null; then dnf install -y curl; elif command -v yum >/dev/null; then yum install -y curl; else echo "请先手动安装 curl" >&2; exit 1; fi; }

if ! command -v docker >/dev/null 2>&1; then
  [[ "$INSTALL_DOCKER" -eq 1 ]] || { echo "未找到 Docker，已按参数停止安装。" >&2; exit 1; }
  echo "未检测到 Docker，开始安装..."
  curl -fsSL https://get.docker.com | sh
fi

if ! docker compose version >/dev/null 2>&1; then
  if command -v docker-compose >/dev/null 2>&1; then
    COMPOSE=(docker-compose)
  else
    echo "Docker Compose 插件不可用，请安装 docker compose plugin。" >&2
    exit 1
  fi
else
  COMPOSE=(docker compose)
fi

mkdir -p "$INSTALL_DIR" "$DATA_DIR"
# 容器使用 UID 65532 运行；绑定目录需要可写。
chown -R 65532:65532 "$DATA_DIR" 2>/dev/null || true
cat > "$INSTALL_DIR/docker-compose.yml" <<EOF
services:
  cdt-monitor:
    image: ghcr.io/wang4386/cdt-monitor:latest
    container_name: cdt-monitor
    restart: unless-stopped
    init: true
    ports:
      - "${PORT}:8080"
    environment:
      CDT_DATA_DIR: /data
      CDT_LISTEN: :8080
      CDT_WORKERS: "${WORKERS}"
      TZ: "${TZ_VALUE}"
    volumes:
      - "${DATA_DIR}:/data"
    healthcheck:
      test: ["CMD", "/cdt-monitor", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
EOF

cd "$INSTALL_DIR"
echo "拉取镜像并启动 CDT Monitor..."
"${COMPOSE[@]}" pull
"${COMPOSE[@]}" up -d

IP="$(curl -4fsS --max-time 3 https://api.ipify.org 2>/dev/null || true)"
[[ -n "$IP" ]] || IP="服务器IP"
echo
echo "部署完成！"
echo "访问地址: http://${IP}:${PORT}"
echo "安装目录: ${INSTALL_DIR}"
echo "数据目录: ${DATA_DIR}"
echo
echo "常用命令："
echo "  cd ${INSTALL_DIR} && ${COMPOSE[*]} logs -f"
echo "  cd ${INSTALL_DIR} && ${COMPOSE[*]} restart"
echo "  cd ${INSTALL_DIR} && ${COMPOSE[*]} down"
