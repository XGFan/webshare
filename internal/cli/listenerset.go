package cli

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/guofan/webshare-proxy/internal/auth"
	"github.com/guofan/webshare-proxy/internal/listener"
	"github.com/guofan/webshare-proxy/internal/registry"
	"github.com/guofan/webshare-proxy/internal/tunnel"
)

// listenerSet is the HTTP+SOCKS5 listener lifecycle manager. It supports:
//   - explicit Start / Stop (req #1 user-initiated toggle)
//   - bind-then-swap Reconfigure so a port-change failure leaves the OLD
//     listener intact (req #3 "新端口被占用 → 旧端口不释放")
//   - idempotent Stop that releases sockets fully (req #4 "不要不释放未使用端口")
//
// All transitions take the same mutex; concurrent Start/Stop/Reconfigure
// calls serialize cleanly.
type listenerSet struct {
	mu       sync.Mutex
	mgr      *tunnel.Manager
	reg      *registry.ConnectionRegistry
	denyList *auth.DenyList

	// running is true iff httpProxy and socksProxy are bound and have a
	// Serve goroutine alive.
	running bool

	httpProxy  *listener.HTTPProxy
	socksProxy *listener.SOCKS5Proxy

	httpDone  chan struct{}
	socksDone chan struct{}
}

// Start binds both listeners on the supplied addresses and spawns their
// Serve goroutines. Idempotent on the (running && same addrs) case. If
// either bind fails, neither port is left bound.
func (l *listenerSet) Start(ctx context.Context, httpBind string, httpPort int, socksBind string, socksPort int) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.running {
		// Already running on these addrs → no-op. On different addrs,
		// callers should call Reconfigure instead (which handles the
		// bind-then-swap path with rollback).
		if l.httpProxy.Addr() == fmt.Sprintf("%s:%d", httpBind, httpPort) &&
			l.socksProxy.Addr() == fmt.Sprintf("%s:%d", socksBind, socksPort) {
			return nil
		}
		return fmt.Errorf("listenerSet: already running on different addresses; use Reconfigure")
	}

	hp, sp, err := bindPair(l.mgr, l.reg, l.denyList, httpBind, httpPort, socksBind, socksPort)
	if err != nil {
		return err
	}
	l.httpProxy = hp
	l.socksProxy = sp
	l.spawnServeLocked(ctx)
	l.running = true
	return nil
}

// Stop closes the listeners (releasing the kernel sockets) and waits for
// their Serve goroutines to exit. Safe to call when already stopped.
func (l *listenerSet) Stop() {
	l.mu.Lock()
	hp := l.httpProxy
	sp := l.socksProxy
	httpDone := l.httpDone
	socksDone := l.socksDone
	l.running = false
	l.httpProxy = nil
	l.socksProxy = nil
	l.httpDone = nil
	l.socksDone = nil
	l.mu.Unlock()

	if hp != nil {
		_ = hp.Close()
	}
	if sp != nil {
		_ = sp.Close()
	}
	if httpDone != nil {
		<-httpDone
	}
	if socksDone != nil {
		<-socksDone
	}
}

// Reconfigure attempts to swap the live listeners to the new addresses.
//
// Two strategies:
//   - Different ports → bind-then-close: bind NEW first, then close OLD. If
//     the new bind fails, the OLD listeners stay running and the error is
//     returned (req #3 "新端口被占用 → 旧端口不释放").
//   - Same-port (bind changes only, e.g. 127.0.0.1:8080 → 0.0.0.0:8080) →
//     close-then-bind with rollback: bind-first would always fail because
//     0.0.0.0 collides with 127.0.0.1 on the same port. We close old, try
//     new; if new fails we attempt to restore the old bind so the user
//     keeps the previously-working listener.
//
// When the set is currently stopped this is a no-op + nil — settings are
// persisted by the caller; the next Start uses them.
func (l *listenerSet) Reconfigure(ctx context.Context, httpBind string, httpPort int, socksBind string, socksPort int) error {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return nil
	}
	newHTTPAddr := fmt.Sprintf("%s:%d", httpBind, httpPort)
	newSocksAddr := fmt.Sprintf("%s:%d", socksBind, socksPort)
	// No-change short-circuit: avoid a needless rebind cycle that would
	// drop any in-flight connections.
	if l.httpProxy.Addr() == newHTTPAddr && l.socksProxy.Addr() == newSocksAddr {
		l.mu.Unlock()
		return nil
	}
	oldHTTP := l.httpProxy
	oldSocks := l.socksProxy
	oldHTTPDone := l.httpDone
	oldSocksDone := l.socksDone
	oldHTTPHost, oldHTTPPort := splitHostPort(l.httpProxy.Addr())
	oldSocksHost, oldSocksPort := splitHostPort(l.socksProxy.Addr())
	l.mu.Unlock()

	// If any new port collides with an old port (same number on either
	// listener), bind-then-close is impossible: kernel sees the old socket
	// holding the port and rejects the new bind, even when the bind
	// address differs (0.0.0.0 vs 127.0.0.1 share the port).
	portCollision := httpPort == oldHTTPPort || httpPort == oldSocksPort ||
		socksPort == oldHTTPPort || socksPort == oldSocksPort

	if portCollision {
		// Close-then-bind. Drop accept loops first so the kernel releases
		// the ports, then try the new bind. On failure, restore the old
		// bind so the user keeps a working proxy.
		_ = oldHTTP.Close()
		_ = oldSocks.Close()
		if oldHTTPDone != nil {
			<-oldHTTPDone
		}
		if oldSocksDone != nil {
			<-oldSocksDone
		}
		hp, sp, err := bindPair(l.mgr, l.reg, l.denyList, httpBind, httpPort, socksBind, socksPort)
		if err != nil {
			ohp, osp, restErr := bindPair(l.mgr, l.reg, l.denyList, oldHTTPHost, oldHTTPPort, oldSocksHost, oldSocksPort)
			if restErr != nil {
				// Both new and rollback bind failed (someone else grabbed
				// the old port in the gap). Listener is now stopped.
				l.mu.Lock()
				l.running = false
				l.httpProxy = nil
				l.socksProxy = nil
				l.httpDone = nil
				l.socksDone = nil
				l.mu.Unlock()
				return fmt.Errorf("%w (rollback to %s / %s also failed: %v)",
					err, oldHTTP.Addr(), oldSocks.Addr(), restErr)
			}
			l.mu.Lock()
			l.httpProxy = ohp
			l.socksProxy = osp
			l.spawnServeLocked(ctx)
			l.mu.Unlock()
			return err
		}
		l.mu.Lock()
		l.httpProxy = hp
		l.socksProxy = sp
		l.spawnServeLocked(ctx)
		l.mu.Unlock()
		return nil
	}

	// Different ports: bind NEW before closing OLD. bindPair takes no
	// instance locks so we don't deadlock; on failure neither new port is
	// left bound and the old listeners keep accepting.
	hp, sp, err := bindPair(l.mgr, l.reg, l.denyList, httpBind, httpPort, socksBind, socksPort)
	if err != nil {
		return err
	}

	// Bind succeeded — swap in new, close old. Lock around the pointer
	// swap so Status()/HTTPAddr() callers never see a half-state.
	l.mu.Lock()
	l.httpProxy = hp
	l.socksProxy = sp
	l.spawnServeLocked(ctx)
	l.mu.Unlock()

	_ = oldHTTP.Close()
	_ = oldSocks.Close()
	if oldHTTPDone != nil {
		<-oldHTTPDone
	}
	if oldSocksDone != nil {
		<-oldSocksDone
	}
	return nil
}

// splitHostPort extracts host and port from a "host:port" string. Returns
// ("", 0) on parse failure — callers use the values to attempt a rebind, so
// a malformed addr just means "we don't know the old binding" and the
// rollback will surface as a bind error.
func splitHostPort(addr string) (string, int) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0
	}
	port, _ := strconv.Atoi(p)
	return h, port
}

// Status returns the current running flag and the bound addresses (empty
// strings when stopped). Snapshots under the mutex so all three values are
// consistent.
func (l *listenerSet) Status() (running bool, httpAddr, socksAddr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running {
		return false, "", ""
	}
	return true, l.httpProxy.Addr(), l.socksProxy.Addr()
}

// spawnServeLocked must be called with l.mu held. Replaces any stale
// httpDone/socksDone with fresh channels and starts the Serve loops.
func (l *listenerSet) spawnServeLocked(ctx context.Context) {
	hp := l.httpProxy
	sp := l.socksProxy
	httpDone := make(chan struct{})
	socksDone := make(chan struct{})
	l.httpDone = httpDone
	l.socksDone = socksDone
	go func() {
		defer close(httpDone)
		_ = hp.Serve(ctx)
	}()
	go func() {
		defer close(socksDone)
		_ = sp.Serve(ctx)
	}()
}

// bindPair constructs and Bind()s an HTTP+SOCKS5 listener pair. On any
// failure both new sockets are closed before the error is returned, so
// no port is leaked.
func bindPair(mgr *tunnel.Manager, reg *registry.ConnectionRegistry, denyList *auth.DenyList,
	httpBind string, httpPort int, socksBind string, socksPort int,
) (*listener.HTTPProxy, *listener.SOCKS5Proxy, error) {
	hp := listener.NewHTTPProxy(fmt.Sprintf("%s:%d", httpBind, httpPort), mgr, reg, denyList, nil)
	if err := hp.Bind(); err != nil {
		return nil, nil, fmt.Errorf("http listener bind %s:%d: %w", httpBind, httpPort, err)
	}
	sp := listener.NewSOCKS5Proxy(fmt.Sprintf("%s:%d", socksBind, socksPort), mgr, reg, denyList, nil)
	if err := sp.Bind(); err != nil {
		_ = hp.Close()
		return nil, nil, fmt.Errorf("socks5 listener bind %s:%d: %w", socksBind, socksPort, err)
	}
	return hp, sp, nil
}
