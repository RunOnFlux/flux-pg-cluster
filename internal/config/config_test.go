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
