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
	LogLevel          string
	MaxHeaderBufBytes int
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
		LogLevel:          envStr("LOG_LEVEL", "info"),
		MaxHeaderBufBytes: 8192,
		NumCPU:            runtime.NumCPU(),
	}
}
