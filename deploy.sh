#!/usr/bin/env bash
# 一键部署 / 更新 vless-ws-server。
# 初次部署和后续更新都用这一个脚本，逻辑完全一样：
#   编译 -> 备份旧二进制 -> 部署新二进制 -> 装/更新 systemd 服务
#   -> daemon-reload -> enable --now / restart -> 校验服务确实启动成功
#
# 用法（在项目根目录，也就是 main.go 所在目录下执行）：
#   chmod +x deploy.sh
#   ./deploy.sh
#
# 需要 sudo 权限（部署到 /opt 和 /etc/systemd/system 都需要）。
# 如果拉依赖时连不上 proxy.golang.org，取消下面这行注释：
# export GOPROXY=direct GOSUMDB=off

set -euo pipefail

APP_NAME="vless-ws-server"
INSTALL_DIR="/opt/vless-ws-go"
SERVICE_SRC="./vless-ws-server.service"
SERVICE_DST="/etc/systemd/system/${APP_NAME}.service"

info()  { echo -e "\033[36m[deploy]\033[0m $*"; }
warn()  { echo -e "\033[33m[deploy]\033[0m $*"; }
error() { echo -e "\033[31m[deploy]\033[0m $*" >&2; }

if [ ! -f "./main.go" ] || [ ! -f "$SERVICE_SRC" ]; then
  error "请在项目根目录（含 main.go 和 vless-ws-server.service 的目录）下运行本脚本"
  exit 1
fi

# ── 1. 编译 ──────────────────────────────────────────────
info "拉取依赖 (go mod tidy) ..."
go mod tidy

info "编译 ${APP_NAME} ..."
go build -o "${APP_NAME}" .
info "编译完成：$(du -h "${APP_NAME}" | cut -f1)"

# ── 2. 部署二进制（先备份旧的，新的启动失败时方便回滚）──────
FIRST_DEPLOY=true
if [ -f "${INSTALL_DIR}/${APP_NAME}" ]; then
  FIRST_DEPLOY=false
fi

sudo mkdir -p "${INSTALL_DIR}"

if systemctl is-active --quiet "${APP_NAME}" 2>/dev/null; then
  info "停止正在运行的服务..."
  sudo systemctl stop "${APP_NAME}"
fi

if [ "$FIRST_DEPLOY" = false ]; then
  BACKUP="${INSTALL_DIR}/${APP_NAME}.bak"
  info "检测到已有部署，备份旧二进制到 ${BACKUP}"
  sudo cp "${INSTALL_DIR}/${APP_NAME}" "${BACKUP}"
fi

info "复制新二进制到 ${INSTALL_DIR}/"
sudo cp "${APP_NAME}" "${INSTALL_DIR}/"
sudo chmod +x "${INSTALL_DIR}/${APP_NAME}"

# ── 3. 安装/更新 systemd 服务文件 ────────────────────────
info "安装 systemd 服务文件"
sudo cp "$SERVICE_SRC" "$SERVICE_DST"
sudo systemctl daemon-reload

if [ "$FIRST_DEPLOY" = true ]; then
  info "首次部署：enable + 启动服务"
  sudo systemctl enable --now "${APP_NAME}"
else
  info "更新部署：重启服务"
  sudo systemctl restart "${APP_NAME}"
fi

# ── 4. 校验服务确实启动成功 ──────────────────────────────
# 给服务几秒钟时间起来，避免启动瞬间的状态误判为失败
sleep 2

if sudo systemctl is-active --quiet "${APP_NAME}"; then
  info "服务已成功启动/重启 ✅"
  sudo systemctl status "${APP_NAME}" --no-pager -l
else
  error "服务启动失败 ❌，最近日志如下："
  sudo journalctl -u "${APP_NAME}" -n 50 --no-pager
  if [ "$FIRST_DEPLOY" = false ]; then
    error "可以用备份的旧二进制手动回滚："
    error "  sudo cp ${INSTALL_DIR}/${APP_NAME}.bak ${INSTALL_DIR}/${APP_NAME} && sudo systemctl restart ${APP_NAME}"
  fi
  exit 1
fi

echo ""
info "部署完成。查看实时日志可以运行："
info "  sudo journalctl -u ${APP_NAME} -f"
