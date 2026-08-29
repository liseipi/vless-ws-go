package main

import (
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
	return &Config{
		Host:              envStr("HOST", "0.0.0.0"),
		Port:              envStr("PORT", "8081"),
		WSPath:            envStr("WS_PATH", "/api"),
		UUID:              envStr("UUID", "a3d2e1f0-b4c5-4d6e-8f70-1a2b3c4d5e6f"),
		Token:             envStr("TOKEN", "1bc361e9ec74a28dc4694f130bff09f5c39cc09bc2fcf6df"),
		IdleTimeoutMS:     envInt64("IDLE_TIMEOUT_MS", 120000),
		HeartbeatMS:       envInt64("HEARTBEAT_MS", 25000),
		ConnectTimeoutMS:  envInt64("CONNECT_TIMEOUT_MS", 12000),
		ConnectRetries:    int(envInt64("CONNECT_RETRIES", 1)),
		RetryBaseMS:       envInt64("RETRY_BASE_MS", 200),
		MaxFrameBytes:     envInt64("MAX_FRAME_BYTES", 2*1024*1024),
		LogLevel:          envStr("LOG_LEVEL", "info"),
		MaxHeaderBufBytes: 8192,
		// yamux 默认单流窗口只有 256KB，在有一定延迟的链路上会严重限制单个
		// stream（比如一次大文件传输）的吞吐量——发送方发满 256KB 未确认数据
		// 就必须停下来等对端的窗口更新，相当于每 256KB 就insert一次往返等待。
		// 这里默认给到 16MB，通过 YAMUX_WINDOW_KB 可以按你的链路带宽时延积
		// （带宽 x RTT）调整：数值越大，大文件吞吐上限越高，但会增加每个
		// stream 的内存占用（默认配置下每个并发连接最多占这么多接收缓冲）。
		YamuxWindowBytes: uint32(envInt64("YAMUX_WINDOW_KB", 20*1024)) * 1024,
		NumCPU:           runtime.NumCPU(),
	}
}
