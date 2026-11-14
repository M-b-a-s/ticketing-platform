package middleware

import (
	"context"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/httpx"
)

// Limiter is a set of token buckets, one per key (an IP, an email, ...).
// Each key may make `burst` requests at once, then one more every `every`.
// State is in memory: it is per process and resets on restart.
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*entry
	every   time.Duration
	burst   int
	ttl     time.Duration
	now     func() time.Time
}

type entry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

func NewLimiter(every time.Duration, burst int) *Limiter {
	// After a full refill a bucket is indistinguishable from a new one,
	// so an entry idle for longer than that can be dropped with no effect.
	ttl := every * time.Duration(burst) * 2
	if ttl < time.Minute {
		ttl = time.Minute
	}
	return &Limiter{
		entries: make(map[string]*entry),
		every:   every,
		burst:   burst,
		ttl:     ttl,
		now:     time.Now,
	}
}

// Allow consumes one token for key. If none is available it returns false and
// how long until one is. A rejected request does NOT consume anything, so
// hammering the endpoint doesn't extend the wait.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		e = &entry{lim: rate.NewLimiter(rate.Every(l.every), l.burst)}
		l.entries[key] = e
	}
	e.lastSeen = now

	res := e.lim.ReserveN(now, 1)
	delay := res.DelayFrom(now)
	if res.OK() && delay == 0 {
		return true, 0
	}
	res.CancelAt(now)
	if !res.OK() {
		delay = l.every
	}
	return false, delay
}

// Run evicts idle keys until ctx is cancelled, so memory stays bounded.
func (l *Limiter) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.sweep()
		}
	}
}

func (l *Limiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-l.ttl)
	for k, e := range l.entries {
		if e.lastSeen.Before(cutoff) {
			delete(l.entries, k)
		}
	}
}

// WriteRateLimited sends the standard 429 response.
func WriteRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, try again shortly", nil)
}

// RateLimit rejects requests once the key's bucket is empty.
func RateLimit(l *Limiter, key func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, retry := l.Allow(key(r)); !ok {
				WriteRateLimited(w, retry)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP identifies the caller. Behind a reverse proxy (Nginx) the socket
// address is the proxy's, so with trustProxy set we use the LAST entry of
// X-Forwarded-For: that is the address our own proxy appended. Earlier entries
// are client-supplied and trivially spoofed. Only enable trustProxy when
// traffic really does pass through a proxy you control, because otherwise a
// client can send a fake header and dodge every limit.
func ClientIP(trustProxy bool) func(*http.Request) netip.Addr {
	return func(r *http.Request) netip.Addr {
		if trustProxy {
			if xff := strings.Join(r.Header.Values("X-Forwarded-For"), ","); xff != "" {
				last := strings.TrimSpace(xff[strings.LastIndex(xff, ",")+1:])
				if addr, err := netip.ParseAddr(last); err == nil {
					return addr.Unmap()
				}
			}
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		addr, _ := netip.ParseAddr(host)
		return addr.Unmap()
	}
}

// KeyByIP is the usual key function for RateLimit.
func KeyByIP(trustProxy bool) func(*http.Request) string {
	ip := ClientIP(trustProxy)
	return func(r *http.Request) string { return ip(r).String() }
}