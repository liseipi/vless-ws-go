package main

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	vlessVer   byte = 0x00
	cmdTCP     byte = 0x01
	cmdUDP     byte = 0x02
	atypIPv4   byte = 0x01
	atypDomain byte = 0x02
	atypIPv6   byte = 0x03
)

// errNeedMore 表示当前缓冲区数据不足，需要等待更多字节，
// 与"格式错误"区分开。
var errNeedMore = errors.New("need more data")

type vlessHeader struct {
	Cmd       byte
	Port      uint16
	Addr      string
	HeaderLen int
}

// parseVlessHeader 尝试从 buf 中解析出完整的 VLESS 请求头。
// 返回 errNeedMore 时上层应继续攒包等待更多数据；
// 返回其他 error 时应视为协议错误直接断开连接。
func parseVlessHeader(buf []byte, srvUUID [16]byte) (*vlessHeader, error) {
	if len(buf) < 26 {
		return nil, errNeedMore
	}
	if buf[0] != vlessVer {
		return nil, fmt.Errorf("bad version 0x%02x", buf[0])
	}

	var reqUUID [16]byte
	copy(reqUUID[:], buf[1:17])
	if subtle.ConstantTimeCompare(reqUUID[:], srvUUID[:]) != 1 {
		return nil, fmt.Errorf("uuid mismatch")
	}

	addonsLen := int(buf[17])
	off := 18 + addonsLen
	if len(buf) < off+4 {
		return nil, errNeedMore
	}

	cmd := buf[off]
	off++
	port := binary.BigEndian.Uint16(buf[off : off+2])
	off += 2
	atyp := buf[off]
	off++

	var addr string
	switch atyp {
	case atypIPv4:
		if len(buf) < off+4 {
			return nil, errNeedMore
		}
		addr = fmt.Sprintf("%d.%d.%d.%d", buf[off], buf[off+1], buf[off+2], buf[off+3])
		off += 4
	case atypDomain:
		if len(buf) < off+1 {
			return nil, errNeedMore
		}
		dlen := int(buf[off])
		off++
		if dlen == 0 {
			return nil, fmt.Errorf("empty domain")
		}
		if len(buf) < off+dlen {
			return nil, errNeedMore
		}
		addr = string(buf[off : off+dlen])
		off += dlen
	case atypIPv6:
		if len(buf) < off+16 {
			return nil, errNeedMore
		}
		parts := make([]string, 8)
		for i := 0; i < 8; i++ {
			parts[i] = fmt.Sprintf("%x", binary.BigEndian.Uint16(buf[off+i*2:off+i*2+2]))
		}
		addr = ""
		for i, p := range parts {
			if i > 0 {
				addr += ":"
			}
			addr += p
		}
		off += 16
	default:
		return nil, fmt.Errorf("unknown atyp 0x%02x", atyp)
	}

	return &vlessHeader{Cmd: cmd, Port: port, Addr: addr, HeaderLen: off}, nil
}

// uuidToBytes 把标准 UUID 字符串（带或不带连字符）转换为 16 字节数组
func uuidToBytes(uuid string) ([16]byte, error) {
	var out [16]byte
	hex := make([]byte, 0, 32)
	for i := 0; i < len(uuid); i++ {
		c := uuid[i]
		if c == '-' {
			continue
		}
		hex = append(hex, c)
	}
	if len(hex) != 32 {
		return out, fmt.Errorf("invalid uuid: %s", uuid)
	}
	if _, err := decodeHexInto(out[:], hex); err != nil {
		return out, err
	}
	return out, nil
}

func decodeHexInto(dst, src []byte) (int, error) {
	n := len(src) / 2
	for i := 0; i < n; i++ {
		hi, err := hexVal(src[i*2])
		if err != nil {
			return 0, err
		}
		lo, err := hexVal(src[i*2+1])
		if err != nil {
			return 0, err
		}
		dst[i] = hi<<4 | lo
	}
	return n, nil
}

func hexVal(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("invalid hex char: %c", c)
	}
}
