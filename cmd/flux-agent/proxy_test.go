package main

import (
	"testing"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
)

func TestPatroniProbeClients(t *testing.T) {
	cfg := &config.Config{
		SSLEnabled:         true,
		HostPatroniAPIPort: 22481,
		PatroniAPIPort:     8008,
	}
	clients := newPatroniProbeClients(cfg)
	if clients.local == nil || clients.remote == nil {
		t.Fatal("expected local and remote clients")
	}
	if clients.local == clients.remote {
		t.Fatal("expected distinct clients when host and container ports differ")
	}
	if clients.local.Port != 8008 || clients.remote.Port != 22481 {
		t.Fatalf("client ports = %d/%d, want 8008/22481", clients.local.Port, clients.remote.Port)
	}

	samePortCfg := &config.Config{
		SSLEnabled:         true,
		HostPatroniAPIPort: 8008,
		PatroniAPIPort:     8008,
	}
	sameClients := newPatroniProbeClients(samePortCfg)
	if sameClients.local != sameClients.remote {
		t.Fatal("expected shared client when host and container ports match")
	}
}

func TestPatroniProbeTarget(t *testing.T) {
	cfg := &config.Config{
		MyIP:               "80.72.20.162",
		HostPatroniAPIPort: 22481,
		PatroniAPIPort:     8008,
	}

	host, port := patroniProbeTarget(cfg, "80.72.20.162")
	if host != "127.0.0.1" || port != 8008 {
		t.Fatalf("local probe = %q:%d, want 127.0.0.1:8008", host, port)
	}

	host, port = patroniProbeTarget(cfg, "94.60.150.175")
	if host != "94.60.150.175" || port != 22481 {
		t.Fatalf("remote probe = %q:%d, want 94.60.150.175:22481", host, port)
	}
}

func TestCandidateIPs(t *testing.T) {
	cfg := &config.Config{
		EtcdHosts: "80.72.20.160:59121,80.72.20.162:59121,94.60.150.175:59121",
	}
	got := candidateIPs(cfg)
	want := []string{"80.72.20.160", "80.72.20.162", "94.60.150.175"}
	if len(got) != len(want) {
		t.Fatalf("candidateIPs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidateIPs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
