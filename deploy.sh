#!/usr/bin/env bash
# 一键部署 / 更新 vless-ws-server。
# 初次部署和后续更新都用这一个脚本，逻辑完全一样：
#   检查/安装 Go -> 编译 -> 备份旧二进制 -> 部署新二进制 -> 装/更新 systemd 服务
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
ENV_FILE="./server.env"

info()  { echo -e "\033[36m[deploy]\033[0m $*"; }
warn()  { echo -e "\033[33m[deploy]\033[0m $*"; }
error() { echo -e "\033[31m[deploy]\033[0m $*" >&2; }

if [ ! -f "./main.go" ] || [ ! -f "$SERVICE_SRC" ]; then
  error "请在项目根目录（含 main.go 和 vless-ws-server.service 的目录）下运行本脚本"
  exit 1
fi

# ── 0. 配置文件校验 ──────────────────────────────────────
# UUID 不再有硬编码默认值，缺少 server.env 或者 UUID 没填的话服务端启动会
# 直接失败，这里提前拦下来，避免部署到一半才发现。
if [ ! -f "$ENV_FILE" ]; then
  error "找不到 ${ENV_FILE}，请先执行："
  error "  cp server.env.example ${ENV_FILE}"
  error "然后编辑 ${ENV_FILE}，务必换成你自己生成的 UUID（不要用示例里的占位值）"
  exit 1
fi

ENV_UUID="$(grep -E '^UUID=' "$ENV_FILE" | tail -n1 | cut -d= -f2- | tr -d '"')"
if [ -z "$ENV_UUID" ] || [ "$ENV_UUID" = "替换成你自己生成的随机UUID，例如用 uuidgen 生成" ]; then
  error "${ENV_FILE} 里的 UUID 还没有设置成你自己的值（或者还是示例占位值），请先编辑好再部署"
  exit 1
fi

# ── 1. 检查 Go 环境（缺失时用 snap 安装 1.22）─────────────
GO_SNAP_CHANNEL="1.22/stable"
GO_REQUIRED="$(awk '/^go[[:space:]]/{print $2; exit}' go.mod 2>/dev/null || true)"

# 版本比较：version_ge A B 表示 A >= B
version_ge() {
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

if command -v go >/dev/null 2>&1; then
  info "检测到已安装 Go：$(go version)"
else
  warn "未检测到 Go，尝试通过 snap 安装（channel=${GO_SNAP_CHANNEL}）..."

  if ! command -v snap >/dev/null 2>&1; then
    error "系统未安装 snap，无法自动安装 Go。"
    error "请先安装 snapd 后重试，或手动安装 Go >= ${GO_REQUIRED:-1.22}：https://go.dev/dl/"
    exit 1
  fi

  sudo snap install go --channel="${GO_SNAP_CHANNEL}" --classic
  hash -r 2>/dev/null || true

  # snap 安装的 go 位于 /snap/bin，当前 shell 的 PATH 可能还没生效，这里补一下
  if ! command -v go >/dev/null 2>&1 && [ -x /snap/bin/go ]; then
    export PATH="/snap/bin:${PATH}"
    hash -r 2>/dev/null || true
  fi

  if ! command -v go >/dev/null 2>&1; then
    error "Go 安装后仍未找到 go 命令，请重新登录 shell（或手动把 /snap/bin 加入 PATH）后再运行本脚本"
    exit 1
  fi
  info "Go 安装完成：$(go version)"
fi

# go.mod 要求的最低版本校验，版本过低时给出升级提示（不强制中断）
if [ -n "${GO_REQUIRED}" ]; then
  GO_CURRENT="$(go version | awk '{print $3}' | sed 's/^go//')"
  if ! version_ge "${GO_CURRENT}" "${GO_REQUIRED}"; then
    warn "当前 Go 版本 ${GO_CURRENT} 低于 go.mod 要求的 ${GO_REQUIRED}，编译可能失败。"
    warn "可通过 snap 升级到 1.22："
    warn "  sudo snap install go --channel=${GO_SNAP_CHANNEL} --classic   # 未安装时"
    warn "  sudo snap refresh go --channel=${GO_SNAP_CHANNEL} --classic   # 已安装时"
  fi
fi

# ── 2. 编译 ──────────────────────────────────────────────
info "拉取依赖 (go mod tidy) ..."
go mod tidy

info "编译 ${APP_NAME} ..."
go build -o "${APP_NAME}" .
info "编译完成：$(du -h "${APP_NAME}" | cut -f1)"

# ── 3. 部署二进制（先备份旧的，新的启动失败时方便回滚）──────
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

info "同步 ${ENV_FILE} 到 ${INSTALL_DIR}/server.env（systemd EnvironmentFile 会从这里读取 UUID/TOKEN 等）"
sudo cp "${ENV_FILE}" "${INSTALL_DIR}/server.env"
sudo chmod 600 "${INSTALL_DIR}/server.env" # 含密钥，收紧权限，只有 root 可读

# ── 4. 安装/更新 systemd 服务文件 ────────────────────────
# 单元文件里的 WorkingDirectory / ExecStart / EnvironmentFile 统一按 ${INSTALL_DIR}
# 重写，保证和本脚本实际部署的目录一致。否则一旦两边路径不一致，systemd 会因为
# 找不到可执行文件/环境文件而启动失败，此时状态会显示 "Result: resources"、
# CPU 恒为 0，日志里也看不到任何程序本身的输出（因为进程根本没被拉起来）。
info "安装 systemd 服务文件（工作目录 ${INSTALL_DIR}）"
sed -e "s|^WorkingDirectory=.*|WorkingDirectory=${INSTALL_DIR}|" \
    -e "s|^ExecStart=.*|ExecStart=${INSTALL_DIR}/${APP_NAME}|" \
    -e "s|^EnvironmentFile=.*|EnvironmentFile=${INSTALL_DIR}/server.env|" \
    "$SERVICE_SRC" | sudo tee "$SERVICE_DST" >/dev/null
sudo systemctl daemon-reload

# 上一个版本如果一直启动失败，可能已经触发 systemd 的启动频率限制
# （"Start request repeated too quickly"），不清掉的话 restart 会直接被拒绝。
sudo systemctl reset-failed "${APP_NAME}" 2>/dev/null || true

if [ "$FIRST_DEPLOY" = true ]; then
  info "首次部署：enable + 启动服务"
  sudo systemctl enable --now "${APP_NAME}"
else
  info "更新部署：重启服务"
  sudo systemctl restart "${APP_NAME}"
fi

# ── 5. 校验服务确实启动成功 ──────────────────────────────
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
