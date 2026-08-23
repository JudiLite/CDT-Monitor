#!/usr/bin/env bash
set -Eeuo pipefail

APP_NAME="cdt-monitor"
IMAGE="ghcr.io/judilite/cdt-monitor:latest"
INSTALL_DIR="/opt/cdt-monitor"
PORT="43210"
DATA_DIR=""
WORKERS="4"
TZ_VALUE="Asia/Shanghai"
INSTALL_DOCKER=1
CDT_BIN="/usr/local/bin/cdt"

usage() {
  cat <<'EOF'
CDT Monitor 一键部署脚本

用法：sudo bash install.sh [选项]
  --port PORT             Web 端口，默认 43210
  --data-dir DIR          数据目录，默认 /opt/cdt-monitor/data
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
    --dir) INSTALL_DIR="${2:?缺少安装目录}"; shift 2;;
    -h|--help) usage; exit 0;;
    *) echo "未知选项：$1" >&2; usage; exit 2;;
  esac
done

if [[ "${EUID}" -ne 0 ]]; then
  echo "请使用 root 运行：sudo bash install.sh" >&2
  exit 1
fi

if [[ -z "$DATA_DIR" ]]; then
  DATA_DIR="${INSTALL_DIR}/data"
fi

install_package() {
  local package="$1"
  if command -v apk >/dev/null 2>&1; then
    apk add --no-cache "$package"
  elif command -v apt-get >/dev/null 2>&1; then
    apt-get update && apt-get install -y "$package"
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y "$package"
  elif command -v yum >/dev/null 2>&1; then
    yum install -y "$package"
  else
    echo "请先手动安装 $package" >&2
    exit 1
  fi
}

command -v curl >/dev/null 2>&1 || { echo "正在安装 curl..."; install_package curl; }

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
chown -R 65532:65532 "$DATA_DIR" 2>/dev/null || true

cat > "$INSTALL_DIR/docker-compose.yml" <<EOF
services:
  ${APP_NAME}:
    image: ${IMAGE}
    container_name: ${APP_NAME}
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

cat > "$INSTALL_DIR/uninstall.sh" <<'UNINSTALL_EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

APP_NAME="cdt-monitor"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_DIR="$SCRIPT_DIR"
PURGE_DATA=0
YES=0
CDT_BIN="/usr/local/bin/cdt"

usage() {
  cat <<'EOF'
CDT Monitor 一键卸载脚本

用法：sudo bash uninstall.sh [选项]
  --dir DIR       安装目录，默认 /opt/cdt-monitor
  --purge-data    同时删除数据目录和安装目录
  -y, --yes       跳过确认
  -h, --help      显示帮助
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir) INSTALL_DIR="${2:?缺少安装目录}"; shift 2;;
    --purge-data) PURGE_DATA=1; shift;;
    -y|--yes) YES=1; shift;;
    -h|--help) usage; exit 0;;
    *) echo "未知选项：$1" >&2; usage; exit 2;;
  esac
done

if [[ "${EUID}" -ne 0 ]]; then
  echo "请使用 root 运行：sudo bash uninstall.sh" >&2
  exit 1
fi

COMPOSE_FILE="${INSTALL_DIR}/docker-compose.yml"
if docker compose version >/dev/null 2>&1; then
  COMPOSE=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE=(docker-compose)
else
  COMPOSE=()
fi

DATA_DIR=""
if [[ -f "$COMPOSE_FILE" ]]; then
  DATA_DIR="$(awk -F ':' '/:\/data/ {gsub(/^[[:space:]]*-[[:space:]]*"?/, "", $1); gsub(/"$/, "", $1); print $1; exit}' "$COMPOSE_FILE")"
fi

echo "即将卸载 CDT Monitor"
echo "安装目录: ${INSTALL_DIR}"
if [[ -n "$DATA_DIR" ]]; then
  echo "数据目录: ${DATA_DIR}"
fi
if [[ "$PURGE_DATA" -eq 1 ]]; then
  echo "模式: 删除容器、安装目录和数据"
else
  echo "模式: 删除容器和管理命令，保留数据目录"
fi

if [[ "$YES" -ne 1 ]]; then
  read -r -p "确认继续？[y/N] " answer
  case "$answer" in
    y|Y|yes|YES) ;;
    *) echo "已取消"; exit 0;;
  esac
fi

if [[ -f "$COMPOSE_FILE" && "${#COMPOSE[@]}" -gt 0 ]]; then
  (cd "$INSTALL_DIR" && "${COMPOSE[@]}" down --remove-orphans)
elif command -v docker >/dev/null 2>&1; then
  docker rm -f "$APP_NAME" >/dev/null 2>&1 || true
fi

if [[ -e "$CDT_BIN" ]]; then
  rm -f "$CDT_BIN"
fi

if [[ "$PURGE_DATA" -eq 1 ]]; then
  rm -rf "$INSTALL_DIR"
  if [[ -n "$DATA_DIR" && "$DATA_DIR" != "$INSTALL_DIR"* ]]; then
    rm -rf "$DATA_DIR"
  fi
else
  rm -f "$COMPOSE_FILE"
  rm -f "$INSTALL_DIR/uninstall.sh"
  rmdir "$INSTALL_DIR" 2>/dev/null || true
fi

echo "卸载完成。"
if [[ "$PURGE_DATA" -ne 1 && -n "$DATA_DIR" ]]; then
  echo "数据已保留: ${DATA_DIR}"
fi
UNINSTALL_EOF
chmod +x "$INSTALL_DIR/uninstall.sh"

cat > "$CDT_BIN" <<EOF
#!/usr/bin/env bash
set -Eeuo pipefail

APP_NAME="${APP_NAME}"
INSTALL_DIR="${INSTALL_DIR}"
COMPOSE_FILE="\${INSTALL_DIR}/docker-compose.yml"

compose_cmd() {
  if docker compose version >/dev/null 2>&1; then
    docker compose "\$@"
  elif command -v docker-compose >/dev/null 2>&1; then
    docker-compose "\$@"
  else
    echo "Docker Compose 不可用" >&2
    exit 1
  fi
}

need_install_dir() {
  if [[ ! -f "\$COMPOSE_FILE" ]]; then
    echo "未找到 CDT Monitor 安装目录：\$INSTALL_DIR" >&2
    exit 1
  fi
}

run_compose() {
  need_install_dir
  (cd "\$INSTALL_DIR" && compose_cmd "\$@")
}

open_panel() {
  need_install_dir
  local port
  port="\$(awk -F '"' '/:8080/ {print \$2; exit}' "\$COMPOSE_FILE" | awk -F ':' '{print \$1}')"
  [[ -n "\$port" ]] || port="43210"
  echo "访问地址: http://127.0.0.1:\${port}"
}

show_menu() {
  while true; do
    cat <<'MENU'

CDT Monitor 管理菜单
  1) 查看状态
  2) 查看日志
  3) 重启服务
  4) 停止服务
  5) 启动服务
  6) 更新镜像并重启
  7) 显示本机访问地址
  8) 卸载（保留数据）
  9) 卸载（删除数据）
  0) 退出
MENU
    read -r -p "请选择: " choice
    case "\$choice" in
      1) run_compose ps;;
      2) run_compose logs -f --tail=200;;
      3) run_compose restart;;
      4) run_compose stop;;
      5) run_compose up -d;;
      6) run_compose pull && run_compose up -d;;
      7) open_panel;;
      8) if command -v sudo >/dev/null 2>&1; then sudo "\$INSTALL_DIR/uninstall.sh"; else "\$INSTALL_DIR/uninstall.sh"; fi; exit 0;;
      9) if command -v sudo >/dev/null 2>&1; then sudo "\$INSTALL_DIR/uninstall.sh" --purge-data; else "\$INSTALL_DIR/uninstall.sh" --purge-data; fi; exit 0;;
      0) exit 0;;
      *) echo "无效选择";;
    esac
  done
}

case "\${1:-menu}" in
  menu) show_menu;;
  status|ps) run_compose ps;;
  logs) shift || true; run_compose logs -f --tail=200 "\$@";;
  restart) run_compose restart;;
  stop) run_compose stop;;
  start) run_compose up -d;;
  update) run_compose pull && run_compose up -d;;
  url) open_panel;;
  uninstall) shift || true; if command -v sudo >/dev/null 2>&1; then sudo "\$INSTALL_DIR/uninstall.sh" "\$@"; else "\$INSTALL_DIR/uninstall.sh" "\$@"; fi;;
  help|-h|--help)
    echo "用法: cdt [menu|status|logs|restart|stop|start|update|url|uninstall]"
    ;;
  *) echo "未知命令: \$1" >&2; exit 2;;
esac
EOF
chmod +x "$CDT_BIN"

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
echo "管理命令："
echo "  cdt                 打开管理菜单"
echo "  cdt status          查看状态"
echo "  cdt logs            查看日志"
echo "  cdt update          更新镜像并重启"
echo "  cdt uninstall       卸载服务"
