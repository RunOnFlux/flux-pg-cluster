package config

import "testing"

func TestHostProxyPortDefaultsToMappedPortAfterPostgres(t *testing.T) {
	t.Setenv("HOST_POSTGRES_PORT", "15432")
	t.Setenv("POSTGRES_PORT", "5432")
	t.Setenv("PROXY_LISTEN_PORT", "5433")
	t.Setenv("HOST_PROXY_PORT", "")
	cfg := FromEnv()
	if cfg.HostProxyPort != 15433 {
		t.Fatalf("HostProxyPort = %d, want 15433", cfg.HostProxyPort)
	}
}

func TestHostProxyPortExplicitOverride(t *testing.T) {
	t.Setenv("HOST_POSTGRES_PORT", "15432")
	t.Setenv("HOST_PROXY_PORT", "25433")
	cfg := FromEnv()
	if cfg.HostProxyPort != 25433 {
		t.Fatalf("HostProxyPort = %d, want 25433", cfg.HostProxyPort)
	}
}
