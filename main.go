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
	positiveCacheTTL            = 5 * time.Second
	negativeCacheTTL            = 2 * time.Second
	avahiRefreshInterval        = 5 * time.Minute
	responseTTL          uint32 = 10
	resolveTimeout              = 500 * time.Millisecond
	resolveAttempts             = 3
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
	return cachedResolveCtx(context.Background(), name, qtype)
}

func cachedResolveCtx(ctx context.Context, name string, qtype uint16) (net.IP, error) {
	key := cacheKey(name, qtype)
	if ip, err, ok := cache.get(key); ok {
		return ip, err
	}

	ip, err := resolveMDNS(ctx, name, qtype)
	if err != nil {
		cache.set(key, nil, err, negativeCacheTTL)
		return nil, err
	}

	cache.set(key, ip, nil, positiveCacheTTL)
	return ip, nil
}

func recordForIP(name string, qtype uint16, ip net.IP) (dns.RR, bool) {
	switch qtype {
	case dns.TypeA:
		if a4 := ip.To4(); a4 != nil {
			return &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: responseTTL}, A: a4}, true
		}
	case dns.TypeAAAA:
		if ip6 := ip.To16(); ip6 != nil {
			return &dns.AAAA{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: responseTTL}, AAAA: ip6}, true
		}
	}
	return nil, false
}

func addRecordForQuery(msg *dns.Msg, q dns.Question) bool {
	ip, err := cachedResolve(q.Name, q.Qtype)
	if err != nil {
		log.Printf("mdns resolve failed for %s (%s): %v", q.Name, dns.TypeToString[q.Qtype], err)
		return false
	}

	rr, ok := recordForIP(q.Name, q.Qtype, ip)
	if !ok {
		return false
	}
	msg.Answer = append(msg.Answer, rr)
	return true
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
			// resolve A and AAAA in parallel
			var okA, okAAAA bool
			var wg sync.WaitGroup
			var mu sync.Mutex
			resolveCtx, cancel := context.WithTimeout(context.Background(), resolveTimeout*time.Duration(resolveAttempts))
			wg.Add(2)
			go func() {
				defer wg.Done()
				if ip, err := cachedResolveCtx(resolveCtx, a.Name, a.Qtype); err == nil {
					if rr, ok := recordForIP(a.Name, a.Qtype, ip); ok {
						mu.Lock()
						msg.Answer = append(msg.Answer, rr)
						okA = true
						mu.Unlock()
					}
				}
			}()
			go func() {
				defer wg.Done()
				if ip, err := cachedResolveCtx(resolveCtx, aaaa.Name, aaaa.Qtype); err == nil {
					if rr, ok := recordForIP(aaaa.Name, aaaa.Qtype, ip); ok {
						mu.Lock()
						msg.Answer = append(msg.Answer, rr)
						okAAAA = true
						mu.Unlock()
					}
				}
			}()
			wg.Wait()
			cancel()
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

func periodicWarmUpAvahi(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			warmUpAvahi(ctx)
		}
	}
}

func newUDPServer(addr string, network string) (*dns.Server, error) {
	pc, err := net.ListenPacket(network, addr)
	if err != nil {
		return nil, err
	}
	return &dns.Server{Addr: addr, Net: network, PacketConn: pc, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}, nil
}

func newTCPServer(addr string, network string) (*dns.Server, error) {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	return &dns.Server{Addr: addr, Net: network, Listener: ln, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}, nil
}

func main() {
	addrLegacy := flag.String("addr", "", "(deprecated) listen address for IPv4 (udp4/tcp4)")
	addr4 := flag.String("addr4", ":53", "listen address for IPv4 (udp4/tcp4); empty disables IPv4")
	addr6 := flag.String("addr6", "[::]:53", "listen address for IPv6 (udp6/tcp6); empty disables IPv6")
	warm := flag.Bool("warmup", true, "run avahi-browse warmup")
	flag.Parse()

	dns.HandleFunc(".", handleDNS)

	warmCtx, cancelWarm := context.WithCancel(context.Background())
	defer cancelWarm()

	if *warm {
		go warmUpAvahi(warmCtx)
		go periodicWarmUpAvahi(warmCtx, avahiRefreshInterval)
	}

	type serverEntry struct {
		server *dns.Server
		label  string
	}

	if *addrLegacy != "" {
		*addr4 = *addrLegacy
	}

	var servers []serverEntry

	// IPv4 listeners
	if *addr4 != "" {
		if udp4, err := newUDPServer(*addr4, "udp4"); err != nil {
			log.Printf("udp4 listen failed on %s: %v", *addr4, err)
		} else {
			servers = append(servers, serverEntry{udp4, "udp4"})
		}
		if tcp4, err := newTCPServer(*addr4, "tcp4"); err != nil {
			log.Printf("tcp4 listen failed on %s: %v", *addr4, err)
		} else {
			servers = append(servers, serverEntry{tcp4, "tcp4"})
		}
	}

	// IPv6 listeners
	if *addr6 != "" {
		if udp6, err := newUDPServer(*addr6, "udp6"); err != nil {
			log.Printf("udp6 listen failed on %s: %v", *addr6, err)
		} else {
			servers = append(servers, serverEntry{udp6, "udp6"})
		}
		if tcp6, err := newTCPServer(*addr6, "tcp6"); err != nil {
			log.Printf("tcp6 listen failed on %s: %v", *addr6, err)
		} else {
			servers = append(servers, serverEntry{tcp6, "tcp6"})
		}
	}

	if len(servers) == 0 {
		log.Fatalf("no listeners started; check -addr4/-addr6")
	}

	type serverError struct {
		label string
		err   error
	}

	errCh := make(chan serverError, len(servers))
	active := len(servers)

	for _, s := range servers {
		go func(se serverEntry) {
			log.Printf("Starting DNS → mDNS bridge on %s (%s)", se.server.Addr, se.label)
			if err := se.server.ActivateAndServe(); err != nil {
				errCh <- serverError{label: se.label, err: err}
			}
		}(s)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case sig := <-sigCh:
			log.Printf("Received signal %s, shutting down", sig)
			cancelWarm()
			for _, s := range servers {
				if err := s.server.Shutdown(); err != nil {
					log.Printf("%s shutdown error: %v", s.label, err)
				}
			}
			return
		case srvErr := <-errCh:
			log.Printf("server error (%s): %v", srvErr.label, srvErr.err)
			active--
			if active == 0 {
				cancelWarm()
				log.Fatalf("all listeners stopped")
			}
		}
	}
}
