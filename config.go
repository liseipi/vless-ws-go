package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
)

// Config 保存全部运行时配置，全部可通过环境变量覆盖。
type Config struct {
	Host              string
	Port              string
	WSPath            string
	UUID              string
	Token             string
	IdleTimeoutMS     int64
	HeartbeatMS       int64
	ConnectTimeoutMS  int64
	ConnectRetries    int   // 连接上游目标失败时的额外重试次数（不含首次尝试），指数退避
	RetryBaseMS       int64 // 重试退避基准时间（毫秒）
	MaxFrameBytes     int64 // 单个 WebSocket 帧允许的最大字节数，防止恶意超大单帧占用内存；<=0 表示不限制
	LogLevel          string
	MaxHeaderBufBytes int
	YamuxWindowBytes  uint32 // 单个 yamux stream 的接收窗口大小，见 session.go 里的说明

	// yamux keepalive ping 等待确认的最长时间，超时会判定整条底层连接已死，
	// 直接关闭该连接上复用的所有 stream（不是只关一个）。链路延迟较高，或者
	// MaxStreamWindowSize 调得比较大、经常有突发大流量占满带宽时，适当调大
	// 这个值可以降低"一次网络抖动打断所有并发请求"的概率，见 session.go
	// 里 HandleSession 的说明。
	YamuxWriteTimeoutMS int64

	// NAT64 地址转换使用的 IPv6 前缀。当上游目标只有 IPv4 地址、而本机对
	// 目标网络只有 IPv6 直连能力时，需要通过真实的 NAT64 网关把 IPv4 地址
	// 映射成 IPv6 地址再拨号。RFC 6052 定义的"Well-Known Prefix"是
	// 64:ff9b::/96，绝大多数运营商/公有云提供的 NAT64 网关都用这个前缀；
	// 如果你的网络环境用的是自定义前缀，通过 NAT64_PREFIX 覆盖。
	// 注意：这只在直连失败、且本机确实处于 NAT64 环境时才有意义；普通双栈
	// 或纯 IPv4 服务器不会走到这条兜底路径，改这个值不影响正常连接。
	NAT64Prefix string

	// GOMAXPROCS 默认等于 CPU 核心数，Go runtime 自动利用多核。
	NumCPU int
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func LoadConfig() *Config {
	cfg := &Config{
		Host: envStr("HOST", "0.0.0.0"),
		Port: envStr("PORT", "8081"),

		WSPath: envStr("WS_PATH", "/api"),

		// 【安全修复】UUID / TOKEN 不再提供硬编码默认值——之前默认值是明文
		// 写在公开 GitHub 仓库源码里的，部署时如果忘记设置对应环境变量，
		// 服务端会悄悄用这组"任何人在 GitHub 上都能搜到"的凭据对外提供
		// 服务，等于一个开放代理。现在留空表示"未配置"，下面会强制校验：
		// UUID 必须显式设置，否则拒绝启动。
		UUID:  envStr("UUID", ""),
		Token: envStr("TOKEN", ""), // Token 本身就是可选鉴权项，留空 = 关闭 Token 校验，这个语义不变

		IdleTimeoutMS:    envInt64("IDLE_TIMEOUT_MS", 120000),
		HeartbeatMS:      envInt64("HEARTBEAT_MS", 25000),
		ConnectTimeoutMS: envInt64("CONNECT_TIMEOUT_MS", 12000),
		ConnectRetries:   int(envInt64("CONNECT_RETRIES", 1)),
		RetryBaseMS:      envInt64("RETRY_BASE_MS", 200),
		MaxFrameBytes:    envInt64("MAX_FRAME_BYTES", 2*1024*1024),
		LogLevel:         envStr("LOG_LEVEL", "info"),

		MaxHeaderBufBytes: 8192,

		// yamux 默认单流窗口只有 256KB，在有一定延迟的链路上会严重限制单个
		// stream（比如一次大文件传输）的吞吐量——发送方发满 256KB 未确认数据
		// 就必须停下来等对端的窗口更新，相当于每 256KB 就insert一次往返等待。
		// 这里默认给到 20MB，通过 YAMUX_WINDOW_KB 可以按你的链路带宽时延积
		// （带宽 x RTT）调整：数值越大，大文件吞吐上限越高，但会增加每个
		// stream 的内存占用（默认配置下每个并发连接最多占这么多接收缓冲）。
		YamuxWindowBytes: uint32(envInt64("YAMUX_WINDOW_KB", 20*1024)) * 1024,

		YamuxWriteTimeoutMS: envInt64("YAMUX_WRITE_TIMEOUT_MS", 30000),
		NAT64Prefix:         envStr("NAT64_PREFIX", "64:ff9b::"),

		NumCPU: runtime.NumCPU(),
	}

	// 必填项校验：UUID 缺失时直接拒绝启动，不允许回退到任何默认值。
	if cfg.UUID == "" {
		fmt.Fprintln(os.Stderr, "错误: 必须通过环境变量设置 UUID（不再提供默认值，避免使用公开在源码里的默认凭据）")
		fmt.Fprintln(os.Stderr, "      cp server.env.example server.env 后编辑填入你自己的 UUID，再用 EnvironmentFile 加载，或者 export UUID=... 手动设置")
		os.Exit(1)
	}

	return cfg
}
