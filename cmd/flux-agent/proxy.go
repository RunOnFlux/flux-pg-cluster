package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
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

	pc := patroni.New(cfg.SSLEnabled, cfg.HostPatroniAPIPort)

	// Atomic primary IP - updated by health checker, read by acceptor.
	var primaryIP atomic.Value
	primaryIP.Store("") // empty until first probe finds one

	// Health-check goroutine: probes all known IPs to find primary.
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.ProxyHealthInterval) * time.Second)
		defer ticker.Stop()
		// Initial probe immediately
		probePrimary(ctx, pc, cfg, &primaryIP)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				probePrimary(ctx, pc, cfg, &primaryIP)
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
		go handleProxyConn(conn, primaryIP.Load().(string), *targetPort)
	}
}

// probePrimary contacts every candidate IP and updates primaryIP atomically.
// Candidates come from /etc/cluster_env (CLUSTER_IPS derived) plus FluxAPI;
// for simplicity we currently use the IPs encoded in ETCD_HOSTS.
func probePrimary(ctx context.Context, pc *patroni.Client, cfg *config.Config, primaryIP *atomic.Value) {
	ips := candidateIPs(cfg)
	if len(ips) == 0 {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex
	found := ""
	for _, ip := range ips {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			role, _ := pc.CheckRole(pctx, ip)
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
func handleProxyConn(client net.Conn, primaryIP string, targetPort int) {
	defer client.Close()
	if primaryIP == "" {
		pkglog.Warnf("proxy: rejecting connection from %s — no primary known yet", client.RemoteAddr())
		return
	}
	target := fmt.Sprintf("%s:%d", primaryIP, targetPort)
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		pkglog.Warnf("proxy: dial %s: %v", target, err)
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}
