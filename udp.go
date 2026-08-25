package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const maxUDPPacketSize = 65507 // UDP 单包理论最大载荷（IPv4）

func writeUDPFrame(w io.Writer, payload []byte) error {
	if len(payload) > 0xFFFF {
		return fmt.Errorf("udp packet too large: %d bytes", len(payload))
	}
	lenBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(lenBuf, uint16(len(payload)))
	if _, err := w.Write(lenBuf); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readUDPFrame(r io.Reader) ([]byte, error) {
	lenBuf := make([]byte, 2)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(lenBuf)
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	return payload, nil
}

func (s *Server) dialUpstreamUDP(ctx context.Context, addr string, port uint16, sid string) (*net.UDPConn, error) {
	target := net.JoinHostPort(addr, strconv.Itoa(int(port)))
	udpAddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		s.log.Debug(fmt.Sprintf("[%s] udp resolve err %s: %s (will NAT64 retry)", sid, target, err.Error()))
		mapped, rerr := s.resolver.Resolve(ctx, addr)
		if rerr != nil {
			return nil, fmt.Errorf("nat64: %w (original: %v)", rerr, err)
		}
		mappedTarget := net.JoinHostPort(mapped, strconv.Itoa(int(port)))
		udpAddr, err = net.ResolveUDPAddr("udp", mappedTarget)
		if err != nil {
			return nil, fmt.Errorf("nat64 resolve: %w", err)
		}
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func relayUDP(nc io.ReadWriteCloser, udpConn *net.UDPConn, idleTimeout time.Duration, touch func()) {
	errCh := make(chan error, 2)

	go func() {
		for {
			payload, err := readUDPFrame(nc)
			if err != nil {
				errCh <- err
				return
			}
			touch()
			if _, err := udpConn.Write(payload); err != nil {
				errCh <- err
				return
			}
		}
	}()

	go func() {
		buf := make([]byte, maxUDPPacketSize)
		for {
			if idleTimeout > 0 {
				_ = udpConn.SetReadDeadline(time.Now().Add(idleTimeout))
			}
			n, err := udpConn.Read(buf)
			if err != nil {
				errCh <- err
				return
			}
			touch()
			if err := writeUDPFrame(nc, buf[:n]); err != nil {
				errCh <- err
				return
			}
		}
	}()

	<-errCh
	_ = udpConn.Close()
	_ = nc.Close()
	<-errCh
}

type prefixReader struct {
	prefix []byte
	r      io.Reader
}

func (p *prefixReader) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.r.Read(b)
}

// handleUDPSessionStream 处理一个 yamux stream 上的 UDP 会话。与 TCP 一样，
// 出错时只关闭这一个 stream，不影响同一 WS 连接上的其它并发请求。
func (s *Server) handleUDPSessionStream(
	ctx context.Context,
	stream io.ReadWriteCloser,
	hdr *vlessHeader,
	tail []byte,
	sid string,
	touch func(),
) {
	connectTimeout := time.Duration(s.cfg.ConnectTimeoutMS) * time.Millisecond
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	udpConn, err := s.dialUpstreamUDP(dctx, hdr.Addr, hdr.Port, sid)
	cancel()
	if err != nil {
		s.log.Warn(fmt.Sprintf("[%s] udp connect failed %s:%d: %s", sid, hdr.Addr, hdr.Port, err.Error()))
		_ = stream.Close()
		return
	}
	defer udpConn.Close()

	if _, err := stream.Write([]byte{vlessVer, 0x00}); err != nil {
		s.log.Debug(fmt.Sprintf("[%s] write vless resp: %s", sid, err.Error()))
		return
	}

	reader := &prefixReader{prefix: tail, r: stream}

	idleTimeout := time.Duration(s.cfg.IdleTimeoutMS) * time.Millisecond
	relayUDP(&udpReadWriteCloser{Reader: reader, Writer: stream, Closer: stream}, udpConn, idleTimeout, touch)
}

type udpReadWriteCloser struct {
	io.Reader
	io.Writer
	io.Closer
}
