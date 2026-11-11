package patroni

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Accept TCP but never speak TLS, as an exhausted Patroni worker pool does.
func silentTLSPeer(t *testing.T) (string, int, func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	conns := make(map[net.Conn]struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns[c] = struct{}{}
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
				mu.Lock()
				delete(conns, c)
				mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(port)
	return host, n, func() int { mu.Lock(); defer mu.Unlock(); return len(conns) }
}

func waitConnections(t *testing.T, count func() int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if count() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d TLS connections remain after timed-out probes", count())
}

func TestCancelledProbesReleaseDetachedTLSHandshakes(t *testing.T) {
	host, port, count := silentTLSPeer(t)
	for range 6 {
		client := New(true, port)
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		if _, err := client.GetInfo(ctx, host); err == nil {
			t.Fatal("silent TLS peer unexpectedly responded")
		}
		cancel()
		// Deliberately omit Close: even an abandoned caller must be bounded by the
		// handshake timeout. This reproduced the production leak before the fix.
	}
	waitConnections(t, count, 3*time.Second)
}

func TestCloseCancelsDiscardedTransportDial(t *testing.T) {
	host, port, count := silentTLSPeer(t)
	client := New(true, port)
	// Isolate lifecycle cleanup from the independent handshake deadline.
	client.HTTP.Transport.(*http.Transport).TLSHandshakeTimeout = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, _ = client.GetInfo(ctx, host)
	cancel()
	client.Close()
	waitConnections(t, count, time.Second)
}

func TestProbeRolesAndIdentityWithBoundedTransport(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/primary":
			w.WriteHeader(http.StatusOK)
		case "/patroni":
			_, _ = io.WriteString(w, `{"role":"primary","state":"running","database_system_identifier":"7658016226426196166"}`)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	client := New(true, n)
	defer client.Close()
	role, err := client.CheckRole(context.Background(), host)
	if err != nil || role != RolePrimary {
		t.Fatalf("role=%s err=%v", role, err)
	}
	info, err := client.GetInfo(context.Background(), host)
	if err != nil || info.DatabaseSystemIdentifier != "7658016226426196166" || info.State != "running" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}
