package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
	pkglog "github.com/RunOnFlux/flux-pg-cluster/internal/log"
	"github.com/RunOnFlux/flux-pg-cluster/internal/patroni"
)

// runProxy starts a TCP proxy listener that forwards every connection to the
// IP of the current Patroni primary. A background goroutine polls all known
// nodes' Patroni REST API to keep the target IP current. When the primary
// changes, in-flight connections are NOT killed (we don't want to disrupt
// transactions) but new connections route to the new primary.
func runProxy(args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	listenAddr := fs.String("listen", "", "listen address (default :PROXY_LISTEN_PORT)")
	targetPort := fs.Int("target-port", 0, "PostgreSQL port on primary (default HOST_POSTGRES_PORT)")
	_ = fs.Parse(args)

	cfg := config.FromEnv()
	if err := config.LoadClusterEnv(cfg); err != nil {
		pkglog.Warnf("proxy: could not load %s (will rely on env): %v", config.ClusterEnvFile, err)
	}

	if !cfg.ProxyEnabled {
		pkglog.Infof("proxy: PROXY_ENABLED=false — skipping proxy startup")
		return
	}

	if *listenAddr == "" {
		*listenAddr = fmt.Sprintf(":%d", cfg.ProxyListenPort)
	}
	if *targetPort == 0 {
		*targetPort = cfg.HostPostgresPort
	}

	pkglog.Section("FLUX-AGENT PROXY STARTING")
	pkglog.Infof("listen=%s target_port=%d patroni_port=%d ssl=%v",
		*listenAddr, *targetPort, cfg.HostPatroniAPIPort, cfg.SSLEnabled)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Atomic primary IP - updated by health checker, read by acceptor.
	var primaryIP atomic.Value
	primaryIP.Store("") // empty until first probe finds one

	// Health-check goroutine: probes all known IPs to find primary.
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.ProxyHealthInterval) * time.Second)
		defer ticker.Stop()
		// Initial probe immediately
		probePrimary(ctx, cfg, &primaryIP)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				probePrimary(ctx, cfg, &primaryIP)
			}
		}
	}()

	// TCP listener
	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		pkglog.Fatalf("proxy: listen %s: %v", *listenAddr, err)
	}
	pkglog.Infof("proxy: accepting connections on %s", *listenAddr)

	// Signal handling: graceful shutdown
	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		<-sigc
		pkglog.Infof("proxy: shutdown signal received")
		_ = listener.Close()
		cancel()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				pkglog.Warnf("proxy: accept: %v", err)
				continue
			}
		}
		go handleProxyConn(conn, primaryIP.Load().(string), cfg, *targetPort)
	}
}

// probePrimary contacts every candidate IP and updates primaryIP atomically.
// Membership is refreshed from ETCD_HOSTS in /etc/cluster_env each cycle via a
// local config copy so the shared startup cfg used by the accept loop is never
// mutated concurrently.
func probePrimary(ctx context.Context, cfg *config.Config, primaryIP *atomic.Value) {
	probeCfg := *cfg
	if err := config.LoadClusterEnv(&probeCfg); err != nil {
		pkglog.Warnf("proxy: could not reload %s: %v", config.ClusterEnvFile, err)
	}
	ips := candidateIPs(&probeCfg)
	if len(ips) == 0 {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	clients := newPatroniProbeClients(&probeCfg)

	var wg sync.WaitGroup
	var mu sync.Mutex
	found := ""
	for _, ip := range ips {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			role := checkPatroniRole(pctx, &probeCfg, ip, clients)
			if role == patroni.RolePrimary {
				mu.Lock()
				found = ip
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()

	prev, _ := primaryIP.Load().(string)
	if found != "" && found != prev {
		pkglog.Infof("proxy: primary changed: %q -> %q", prev, found)
		primaryIP.Store(found)
	} else if found == "" && prev != "" {
		pkglog.Warnf("proxy: no primary currently reachable (keeping last known: %q)", prev)
	} else if found == "" && prev == "" {
		pkglog.Warnf("proxy: no primary found among candidates %v", ips)
	}
}

// patroniProbeClients holds reusable Patroni HTTP clients for one probe cycle.
// Local probes use the container Patroni port; remote probes use the host-mapped
// port. http.Client is safe for concurrent use by the per-IP probe goroutines.
type patroniProbeClients struct {
	local  *patroni.Client
	remote *patroni.Client
}

func newPatroniProbeClients(cfg *config.Config) patroniProbeClients {
	local := patroni.New(cfg.SSLEnabled, cfg.PatroniAPIPort)
	if cfg.HostPatroniAPIPort == cfg.PatroniAPIPort {
		return patroniProbeClients{local: local, remote: local}
	}
	return patroniProbeClients{
		local:  local,
		remote: patroni.New(cfg.SSLEnabled, cfg.HostPatroniAPIPort),
	}
}

// checkPatroniRole queries the Patroni REST API for the node at ip. When ip is
// this node's public address, the probe uses localhost and the container
// Patroni port to avoid hairpin NAT failures on the public IP.
func checkPatroniRole(ctx context.Context, cfg *config.Config, ip string, clients patroniProbeClients) patroni.Role {
	host, port := patroniProbeTarget(cfg, ip)
	pc := clients.remote
	if port == cfg.PatroniAPIPort {
		pc = clients.local
	}
	role, _ := pc.CheckRole(ctx, host)
	return role
}

// patroniProbeTarget returns the host and port to use when polling Patroni.
func patroniProbeTarget(cfg *config.Config, ip string) (host string, port int) {
	host = ip
	port = cfg.HostPatroniAPIPort
	if cfg.MyIP != "" && ip == cfg.MyIP {
		host = "127.0.0.1"
		port = cfg.PatroniAPIPort
	}
	return host, port
}

// candidateIPs derives the list of candidate node IPs from ETCD_HOSTS.
// Format: "IP:PORT,IP:PORT" — we split and strip ports.
func candidateIPs(cfg *config.Config) []string {
	hosts := cfg.EtcdHosts
	if hosts == "" {
		return nil
	}
	var ips []string
	for _, h := range splitCSV(hosts) {
		if i := indexByte(h, ':'); i > 0 {
			ips = append(ips, h[:i])
		} else if h != "" {
			ips = append(ips, h)
		}
	}
	return ips
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(s[i])
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// handleProxyConn dials the current primary and bidirectionally copies bytes.
// When the primary is this node, dial via localhost and the container Postgres
// port to avoid hairpin NAT failures on the public IP.
func handleProxyConn(client net.Conn, primaryIP string, cfg *config.Config, hostPostgresPort int) {
	defer client.Close()
	reader := bufio.NewReader(client)
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	prefix, err := reader.Peek(4)
	_ = client.SetReadDeadline(time.Time{})
	if err == nil && string(prefix) == "GET " {
		req, readErr := http.ReadRequest(reader)
		if readErr != nil {
			return
		}
		defer req.Body.Close()
		writeCurrentIdentityResponse(&connResponseWriter{Conn: client}, cfg, req, config.LoadClusterEnv)
		return
	}
	if primaryIP == "" {
		pkglog.Warnf("proxy: rejecting connection from %s — no primary known yet", client.RemoteAddr())
		return
	}
	dialHost := primaryIP
	dialPort := hostPostgresPort
	if cfg.MyIP != "" && primaryIP == cfg.MyIP {
		dialHost = "127.0.0.1"
		dialPort = cfg.PostgresPort
	}
	target := fmt.Sprintf("%s:%d", dialHost, dialPort)
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		pkglog.Warnf("proxy: dial %s: %v", target, err)
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, reader); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// writeCurrentIdentityResponse reloads the configuration snapshot used for
// authentication and membership reporting. The daemon can update cluster_env
// without restarting the proxy, so serving the startup snapshot here can make
// otherwise matching peers reject each other's probe tokens or topology.
func writeCurrentIdentityResponse(w http.ResponseWriter, cfg *config.Config, r *http.Request,
	loadClusterEnv func(*config.Config) error) {
	current := *cfg
	if err := loadClusterEnv(&current); err != nil {
		pkglog.Warnf("proxy: could not refresh %s for identity probe: %v", config.ClusterEnvFile, err)
	}
	writeIdentityResponse(w, &current, r)
}

// connResponseWriter is the minimal http.ResponseWriter needed to serve the
// identity document on the existing TCP proxy listener.
type connResponseWriter struct {
	net.Conn
	header      http.Header
	wroteHeader bool
}

func (w *connResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *connResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	_, _ = fmt.Fprintf(w.Conn, "HTTP/1.1 %d %s\r\n", statusCode, http.StatusText(statusCode))
	for key, values := range w.Header() {
		for _, value := range values {
			_, _ = fmt.Fprintf(w.Conn, "%s: %s\r\n", key, value)
		}
	}
	_, _ = io.WriteString(w.Conn, "Connection: close\r\n\r\n")
}

func (w *connResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.Conn.Write(p)
}
