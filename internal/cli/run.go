package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/guofan/webshare-proxy/internal/api"
	"github.com/guofan/webshare-proxy/internal/auth"
	"github.com/guofan/webshare-proxy/internal/registry"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/routing"
	syncpkg "github.com/guofan/webshare-proxy/internal/sync"
	"github.com/guofan/webshare-proxy/internal/tunnel"
	"github.com/guofan/webshare-proxy/internal/web"
	"github.com/guofan/webshare-proxy/internal/ws"
)

// runDaemon is the long-running 'run' subcommand: opens DB, builds routing,
// starts HTTP+SOCKS5 listeners + sync loop + REST/WS API, blocks on ctx.
func runDaemon(ctx context.Context, deps Deps, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(deps.Stderr)
	dataDir := fs.String("data-dir", "", "override the default data directory")
	webBind := fs.String("web-bind", "", "if non-empty, also serve the web admin panel on this addr (e.g. 0.0.0.0:9090)")
	webPassword := fs.String("web-password", "", "password for the web admin panel; alternatively set $WEBSHARE_WEB_PASSWORD")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *webBind != "" {
		if envPwd := os.Getenv("WEBSHARE_WEB_PASSWORD"); envPwd != "" {
			*webPassword = envPwd
		}
		if *webPassword == "" {
			fmt.Fprintln(deps.Stderr, "run: --web-bind requires --web-password (or $WEBSHARE_WEB_PASSWORD)")
			return 2
		}
	}
	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "data dir: %v\n", err)
		return 1
	}
	db, masterKey, err := openDB(ctx, dir)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "open: %v\n", err)
		return 1
	}
	defer db.Close()

	settings, err := repo.LoadSettings(ctx, db)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "settings: %v\n", err)
		return 1
	}
	// Env-var overrides for declarative deployments (K8s, systemd). When
	// set, they win over the persisted values and are written back to the
	// DB so the web UI reflects the actual listener state.
	if v := os.Getenv("WEBSHARE_PROXY_BIND"); v != "" {
		settings.HTTPListenerBind = v
		settings.SOCKS5ListenerBind = v
	}
	if v := os.Getenv("WEBSHARE_PROXY_AUTOSTART"); v == "true" || v == "1" {
		settings.ProxyEnabled = true
	}
	if err := repo.UpdateSettings(ctx, db, settings); err != nil {
		fmt.Fprintf(deps.Stderr, "settings persist: %v\n", err)
		return 1
	}
	core := routing.NewCore(db, masterKey)
	if err := core.Hydrate(ctx); err != nil {
		fmt.Fprintf(deps.Stderr, "hydrate: %v\n", err)
		return 1
	}
	mgr := tunnel.New(core)
	reg := registry.New()
	hub := ws.NewHub(slog.Default())
	defer hub.Close()
	denyList := auth.New(nil)

	// LAN bind warnings (US-019 spec). Single stderr line per non-loopback bind.
	for _, b := range []struct{ proto, bind string; port int }{
		{"http", settings.HTTPListenerBind, settings.HTTPListenerPort},
		{"socks5", settings.SOCKS5ListenerBind, settings.SOCKS5ListenerPort},
	} {
		if !isLoopback(b.bind) {
			fmt.Fprintf(deps.Stderr,
				"WARNING: %s listener bound to %s:%d is reachable from LAN; clients on the LAN can use this proxy if they have valid credentials.\n",
				b.proto, b.bind, b.port)
		}
	}

	// Listener set: state machine for the HTTP+SOCKS5 listeners. The
	// settings PUT handler swaps ports via Reconfigure (bind-then-close);
	// explicit Start/Stop endpoints flip running state. Daemon doesn't bind
	// here — startBound below uses runCtx so the Serve goroutines can be
	// cancelled by daemon shutdown.
	lset := &listenerSet{mgr: mgr, reg: reg, denyList: denyList}

	syncSvc := syncpkg.NewService(db, masterKey, syncpkg.DefaultFetcherFactory)

	// Background sync loop: one goroutine per ApiKey row, ticking at
	// settings.SyncIntervalMinutes (min 5 minutes guard).
	interval := time.Duration(settings.SyncIntervalMinutes) * time.Minute
	if interval < 5*time.Minute {
		interval = 60 * time.Minute
	}

	apiSrv := api.New(api.Deps{
		DB: db, MasterKey: masterKey, Core: core, Registry: reg, Hub: hub,
		DenyList: denyList, DataDir: dir,
		SyncKey: func(ctx context.Context, keyID int64) error {
			err := syncSvc.SyncKey(ctx, keyID)
			if err == nil {
				_ = core.RebuildAfterSync(ctx, func(u, oldUp string) {
					reg.CloseByUserUpstream(u, oldUp)
					hub.Broadcast("mapping_broken", map[string]any{"username": u, "old_upstream_id": oldUp})
				})
			}
			return err
		},
		ReconfigureListeners: nil, // wired below once runCtx exists
		StartProxy:           nil, // wired below
		StopProxy:            nil, // wired below
		ProxyStatus:          nil, // wired below
		ShutdownFn:           nil, // wired below once shutdownCh is in scope
	})

	port, err := apiSrv.Bind("127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(deps.Stderr, "api bind: %v\n", err)
		return 1
	}
	if err := apiSrv.WriteAPIPortFile(); err != nil {
		fmt.Fprintf(deps.Stderr, "write api.port: %v\n", err)
		return 1
	}
	_ = repo.SaveAPIPort(ctx, db, port)

	// Single ctx for all subsystems. SIGTERM, SIGINT, or POST /shutdown cancels.
	runCtx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	// Honor persisted proxy_enabled on boot. A bind failure here is logged
	// but NOT fatal — the daemon keeps the REST surface up so the UI can
	// show the error and let the user pick a different port.
	if settings.ProxyEnabled {
		if err := lset.Start(runCtx, settings.HTTPListenerBind, settings.HTTPListenerPort,
			settings.SOCKS5ListenerBind, settings.SOCKS5ListenerPort); err != nil {
			fmt.Fprintf(deps.Stderr, "startup proxy bind failed (proxy stays stopped): %v\n", err)
		}
	}

	running, httpAddr, socksAddr := lset.Status()
	fmt.Fprintf(deps.Stdout, "webshare-proxyd run: api=127.0.0.1:%d  proxy_running=%v  http=%s  socks5=%s\n",
		port, running, httpAddr, socksAddr)

	apiSrv.Deps().ShutdownFn = cancelAll
	apiSrv.Deps().ReconfigureListeners = func() error {
		latest, err := repo.LoadSettings(context.Background(), db)
		if err != nil {
			return err
		}
		return lset.Reconfigure(runCtx, latest.HTTPListenerBind, latest.HTTPListenerPort,
			latest.SOCKS5ListenerBind, latest.SOCKS5ListenerPort)
	}
	apiSrv.Deps().StartProxy = func() error {
		latest, err := repo.LoadSettings(context.Background(), db)
		if err != nil {
			return err
		}
		if err := lset.Start(runCtx, latest.HTTPListenerBind, latest.HTTPListenerPort,
			latest.SOCKS5ListenerBind, latest.SOCKS5ListenerPort); err != nil {
			return err
		}
		return repo.SetProxyEnabled(context.Background(), db, true)
	}
	apiSrv.Deps().StopProxy = func() error {
		lset.Stop()
		return repo.SetProxyEnabled(context.Background(), db, false)
	}
	apiSrv.Deps().ProxyStatus = func() (bool, string, string) {
		return lset.Status()
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		cancelAll()
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = apiSrv.Serve(runCtx) }()

	// Optional LAN web admin panel. Lives on its own listener with cookie
	// auth in front of the same /api/v1/* handler set the Mac app uses on
	// loopback. Bind failure here is fatal — the operator asked for the
	// listener and a partial bring-up would be confusing.
	if *webBind != "" {
		webSrv, err := web.New(web.Options{
			Bind:       *webBind,
			Password:   *webPassword,
			APIHandler: apiSrv.Handler(),
		})
		if err != nil {
			fmt.Fprintf(deps.Stderr, "web: %v\n", err)
			cancelAll()
			return 1
		}
		webPort, err := webSrv.Bind()
		if err != nil {
			fmt.Fprintf(deps.Stderr, "web bind %s: %v\n", *webBind, err)
			cancelAll()
			return 1
		}
		if !web.IsLoopbackBind(*webBind) {
			fmt.Fprintf(deps.Stderr,
				"WARNING: web admin panel bound to %s is reachable from LAN; anyone on the LAN can attempt the password challenge.\n",
				*webBind)
		}
		fmt.Fprintf(deps.Stdout, "webshare-proxyd web: http://%s (port=%d)\n", *webBind, webPort)
		wg.Add(1)
		go func() { defer wg.Done(); _ = webSrv.Serve(runCtx) }()
	}

	// Periodic sync goroutine: scans api_keys, kicks syncSvc on each
	// active key at the configured cadence.
	syncTickerDone := make(chan struct{})
	go func() {
		defer close(syncTickerDone)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				keys, err := repo.ListApiKeys(runCtx, db)
				if err != nil {
					continue
				}
				for _, k := range keys {
					if !k.Active {
						continue
					}
					_ = syncSvc.SyncKey(runCtx, k.ID)
				}
				_ = core.RebuildAfterSync(runCtx, func(u, oldUp string) {
					reg.CloseByUserUpstream(u, oldUp)
					hub.Broadcast("mapping_broken", map[string]any{"username": u, "old_upstream_id": oldUp})
				})
			}
		}
	}()

	<-runCtx.Done()
	hub.Broadcast("shutting_down", nil)
	// Listener accept loops exit when runCtx is done (Serve's inner watcher
	// closes their sockets). 5s grace, then force-close remaining conns.
	graceTimer := time.NewTimer(5 * time.Second)
	defer graceTimer.Stop()
	select {
	case <-allClosed(reg):
	case <-graceTimer.C:
	}
	// Final hammer: force-close every conn by (user, upstream). The
	// snapshot loop is the only public surface; CloseByUserUpstream
	// iterates the registry's index for each pair.
	for _, c := range reg.SnapshotAll() {
		_ = reg.CloseByUserUpstream(c.Username, c.UpstreamID)
	}

	wg.Wait()
	lset.Stop() // release listener sockets cleanly on shutdown
	<-syncTickerDone
	return 0
}

// isLoopback returns true for 127.0.0.1, ::1, or "localhost".
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// allClosed returns a channel that closes once the registry's connection
// count reaches zero. Polled every 100ms.
func allClosed(r *registry.ConnectionRegistry) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			if r.Len() == 0 {
				close(out)
				return
			}
		}
	}()
	return out
}
