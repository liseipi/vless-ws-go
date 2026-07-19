# VLESS over WebSocket — Go 版本

基于 Go + WebSocket 的 VLESS 协议转发服务器，协议行为与标准 VLESS 规范保持一致。

## 目录结构

```
vless-ws-go/
├── go.mod / go.sum
├── main.go       # HTTP 服务器、鉴权、优雅关闭
├── config.go     # 环境变量配置
├── log.go        # 分级日志 + 失败限频
├── vless.go      # VLESS 协议头解析
├── nat64.go      # NAT64 解析（系统 DNS → DoH → TTL 缓存）
└── session.go    # 核心会话：握手 + WS↔TCP 双向转发
```

## 安装

```bash
sudo apt update
sudo snap install go --classic         # 安装最新稳定版
sudo snap install go --channel=1.22/stable --classic  # 安装某个特定的版本
```

## 构建

```bash
go mod tidy          # 首次构建拉取依赖 github.com/coder/websocket
go build -o vless-ws-server .
```

## 运行

```bash
PORT=8080 \
WS_PATH=/api \
UUID=a3d2e1f0-b4c5-4d6e-8f70-1a2b3c4d5e6f \
TOKEN= \
IDLE_TIMEOUT_MS=120000 \
HEARTBEAT_MS=25000 \
CONNECT_TIMEOUT_MS=12000 \
CONNECT_RETRIES=1 \
RETRY_BASE_MS=200 \
MAX_FRAME_BYTES=2097152 \
LOG_LEVEL=info \
./vless-ws-server

# 生成 UUID
uuidgen
# 生成 TOKEN
openssl rand -hex 24
```

所有环境变量均可按需覆盖，不设额外必填项。新增两项：

- `CONNECT_RETRIES`（默认 `1`）/ `RETRY_BASE_MS`（默认 `200`）：连接目标网站失败时
  （超时、连接被重置等一次性网络抖动）按指数退避自动重试，`RETRY_BASE_MS` 是重试
  等待时间的基准，第 n 次重试大约等 `base × 2^(n-1)` 毫秒。设成 `0` 关闭重试。
- `MAX_FRAME_BYTES`（默认 `2097152`，即 2MB）：单个 WebSocket 帧允许的最大字节数，
  防止恶意构造的超大单帧占用过多内存（防御性配置，不影响正常转发，正常流量的
  单帧远小于这个值）。设成 `0` 或负数表示不限制。

健康检查：`GET /health` → `{"ok":true,"ts":...}`

## 已验证

- VLESS 握手 + IPv4 目标地址转发（本地 echo server 往返测试通过）
- 错误 UUID 被正确拒绝并关闭连接
- 上游连接失败时的指数退避重试逻辑（故意连一个必然拒绝的端口验证）
- 编译通过 `go vet` 静态检查

## systemd 开机自启动

项目自带 `vless-ws-server.service` 单元文件，所有参数从 `config.go` 读取默认值。

```bash
在你的项目目录下（/home/vless-ws-go）执行：

# 1. 编译
go mod tidy
go build -o vless-ws-server .

# 2. 部署到 service 文件指定的路径
sudo mkdir -p /opt/vless-ws-go
sudo cp vless-ws-server /opt/vless-ws-go/
sudo chmod +x /opt/vless-ws-go/vless-ws-server

# 3. 安装并启动服务
sudo cp vless-ws-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now vless-ws-server

# 4. 查看状态
sudo systemctl status vless-ws-server
sudo journalctl -u vless-ws-server -f
# 重启
sudo systemctl restart vless-ws-server
```

如需覆盖默认参数，编辑 `/etc/systemd/system/vless-ws-server.service`，取消注释对应 `Environment=` 行即可。

## 部署建议

- **强烈建议在 Go 服务前面再加一层 Nginx** 做 TLS 终止 + WS 反代 + 伪装站点，
  Go 程序本身只需监听内网端口（`HOST=127.0.0.1`）。
- 单机连接数较大时，注意调整 `ulimit -n`（文件描述符上限），Go 侧不需要额外调整，
  瓶颈通常先出现在系统层面。
