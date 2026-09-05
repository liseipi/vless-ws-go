package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

type nat64CacheEntry struct {
	addr     string
	expireAt time.Time
}

type NAT64Resolver struct {
	mu       sync.RWMutex
	cache    map[string]nat64CacheEntry
	ttl      time.Duration
	resolver *net.Resolver
	client   *http.Client
	log      *Logger
	failLog  *failRateLimiter
	prefix   string // NAT64 IPv6 前缀，见 config.go 里 NAT64Prefix 的注释
}

func NewNAT64Resolver(log *Logger, prefix string) *NAT64Resolver {
	if prefix == "" {
		prefix = "64:ff9b::" // RFC 6052 Well-Known Prefix，兜底
	}
	r := &NAT64Resolver{
		cache:    make(map[string]nat64CacheEntry),
		ttl:      5 * time.Minute,
		resolver: net.DefaultResolver,
		client:   &http.Client{Timeout: 5 * time.Second},
		log:      log,
		failLog:  newFailRateLimiter(60 * time.Second),
		prefix:   prefix,
	}
	go r.gcLoop()
	return r
}

func (r *NAT64Resolver) gcLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		r.mu.Lock()
		for k, v := range r.cache {
			if now.After(v.expireAt) {
				delete(r.cache, k)
			}
		}
		r.mu.Unlock()
	}
}

type dohResponse struct {
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"Answer"`
}

// Resolve 把目标地址转换为可直接拨号的地址：
//   - 若本身就是 IPv4，映射为 <NAT64前缀><IPv4>（默认 64:ff9b::x.x.x.x，
//     RFC 6052 标准写法，真实 NAT64 网关按这个前缀路由）
//   - 若是域名，先查系统 DNS，失败则走 DoH (Cloudflare) 兜底，结果做 TTL 缓存
//
// 【修复说明】之前这里固定用 "::ffff:" 前缀（IPv4-mapped IPv6 地址，
// RFC 4291），这只是一种"在 IPv6 socket 里表示 IPv4 地址"的文本记法，
// 不是真正的 NAT64 网关地址——只有本机本身还有 IPv4 直连能力时才可能碰巧
// 拨通，如果服务器确实处于纯 IPv6、需要靠 NAT64 网关访问 IPv4-only 目标
// 的场景，"::ffff:" 这个地址网关根本不认识，等于兜底完全不起作用。现在
// 默认换成 RFC 6052 定义的 Well-Known Prefix 64:ff9b::/96，绝大多数云厂商/
// 运营商提供的 NAT64 网关都用这个前缀；如果你的环境是自定义前缀，通过
// NAT64_PREFIX 环境变量覆盖即可。对不需要 NAT64 的普通部署没有任何影响
// （这条路径只在直连失败后才会被调用）。
func (r *NAT64Resolver) Resolve(ctx context.Context, target string) (string, error) {
	if ip := net.ParseIP(target); ip != nil && ip.To4() != nil {
		return r.prefix + target, nil
	}

	r.mu.RLock()
	if e, ok := r.cache[target]; ok && time.Now().Before(e.expireAt) {
		r.mu.RUnlock()
		return e.addr, nil
	}
	r.mu.RUnlock()

	// 方案1：系统 DNS
	if addrs, err := r.resolver.LookupHost(ctx, target); err == nil {
		for _, a := range addrs {
			if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
				mapped := r.prefix + a
				r.store(target, mapped)
				return mapped, nil
			}
		}
	} else {
		r.log.Debug(fmt.Sprintf("[dns] system DNS miss for %s: %s", target, err.Error()))
	}

	// 方案2：DoH 兜底 (Cloudflare)
	dohURL := fmt.Sprintf("https://1.1.1.1/dns-query?name=%s&type=A", target)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dohURL, nil)
	if err == nil {
		req.Header.Set("Accept", "application/dns-json")
		resp, err := r.client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			var data dohResponse
			if json.NewDecoder(resp.Body).Decode(&data) == nil {
				for _, ans := range data.Answer {
					if ans.Type == 1 { // A record
						mapped := r.prefix + ans.Data
						r.store(target, mapped)
						return mapped, nil
					}
				}
			}
		} else {
			r.log.Debug(fmt.Sprintf("[dns] DoH miss for %s: %s", target, err.Error()))
		}
	}

	return "", fmt.Errorf("NAT64: no A record for %s", target)
}

func (r *NAT64Resolver) store(target, mapped string) {
	r.mu.Lock()
	r.cache[target] = nat64CacheEntry{addr: mapped, expireAt: time.Now().Add(r.ttl)}
	r.mu.Unlock()
}
