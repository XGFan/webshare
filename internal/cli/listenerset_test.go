package cli

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/guofan/webshare-proxy/internal/auth"
	"github.com/guofan/webshare-proxy/internal/registry"
	"github.com/guofan/webshare-proxy/internal/routing"
	"github.com/guofan/webshare-proxy/internal/tunnel"
)

// TestReconfigure_SameBindNoOp documents the no-op short-circuit and is the
// baseline for the same-port-bind-change regression test below.
func TestReconfigure_SameBindNoOp(t *testing.T) {
	t.Parallel()
	httpPort, socksPort := freePortPair(t)

	l, cleanup := newRunningSet(t, "127.0.0.1", httpPort, "127.0.0.1", socksPort)
	defer cleanup()

	if err := l.Reconfigure(context.Background(), "127.0.0.1", httpPort, "127.0.0.1", socksPort); err != nil {
		t.Fatalf("Reconfigure no-op: %v", err)
	}
	running, http, socks := l.Status()
	if !running || portOf(http) != httpPort || portOf(socks) != socksPort {
		t.Fatalf("status after no-op: running=%v http=%s socks=%s", running, http, socks)
	}
}

// TestReconfigure_SameBindAddressChange covers the user-reported bug: keeping
// the same port but switching the bind address (127.0.0.1 → 0.0.0.0) must
// succeed. The old bind-then-close path always failed here because 0.0.0.0:N
// can't coexist with 127.0.0.1:N.
func TestReconfigure_SameBindAddressChange(t *testing.T) {
	t.Parallel()
	httpPort, socksPort := freePortPair(t)

	l, cleanup := newRunningSet(t, "127.0.0.1", httpPort, "127.0.0.1", socksPort)
	defer cleanup()

	if err := l.Reconfigure(context.Background(), "0.0.0.0", httpPort, "0.0.0.0", socksPort); err != nil {
		t.Fatalf("Reconfigure 127.0.0.1 → 0.0.0.0 (same port): %v", err)
	}
	running, http, socks := l.Status()
	if !running {
		t.Fatalf("expected listener running after bind swap, got running=false")
	}
	// Go's "tcp" listener for 0.0.0.0 reports [::] in dual-stack mode on
	// most platforms — assert on port only.
	if portOf(http) != httpPort || portOf(socks) != socksPort {
		t.Fatalf("ports drifted after bind swap: http=%s socks=%s want http_port=%d socks_port=%d",
			http, socks, httpPort, socksPort)
	}
}

// TestReconfigure_SamePortRollback verifies that when the new bind fails on
// the close-then-bind path, the old listener is restored rather than left
// stopped. Triggers the conflict by squatting on the target wildcard bind
// so the same-port path's new bind cannot succeed.
func TestReconfigure_SamePortRollback(t *testing.T) {
	t.Parallel()
	httpPort, socksPort := freePortPair(t)

	l, cleanup := newRunningSet(t, "127.0.0.1", httpPort, "127.0.0.1", socksPort)
	defer cleanup()

	// Squat a wildcard listener so that reconfiguring to 0.0.0.0:squatPort
	// fails the new bind regardless of platform (dual-stack vs IPv4-only).
	squatPort, squatCleanup := squatTCP(t, "0.0.0.0")
	defer squatCleanup()

	err := l.Reconfigure(context.Background(), "0.0.0.0", squatPort, "0.0.0.0", socksPort)
	if err == nil {
		t.Fatal("expected Reconfigure error when new HTTP port is squatted")
	}
	running, http, socks := l.Status()
	if !running {
		t.Fatalf("expected old listener restored after rollback, got running=false (err=%v)", err)
	}
	if portOf(http) != httpPort || portOf(socks) != socksPort {
		t.Fatalf("rollback did not restore original ports: http=%s socks=%s", http, socks)
	}
}

// --- helpers ---

func newRunningSet(t *testing.T, httpBind string, httpPort int, socksBind string, socksPort int) (*listenerSet, func()) {
	t.Helper()
	core := routing.NewCore(nil, nil)
	mgr := tunnel.New(core)
	reg := registry.New()
	deny := auth.New(nil)
	l := &listenerSet{mgr: mgr, reg: reg, denyList: deny}

	ctx, cancel := context.WithCancel(context.Background())
	if err := l.Start(ctx, httpBind, httpPort, socksBind, socksPort); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	cleanup := func() {
		l.Stop()
		cancel()
		// Brief wait so the kernel releases the sockets before the next
		// subtest binds to similar ports — unnecessary for the same-port
		// rebind tests above (they use freshly-allocated ports each time)
		// but keeps the helper safe to reuse.
		time.Sleep(10 * time.Millisecond)
	}
	return l, cleanup
}

// freePortPair returns two TCP ports that are currently unbound. We pick
// them by binding ":0", reading the assigned port, then closing — so the
// caller can bind them itself. Two separate sockets to avoid the same port.
func freePortPair(t *testing.T) (int, int) {
	t.Helper()
	a := pickFreePort(t)
	b := pickFreePort(t)
	if a == b {
		// 1 in 64k — re-roll once.
		b = pickFreePort(t)
	}
	return a, b
}

func pickFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen :0: %v", err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	n, _ := strconv.Atoi(p)
	return n
}

// squatTCP binds a fresh listener and returns its port + a cleanup. Used to
// force a "new port already in use" condition for rollback tests.
func squatTCP(t *testing.T, host string) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatalf("squat listen: %v", err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return n, func() { _ = ln.Close() }
}

// portOf parses "host:port" (or "[::]:port") and returns the port. Returns
// 0 on malformed input so tests fail loudly instead of silently passing.
func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}
