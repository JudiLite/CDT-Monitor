#!/usr/bin/env bash
set -Eeuo pipefail

APP_NAME="cdt-monitor"
INSTALL_DIR="/opt/cdt-monitor"
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
