#!/usr/bin/env bash
# 编译 + 运行服务端，配置从 server.env 读，不用每次手动 export。
#
# 首次使用：
#   cp server.env.example server.env
#   vim server.env   # 填入你自己的 UUID（务必替换示例值）/ TOKEN 等
#   chmod +x run-server.sh
#   ./run-server.sh
#
# 生产环境长期运行建议用 deploy.sh（systemd 服务 + EnvironmentFile），
# 这个脚本主要用于本地调试/手动前台运行。
#
# 如果拉依赖时连不上 proxy.golang.org，取消下面这行注释：
# export GOPROXY=direct GOSUMDB=off

set -euo pipefail

ENV_FILE="server.env"
BIN_NAME="vless-ws-server"

info()  { echo -e "\033[36m[run-server]\033[0m $*"; }
error() { echo -e "\033[31m[run-server]\033[0m $*" >&2; }

if [ ! -f "./main.go" ]; then
  error "请在项目根目录（含 main.go 的目录）下运行本脚本"
  exit 1
fi

if [ ! -f "$ENV_FILE" ]; then
  error "找不到 ${ENV_FILE}，请先执行："
  error "  cp server.env.example ${ENV_FILE}"
  error "然后编辑 ${ENV_FILE}，务必换成你自己生成的 UUID（不要用示例里的占位值）"
  exit 1
fi

# shellcheck source=/dev/null
set -a
source "$ENV_FILE"
set +a

# 必填项检查，缺了直接报错退出，避免带着空 UUID 跑起来才发现服务端拒绝启动
missing=()
[ -z "${UUID:-}" ] && missing+=("UUID")
if [ ${#missing[@]} -gt 0 ]; then
  error "server.env 里缺少必填项：${missing[*]}"
  exit 1
fi

if [ "${UUID}" = "替换成你自己生成的随机UUID，例如用 uuidgen 生成" ]; then
  error "检测到 UUID 还是示例里的占位值，请先换成你自己生成的随机 UUID（例如执行 uuidgen）"
  exit 1
fi

info "拉取依赖 (go mod tidy) ..."
go mod tidy

info "编译 ${BIN_NAME} ..."
go build -o "${BIN_NAME}" .

info "启动服务端：${HOST:-0.0.0.0}:${PORT:-8081}${WS_PATH:-/api}"
echo ""

exec ./"${BIN_NAME}"
