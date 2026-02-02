package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
)

const (
	positiveCacheTTL        = 5 * time.Second
	negativeCacheTTL        = 2 * time.Second
	responseTTL      uint32 = 10
	resolveTimeout          = 500 * time.Millisecond
	resolveAttempts         = 3
)

type cacheEntry struct {
	ip      net.IP
	err     error
	expires time.Time
}

type resolverCache struct {
	mu sync.RWMutex
	m  map[string]cacheEntry
}

func newResolverCache() *resolverCache {
	return &resolverCache{m: make(map[string]cacheEntry)}
}

func (c *resolverCache) get(key string) (net.IP, error, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.m[key]
	if !ok || time.Now().After(entry.expires) {
		return nil, nil, false
	}
	// copy to avoid aliasing
	if entry.ip != nil {
		return append(net.IP(nil), entry.ip...), entry.err, true
	}
	return nil, entry.err, true
}

func (c *resolverCache) set(key string, ip net.IP, err error, ttl time.Duration) {
	c.mu.Lock()
	if ip != nil {
		ip = append(net.IP(nil), ip...)
	}
	c.m[key] = cacheEntry{ip: ip, err: err, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
}

var cache = newResolverCache()

func cacheKey(name string, qtype uint16) string {
	return fmt.Sprintf("%s|%d", strings.ToLower(name), qtype)
}

func pickIP(out string, qtype uint16) net.IP {
	fields := strings.Fields(out)
	for _, f := range fields {
		ip := net.ParseIP(f)
		if ip == nil {
			continue
		}
		switch qtype {
		case dns.TypeA:
			if ip4 := ip.To4(); ip4 != nil {
				return ip4
			}
		case dns.TypeAAAA:
			if ip.To4() == nil {
				return ip
			}
		default:
			return ip
		}
	}
	return nil
}

func resolveMDNS(ctx context.Context, name string, qtype uint16) (net.IP, error) {
	host := strings.TrimSuffix(name, ".")

	args := []string{"-n", host}
	switch qtype {
	case dns.TypeA:
		args = append([]string{"-4"}, args...)
	case dns.TypeAAAA:
		args = append([]string{"-6"}, args...)
	}

	var lastErr error
	for attempt := 0; attempt < resolveAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		attemptCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
		out, err := exec.CommandContext(attemptCtx, "avahi-resolve", args...).Output()
		timedOut := attemptCtx.Err() == context.DeadlineExceeded
		cancel()

		if err == nil {
			if ip := pickIP(string(out), qtype); ip != nil {
				return ip, nil
			}
			lastErr = errors.New("no valid IP in response")
		} else {
			lastErr = err
		}

		// If we timed out, retry immediately (avahi might just be slow)
		// Otherwise, wait a bit before retrying
		if !timedOut && attempt < resolveAttempts-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	return nil, fmt.Errorf("mdns resolution failed after %d attempts: %w", resolveAttempts, lastErr)
}

func cachedResolve(name string, qtype uint16) (net.IP, error) {
	key := cacheKey(name, qtype)
	if ip, err, ok := cache.get(key); ok {
		return ip, err
	}

	ip, err := resolveMDNS(context.Background(), name, qtype)
	if err != nil {
		cache.set(key, nil, err, negativeCacheTTL)
		return nil, err
	}

	cache.set(key, ip, nil, positiveCacheTTL)
	return ip, nil
}

func addRecordForQuery(msg *dns.Msg, q dns.Question) bool {
	ip, err := cachedResolve(q.Name, q.Qtype)
	if err != nil {
		log.Printf("mdns resolve failed for %s (%s): %v", q.Name, dns.TypeToString[q.Qtype], err)
		return false
	}

	switch q.Qtype {
	case dns.TypeA:
		a := &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: responseTTL}, A: ip.To4()}
		if a.A == nil {
			return false
		}
		msg.Answer = append(msg.Answer, a)
		return true
	case dns.TypeAAAA:
		aaaa := &dns.AAAA{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: responseTTL}, AAAA: ip.To16()}
		if aaaa.AAAA == nil {
			return false
		}
		msg.Answer = append(msg.Answer, aaaa)
		return true
	default:
		return false
	}
}

func handleDNS(w dns.ResponseWriter, r *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(r)
	answered := false

	for _, q := range r.Question {
		switch q.Qtype {
		case dns.TypeA, dns.TypeAAAA:
			if addRecordForQuery(msg, q) {
				answered = true
			}
		case dns.TypeANY:
			a := dns.Question{Name: q.Name, Qtype: dns.TypeA, Qclass: q.Qclass}
			aaaa := dns.Question{Name: q.Name, Qtype: dns.TypeAAAA, Qclass: q.Qclass}
			okA := addRecordForQuery(msg, a)
			okAAAA := addRecordForQuery(msg, aaaa)
			if okA || okAAAA {
				answered = true
			}
		}
	}

	if !answered {
		msg.Rcode = dns.RcodeNameError
	}

	if err := w.WriteMsg(msg); err != nil {
		log.Printf("WriteMsg error: %v", err)
	}
}

func warmUpAvahi(ctx context.Context) {
	warmCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := exec.CommandContext(warmCtx, "avahi-browse", "-a", "-r", "-t").Run(); err != nil {
		log.Printf("avahi-browse warmup failed: %v", err)
	}
}

func main() {
	addr := flag.String("addr", ":53", "listen address (udp and tcp)")
	warm := flag.Bool("warmup", true, "run avahi-browse warmup")
	flag.Parse()

	dns.HandleFunc(".", handleDNS)

	if *warm {
		go warmUpAvahi(context.Background())
	}

	udpServer := &dns.Server{Addr: *addr, Net: "udp", ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	tcpServer := &dns.Server{Addr: *addr, Net: "tcp", ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}

	errCh := make(chan error, 2)

	go func() {
		log.Printf("Starting DNS → mDNS bridge on %s (udp)", *addr)
		errCh <- udpServer.ListenAndServe()
	}()

	go func() {
		log.Printf("Starting DNS → mDNS bridge on %s (tcp)", *addr)
		errCh <- tcpServer.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("Received signal %s, shutting down", sig)
		if err := udpServer.Shutdown(); err != nil {
			log.Printf("udp shutdown error: %v", err)
		}
		if err := tcpServer.Shutdown(); err != nil {
			log.Printf("tcp shutdown error: %v", err)
		}
	case err := <-errCh:
		log.Fatalf("server error: %v", err)
	}
}
