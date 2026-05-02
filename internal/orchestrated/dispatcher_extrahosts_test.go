package orchestrated

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/config"
)

// stubResolver is a test double for hostResolver. Calls increments a counter;
// sleep simulates a slow authoritative DNS server; addrs is the canned
// response used when the sleep (if any) completes in time.
type stubResolver struct {
	calls atomic.Int64
	sleep time.Duration
	addrs []string
	err   error
}

func (s *stubResolver) LookupHost(ctx context.Context, _ string) ([]string, error) {
	s.calls.Add(1)

	if s.sleep > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.sleep):
		}
	}

	if s.err != nil {
		return nil, s.err
	}

	return s.addrs, nil
}

func newTestDispatcher() *Dispatcher {
	return &Dispatcher{
		cfg:      &config.Config{},
		logger:   slog.Default(),
		dnsCache: newDNSCache(dnsCacheTTL, dnsCacheCapacity),
		// Default to a stub that returns NXDOMAIN so unrelated tests using
		// buildWorkerSpec don't hit the real network resolver.
		resolver: &stubResolver{err: errors.New("no resolver configured")},
	}
}

// TestBuildExtraHosts_ResolvesMCPHost asserts that buildExtraHosts calls the
// host resolver for the MCP URL hostname and adds the resolved IP to the
// container's /etc/hosts. This is what makes a hostname only present in the
// runner host's /etc/hosts (e.g. a LAN-only domain) reachable from inside
// the worker container.
func TestBuildExtraHosts_ResolvesMCPHost(t *testing.T) {
	t.Parallel()

	dp := newTestDispatcher()
	dp.resolver = &stubResolver{addrs: []string{"10.0.0.42"}}

	hosts := dp.buildExtraHosts(context.Background(), "https://cm.lan.example/mcp")

	assert.Contains(t, hosts, "host.docker.internal:host-gateway")
	assert.Contains(t, hosts, "cm.lan.example:10.0.0.42")
}

// TestBuildExtraHosts_SkipsIPLiteral asserts that an MCP URL with an IP
// hostname is not added to ExtraHosts (it would be a no-op or worse, a
// shadow entry).
func TestBuildExtraHosts_SkipsIPLiteral(t *testing.T) {
	t.Parallel()

	dp := newTestDispatcher()
	stub := &stubResolver{addrs: []string{"10.0.0.42"}}
	dp.resolver = stub

	hosts := dp.buildExtraHosts(context.Background(), "http://192.168.1.10:8080/mcp")

	assert.Equal(t, []string{"host.docker.internal:host-gateway"}, hosts)
	assert.Equal(t, int64(0), stub.calls.Load(), "IP literals must not trigger a resolver call")
}

// TestBuildExtraHosts_SkipsLocalhost asserts that localhost / host.docker.internal
// are not double-added (host-gateway already covers the second).
func TestBuildExtraHosts_SkipsLocalhost(t *testing.T) {
	t.Parallel()

	dp := newTestDispatcher()
	stub := &stubResolver{addrs: []string{"10.0.0.42"}}
	dp.resolver = stub

	for _, url := range []string{
		"http://localhost:8080/mcp",
		"http://host.docker.internal:8080/mcp",
	} {
		hosts := dp.buildExtraHosts(context.Background(), url)
		assert.Equal(t, []string{"host.docker.internal:host-gateway"}, hosts, url)
	}

	assert.Equal(t, int64(0), stub.calls.Load(), "skipped hostnames must not trigger a resolver call")
}

// TestBuildExtraHosts_DNSTimeout asserts that a slow authoritative DNS server
// cannot stall the spawn path indefinitely. The stub sleeps well past the cap;
// buildExtraHosts must return within a small envelope of the cap and return
// only the default host-gateway entry.
func TestBuildExtraHosts_DNSTimeout(t *testing.T) {
	t.Parallel()

	dp := newTestDispatcher()
	dp.resolver = &stubResolver{sleep: 5 * time.Second, addrs: []string{"10.0.0.1"}}

	start := time.Now()
	hosts := dp.buildExtraHosts(context.Background(), "http://slow-dns.example:8080/mcp")
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 3*time.Second,
		"buildExtraHosts must honour the 2s cap; got %s", elapsed)
	assert.Equal(t, []string{"host.docker.internal:host-gateway"}, hosts,
		"on timeout buildExtraHosts must return only the default entry")
}

// TestBuildExtraHosts_DNSCache asserts that a second resolution for the same
// hostname is served from the cache without a second resolver call.
func TestBuildExtraHosts_DNSCache(t *testing.T) {
	t.Parallel()

	dp := newTestDispatcher()
	stub := &stubResolver{addrs: []string{"192.0.2.17"}}
	dp.resolver = stub

	h1 := dp.buildExtraHosts(context.Background(), "http://cache-me.example:8080/mcp")
	require.Contains(t, h1, "cache-me.example:192.0.2.17")
	require.Equal(t, int64(1), stub.calls.Load(), "first call must hit the resolver")

	h2 := dp.buildExtraHosts(context.Background(), "http://cache-me.example:8080/mcp")
	require.Contains(t, h2, "cache-me.example:192.0.2.17")
	assert.Equal(t, int64(1), stub.calls.Load(),
		"second call with same hostname must be served from cache; got %d calls", stub.calls.Load())
}

// TestBuildExtraHosts_LookupError asserts that a generic resolver failure
// degrades gracefully — the container still starts with the default
// host-gateway entry but no MCP-host mapping.
func TestBuildExtraHosts_LookupError(t *testing.T) {
	t.Parallel()

	dp := newTestDispatcher()
	dp.resolver = &stubResolver{err: errors.New("nxdomain")}

	hosts := dp.buildExtraHosts(context.Background(), "http://does-not-exist.example/mcp")

	assert.Equal(t, []string{"host.docker.internal:host-gateway"}, hosts)
}

// TestBuildExtraHosts_BadURL asserts that an unparseable MCP URL doesn't
// blow up the spawn path; we just fall back to the default entry.
func TestBuildExtraHosts_BadURL(t *testing.T) {
	t.Parallel()

	dp := newTestDispatcher()
	dp.resolver = &stubResolver{addrs: []string{"10.0.0.1"}}

	hosts := dp.buildExtraHosts(context.Background(), "://not a url")

	assert.Equal(t, []string{"host.docker.internal:host-gateway"}, hosts)
}
