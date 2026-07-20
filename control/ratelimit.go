package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type tokenBucket struct {
	tokens float64
	last   time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64 // tokens added per second
	burst   float64 // bucket capacity (and initial fill)
}

func newRateLimiter(ratePerSec, burst float64) *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*tokenBucket), rate: ratePerSec, burst: burst}
}

func (rl *rateLimiter) allow(key string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b := rl.buckets[key]
	if b == nil {
		rl.buckets[key] = &tokenBucket{tokens: rl.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (rl *rateLimiter) sweep() {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for k, b := range rl.buckets {
		if now.Sub(b.last).Seconds()*rl.rate >= rl.burst {
			delete(rl.buckets, k)
		}
	}
}

func clientIP(r *http.Request) string {
	peer := remoteIP(r.RemoteAddr)
	if trustedProxy(peer, os.Getenv("CX_TRUSTED_PROXY_CIDRS")) {
		if xr := parseForwardedIP(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.LastIndexByte(xff, ','); i >= 0 {
				return parseForwardedIP(strings.TrimSpace(xff[i+1:]))
			}
			return parseForwardedIP(xff)
		}
	}
	return peer
}

func remoteIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return strings.TrimSpace(remoteAddr)
}

func parseForwardedIP(raw string) string {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return ""
	}
	return ip.String()
}

func trustedProxy(peer, rawCIDRs string) bool {
	ip := net.ParseIP(strings.TrimSpace(peer))
	if ip == nil || strings.TrimSpace(rawCIDRs) == "" {
		return false
	}
	for _, raw := range strings.Split(rawCIDRs, ",") {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func isRemote(r *http.Request) bool {
	ip := net.ParseIP(clientIP(r))
	return ip != nil && !ip.IsLoopback()
}

func (rl *rateLimiter) limitByIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			next.ServeHTTP(w, r)
			return
		}
		if isRemote(r) && !rl.allow(clientIP(r)) {
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) startRateLimitSweeper(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ipLimiter.sweep()
			s.buyerLimiter.sweep()
			s.workerLimiter.sweep()
			s.signupLimiter.sweep()
		}
	}
}
