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
}

func NewNAT64Resolver(log *Logger) *NAT64Resolver {
	r := &NAT64Resolver{
		cache:    make(map[string]nat64CacheEntry),
		ttl:      5 * time.Minute,
		resolver: net.DefaultResolver,
		client:   &http.Client{Timeout: 5 * time.Second},
		log:      log,
		failLog:  newFailRateLimiter(60 * time.Second),
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
//   - 若本身就是 IPv4，直接映射为 ::ffff:x.x.x.x
//   - 若是域名，先查系统 DNS，失败则走 DoH (Cloudflare) 兜底，结果做 TTL 缓存
func (r *NAT64Resolver) Resolve(ctx context.Context, target string) (string, error) {
	if ip := net.ParseIP(target); ip != nil && ip.To4() != nil {
		return "::ffff:" + target, nil
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
				mapped := "::ffff:" + a
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
						mapped := "::ffff:" + ans.Data
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
