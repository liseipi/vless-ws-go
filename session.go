package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type Server struct {
	cfg      *Config
	log      *Logger
	resolver *NAT64Resolver
	srvUUID  [16]byte
	authFail *failRateLimiter
}

func NewServer(cfg *Config, log *Logger) (*Server, error) {
	uuidBytes, err := uuidToBytes(cfg.UUID)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:      cfg,
		log:      log,
		resolver: NewNAT64Resolver(log),
		srvUUID:  uuidBytes,
		authFail: newFailRateLimiter(60 * time.Second),
	}, nil
}

func newSID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// touchReader 在每次成功读取时回调 touch()，用于驱动"空闲超时"检测，
// 同时不影响底层 Read 的阻塞语义（阻塞写入自带背压，见 README 说明）。
type touchReader struct {
	r     io.Reader
	touch func()
}

func (t *touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.touch()
	}
	return n, err
}

// HandleSession 处理单个 WebSocket 连接的完整生命周期：
// 解析 VLESS 头 → 建立上游 TCP（含 NAT64 回退）→ 双向转发直至任一侧关闭。
func (s *Server) HandleSession(wsConn *websocket.Conn, remoteIP string) {
	sid := newSID()
	cfg := s.cfg
	log := s.log

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nc := websocket.NetConn(ctx, wsConn, websocket.MessageBinary)
	defer nc.Close()

	// ── 空闲超时：用时间戳轮询而非每帧重建定时器 ──
	var lastActivity int64
	touch := func() { atomic.StoreInt64(&lastActivity, time.Now().UnixNano()) }
	touch()

	idleTimeout := time.Duration(cfg.IdleTimeoutMS) * time.Millisecond
	watchdogInterval := idleTimeout
	if watchdogInterval > 30*time.Second {
		watchdogInterval = 30 * time.Second
	}
	idleDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(watchdogInterval)
		defer ticker.Stop()
		for {
			select {
			case <-idleDone:
				return
			case <-ticker.C:
				last := time.Unix(0, atomic.LoadInt64(&lastActivity))
				if time.Since(last) > idleTimeout {
					log.Warn(fmt.Sprintf("[%s] idle timeout", sid))
					_ = nc.Close()
					return
				}
			}
		}
	}()
	defer close(idleDone)

	// ── 服务端心跳：定期 Ping，coder/websocket 会自动等待 Pong 并处理，
	//    超时或连接已关闭时 Ping 返回 error，视为僵尸连接直接关闭 ──
	heartbeatInterval := time.Duration(cfg.HeartbeatMS) * time.Millisecond
	hbDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-ticker.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, heartbeatInterval)
				err := wsConn.Ping(pingCtx)
				pingCancel()
				if err != nil {
					log.Warn(fmt.Sprintf("[%s] zombie terminated (ping failed)", sid))
					_ = nc.Close()
					return
				}
			}
		}
	}()
	defer close(hbDone)

	// ── 阶段1：攒包解析 VLESS 头 ──────────────────────────
	buf := make([]byte, 0, cfg.MaxHeaderBufBytes)
	readBuf := make([]byte, 4096)
	var hdr *vlessHeader

	for {
		n, err := nc.Read(readBuf)
		if n > 0 {
			touch()
			buf = append(buf, readBuf[:n]...)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Debug(fmt.Sprintf("[%s] read header: %s", sid, err.Error()))
			}
			return
		}
		if len(buf) > cfg.MaxHeaderBufBytes {
			log.Warn(fmt.Sprintf("[%s] header overflow", sid))
			return
		}

		h, perr := parseVlessHeader(buf, s.srvUUID)
		if perr == errNeedMore {
			continue
		}
		if perr != nil {
			log.Warn(fmt.Sprintf("[%s] header: %s", sid, perr.Error()))
			s.authFail.shouldLog(remoteIP) // 复用限频记录（UUID 错误也算鉴权失败）
			return
		}
		hdr = h
		break
	}

	if hdr.Cmd != cmdTCP {
		log.Warn(fmt.Sprintf("[%s] unsupported cmd 0x%02x", sid, hdr.Cmd))
		return
	}
	tail := buf[hdr.HeaderLen:]

	// ── 建立上游 TCP 连接（失败时尝试 NAT64 回退）───────────
	connectTimeout := time.Duration(cfg.ConnectTimeoutMS) * time.Millisecond
	tcpConn, err := s.dialUpstream(ctx, hdr.Addr, hdr.Port, connectTimeout, sid)
	if err != nil {
		log.Warn(fmt.Sprintf("[%s] connect failed %s:%d: %s", sid, hdr.Addr, hdr.Port, err.Error()))
		_ = wsConn.Close(websocket.StatusInternalError, "connect failed")
		return
	}
	defer tcpConn.Close()

	if tc, ok := tcpConn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(10 * time.Second)
	}

	// VLESS 响应头：版本 + 附加信息长度(0)
	if _, err := nc.Write([]byte{vlessVer, 0x00}); err != nil {
		log.Debug(fmt.Sprintf("[%s] write vless resp: %s", sid, err.Error()))
		return
	}

	// flush：VLESS 头之后紧跟的应用层数据
	if len(tail) > 0 {
		if _, err := tcpConn.Write(tail); err != nil {
			log.Debug(fmt.Sprintf("[%s] flush tail: %s", sid, err.Error()))
			return
		}
	}

	// ── 阶段2：双向转发 ────────────────────────────────
	// 两个方向都用阻塞 I/O，读取速度天然被写入速度限制（背压免费获得）。
	errCh := make(chan error, 2)

	go func() {
		_, err := io.Copy(tcpConn, &touchReader{r: nc, touch: touch})
		_ = tcpConn.Close() // 半关闭：驱动另一方向尽快退出
		errCh <- err
	}()

	_, err = io.Copy(nc, &touchReader{r: tcpConn, touch: touch})
	_ = nc.Close()
	<-errCh // 等待另一方向 goroutine 结束，避免泄漏
}

// dialUpstream 先尝试直连，失败后走 NAT64 映射重试一次；
// 整个"直连+NAT64兜底"作为一次尝试，外层按 cfg.ConnectRetries 指数退避重试，
// 用来吸收目标网站/上游网络的瞬时抖动（超时、连接被重置等一次性问题）。
func (s *Server) dialUpstream(ctx context.Context, addr string, port uint16, timeout time.Duration, sid string) (net.Conn, error) {
	var lastErr error
	attempts := s.cfg.ConnectRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(s.cfg.RetryBaseMS) * time.Duration(1<<uint(attempt-1)) * time.Millisecond
			s.log.Debug(fmt.Sprintf("[%s] retry %d/%d connecting %s:%d in %s", sid, attempt, s.cfg.ConnectRetries, addr, port, backoff))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		conn, err := s.dialUpstreamOnce(ctx, addr, port, timeout, sid)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// dialUpstreamOnce 尝试一次直连，失败后走 NAT64 映射重试一次。
func (s *Server) dialUpstreamOnce(ctx context.Context, addr string, port uint16, timeout time.Duration, sid string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	target := net.JoinHostPort(addr, strconv.Itoa(int(port)))

	dctx, cancel := context.WithTimeout(ctx, timeout)
	conn, err := dialer.DialContext(dctx, "tcp", target)
	cancel()
	if err == nil {
		return conn, nil
	}
	s.log.Debug(fmt.Sprintf("[%s] connect err %s: %s (will NAT64 retry)", sid, target, err.Error()))

	mapped, rerr := s.resolver.Resolve(ctx, addr)
	if rerr != nil {
		return nil, fmt.Errorf("nat64: %w (original: %v)", rerr, err)
	}

	mappedTarget := net.JoinHostPort(mapped, strconv.Itoa(int(port)))
	dctx2, cancel2 := context.WithTimeout(ctx, timeout)
	defer cancel2()
	conn2, err2 := dialer.DialContext(dctx2, "tcp", mappedTarget)
	if err2 != nil {
		return nil, fmt.Errorf("nat64 retry: %w", err2)
	}
	return conn2, nil
}
