package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

func main() {
	cfg := LoadConfig()
	log := NewLogger(cfg.LogLevel)

	srv, err := NewServer(cfg, log)
	if err != nil {
		log.Error("config error:", err.Error())
		os.Exit(1)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"ts":%d}`, time.Now().UnixMilli())
	})

	mux.HandleFunc(cfg.WSPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cfg.WSPath {
			http.NotFound(w, r)
			return
		}
		if r.Host == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		ip := clientIP(r)

		if cfg.Token != "" {
			tok := r.Header.Get("X-Auth-Token")
			if tok == "" {
				tok = r.URL.Query().Get("token")
			}
			a, b := []byte(tok), []byte(cfg.Token)
			if len(a) != len(b) || subtle.ConstantTimeCompare(a, b) != 1 {
				srv.authFail.shouldLog(ip)
				log.Warn(fmt.Sprintf("[auth] fail from %s", ip))
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}

		wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
			CompressionMode:    websocket.CompressionDisabled,
		})
		if err != nil {
			log.Debug("accept:", err.Error())
			return
		}
		wsConn.SetReadLimit(-1) // 单帧大小不限制，由对端决定分帧策略

		srv.HandleSession(wsConn, ip)
	})

	httpServer := &http.Server{
		Addr:    net.JoinHostPort(cfg.Host, cfg.Port),
		Handler: mux,
	}

	go func() {
		printBanner(cfg)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("listen error:", err.Error())
			os.Exit(1)
		}
	}()

	// ── 优雅关闭 ──────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Info(fmt.Sprintf("%s — shutting down", sig.String()))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("forced shutdown:", err.Error())
	}
}

// clientIP 优先取 X-Forwarded-For 首个地址，其次取 RemoteAddr。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func printBanner(cfg *Config) {
	const w = 60
	bar := strings.Repeat("═", w)
	row := func(s string) string {
		if len(s) < w-2 {
			s = s + strings.Repeat(" ", w-2-len(s))
		}
		return "║  " + s + "║"
	}
	fmt.Printf("\x1b[36m╔%s╗\n", bar)
	fmt.Println(row("VLESS WebSocket Server (Go) — READY"))
	fmt.Printf("╠%s╣\n", bar)
	fmt.Println(row(fmt.Sprintf("Listen   : %s:%s", cfg.Host, cfg.Port)))
	fmt.Println(row(fmt.Sprintf("WS Path  : %s", cfg.WSPath)))
	fmt.Println(row(fmt.Sprintf("UUID     : %s", cfg.UUID)))
	tokState := "disabled"
	if cfg.Token != "" {
		tokState = "enabled"
	}
	fmt.Println(row(fmt.Sprintf("Token    : %s", tokState)))
	fmt.Println(row("NAT64    : auto (system DNS -> DoH, TTL cache)"))
	fmt.Println(row("Backpressure: native (blocking I/O via io.Copy)"))
	fmt.Println(row("MaxPayload: unlimited, adaptive to peer frame size"))
	fmt.Println(row(fmt.Sprintf("Heartbeat: %dms   IdleTO: %dms", cfg.HeartbeatMS, cfg.IdleTimeoutMS)))
	fmt.Println(row(fmt.Sprintf("GOMAXPROCS: %d (runtime auto multi-core)", cfg.NumCPU)))
	fmt.Printf("╚%s╝\x1b[0m\n\n", bar)
}
