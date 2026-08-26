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
	"github.com/hashicorp/yamux"
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

// HandleSession 处理单条底层 WebSocket 连接的完整生命周期。
//
// 与之前"一条 WS 连接 = 一次 VLESS 会话"的模型不同，这里在 WS 连接之上
// 建立一个 yamux 服务端多路复用会话：客户端可以在同一条 WS 连接上开出
// 任意多个逻辑 stream，每个 stream 都是一次独立的 VLESS 请求（一次 TCP
// CONNECT 或一个 UDP 目标），互不影响——某个 stream 出错或关闭，只影响
// 这一个逻辑连接，不会波及同一 WS 连接上的其它 stream，更不会打断底层
// WS 本身。这样客户端多个并发请求可以复用同一条已经完成 TLS+WS 握手
// 的连接，避免重复握手开销。
//
// WS 连接本身的存活检测交给 yamux 内置的 keepalive 机制（映射自
// cfg.HeartbeatMS），不再需要额外的 Ping/idle-watchdog goroutine。
func (s *Server) HandleSession(wsConn *websocket.Conn, remoteIP string) {
	cid := newSID()
	cfg := s.cfg
	log := s.log

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nc := websocket.NetConn(ctx, wsConn, websocket.MessageBinary)

	ymCfg := yamux.DefaultConfig()
	ymCfg.LogOutput = io.Discard // 用自己的 Logger，屏蔽 yamux 自带的标准库日志输出
	ymCfg.EnableKeepAlive = true
	if hb := time.Duration(cfg.HeartbeatMS) * time.Millisecond; hb > 0 {
		ymCfg.KeepAliveInterval = hb
	}
	// keepalive 探测多次超时未响应即视为死连接；ConnectionWriteTimeout 控制单次写入的
	// 最长阻塞时间，避免某次网络抖动导致 goroutine 永久卡死。
	ymCfg.ConnectionWriteTimeout = 15 * time.Second
	// 见 config.go 里 YamuxWindowBytes 的注释：默认 256KB 的窗口会严重限制
	// 单个 stream（比如大文件传输）的吞吐量，这里换成更大的窗口。
	if cfg.YamuxWindowBytes > 0 {
		ymCfg.MaxStreamWindowSize = cfg.YamuxWindowBytes
	}

	sess, err := yamux.Server(nc, ymCfg)
	if err != nil {
		log.Warn(fmt.Sprintf("[%s] yamux session init: %s", cid, err.Error()))
		_ = nc.Close()
		return
	}
	defer sess.Close()

	log.Debug(fmt.Sprintf("[%s] session established from %s", cid, remoteIP))

	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			log.Debug(fmt.Sprintf("[%s] session closed: %s", cid, err.Error()))
			return
		}
		go s.handleStream(ctx, stream, remoteIP)
	}
}

// handleStream 处理 yamux 会话里的单个 stream：解析 VLESS 头 → 建立上游
// TCP/UDP 连接（含 NAT64 回退）→ 双向转发直至任一侧关闭。逻辑与之前
// 直接处理整条 WS 连接时基本一致，只是读写对象从 WS NetConn 换成了
// yamux 的 stream，且出错时只关闭这个 stream 而不是整条 WS 连接。
func (s *Server) handleStream(ctx context.Context, stream *yamux.Stream, remoteIP string) {
	sid := newSID()
	cfg := s.cfg
	log := s.log
	defer stream.Close()

	// ── 单个逻辑连接的空闲超时：用时间戳轮询而非每帧重建定时器 ──
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
					log.Warn(fmt.Sprintf("[%s] stream idle timeout", sid))
					_ = stream.Close()
					return
				}
			}
		}
	}()
	defer close(idleDone)

	// ── 阶段1：攒包解析 VLESS 头 ──────────────────────────
	buf := make([]byte, 0, cfg.MaxHeaderBufBytes)
	readBuf := make([]byte, 4096)
	var hdr *vlessHeader

	for {
		n, err := stream.Read(readBuf)
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

	if hdr.Cmd != cmdTCP && hdr.Cmd != cmdUDP {
		log.Warn(fmt.Sprintf("[%s] unsupported cmd 0x%02x", sid, hdr.Cmd))
		return
	}
	tail := buf[hdr.HeaderLen:]

	if hdr.Cmd == cmdUDP {
		s.handleUDPSessionStream(ctx, stream, hdr, tail, sid, touch)
		return
	}

	// ── 建立上游 TCP 连接（失败时尝试 NAT64 回退）───────────
	connectTimeout := time.Duration(cfg.ConnectTimeoutMS) * time.Millisecond
	tcpConn, err := s.dialUpstream(ctx, hdr.Addr, hdr.Port, connectTimeout, sid)
	if err != nil {
		log.Warn(fmt.Sprintf("[%s] connect failed %s:%d: %s", sid, hdr.Addr, hdr.Port, err.Error()))
		// 只关闭这一个逻辑 stream，不影响同一 WS 连接上的其它并发请求。
		_ = stream.Close()
		return
	}
	defer tcpConn.Close()

	if tc, ok := tcpConn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(10 * time.Second)
	}

	// VLESS 响应头：版本 + 附加信息长度(0)
	if _, err := stream.Write([]byte{vlessVer, 0x00}); err != nil {
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
		_, err := io.Copy(tcpConn, &touchReader{r: stream, touch: touch})
		_ = tcpConn.Close() // 半关闭：驱动另一方向尽快退出
		errCh <- err
	}()

	_, err = io.Copy(stream, &touchReader{r: tcpConn, touch: touch})
	_ = stream.Close()
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
