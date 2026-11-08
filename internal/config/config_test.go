package config

import (
	"strings"
	"testing"
)

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

func TestProductionBootstrapDefaultsAndIdentitySettings(t *testing.T) {
	t.Setenv("AUTO_BOOTSTRAP_IF_FRESH", "")
	t.Setenv("ALLOW_NEW_CLUSTER_BOOTSTRAP", "")
	t.Setenv("EXPECTED_POSTGRES_SYSTEM_ID", "7658016226426196166")
	cfg := FromEnv()
	if cfg.AutoBootstrapIfFresh || cfg.AllowNewClusterBootstrap {
		t.Fatal("normal startup must not authorize creation")
	}
	if cfg.ExpectedSystemID != "7658016226426196166" {
		t.Fatal("deployment identity not loaded")
	}
	cfg.applyKV("CLUSTER_SEED_IPS", "10.0.0.1,10.0.0.2")
	cfg.applyKV("RETIRED_CLUSTER_IPS", "10.0.0.3")
	if cfg.ClusterSeedIPs != "10.0.0.1,10.0.0.2" || cfg.RetiredClusterIPs != "10.0.0.3" {
		t.Fatal("membership settings not loaded")
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

func TestClusterEnvPasswordsAreShellQuotedAndDecoded(t *testing.T) {
	tests := []string{
		"IRwMwFIW&i8XeZf0mMF5",
		"mA%@DS1dL6H-=aqii*E",
		"quote'safe;still-one-value",
	}
	for _, password := range tests {
		encoded := shellQuoteValue(password)
		if !strings.HasPrefix(encoded, "'") || !strings.HasSuffix(encoded, "'") {
			t.Fatalf("password was not shell quoted: %q", encoded)
		}
		if got := decodeShellValue(encoded); got != password {
			t.Fatalf("decoded password = %q, want %q", got, password)
		}

		cfg := &Config{}
		cfg.applyKV("POSTGRES_SUPERUSER_PASSWORD", decodeShellValue(encoded))
		if cfg.PostgresSuperuserPassword != password {
			t.Fatalf("loaded password = %q, want %q", cfg.PostgresSuperuserPassword, password)
		}
	}
}

func TestDecodeShellValueSupportsLegacyUnquotedFiles(t *testing.T) {
	const legacy = "plain-password"
	if got := decodeShellValue(legacy); got != legacy {
		t.Fatalf("legacy value = %q, want %q", got, legacy)
	}
}
