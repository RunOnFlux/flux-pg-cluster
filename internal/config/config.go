// Package config loads cluster configuration from environment variables and
// the /etc/cluster_env file written by `flux-agent init`.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const ClusterEnvFile = "/etc/cluster_env"

// Config aggregates all cluster-wide settings used by the agent subcommands.
type Config struct {
	AppName  string
	MyName   string
	MyIP     string
	HostName string

	PostgresDB                  string
	PostgresSuperuserPassword   string
	PostgresReplicationPassword string

	SSLEnabled           bool
	SSLPassphrase        string
	SSLCertValidityDays  int

	HostPostgresPort    int
	HostPatroniAPIPort  int
	HostEtcdClientPort  int
	HostEtcdPeerPort    int
	PostgresPort        int
	PatroniAPIPort      int
	EtcdClientPort      int
	EtcdPeerPort        int

	EtcdHosts           string // comma-separated IP:port pairs (no scheme)
	EtcdInitialCluster  string // comma-separated name=URL pairs
	ClusterIPs          []string

	AllowNewClusterBootstrap     bool
	AllowAnyNodeBootstrap        bool
	AutoBootstrapIfFresh         bool
	DeadClusterRecovery          bool
	EtcdJoinMaxRetries           int
	EtcdJoinRetryDelaySeconds    int
	UpdateIntervalSeconds        int
	DesiredStateStabilityCycles  int
	EtcdUnavailableRecoveryCycles int

	PatroniTTL                  int
	PatroniLoopWait             int
	PatroniRetryTimeout         int
	PatroniMaxLag               int
	PatroniMasterStartTimeout   int
	PatroniMasterStopTimeout    int
	PatroniUseSlots             bool
	PatroniSynchronousMode      bool
	PatroniSynchronousModeStrict bool
	PatroniSynchronousNodeCount int

	FluxAPIURL string

	// Proxy
	ProxyEnabled        bool
	ProxyListenPort     int
	ProxyHealthInterval int
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := env(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(env(key, ""))
	if v == "" {
		return def
	}
	return v == "true" || v == "1" || v == "yes" || v == "on"
}

// FromEnv loads a Config from current environment variables, applying defaults
// matching entrypoint.sh.
func FromEnv() *Config {
	c := &Config{
		AppName:                       env("APP_NAME", "postgres-cluster"),
		PostgresDB:                    env("POSTGRES_DB", "postgres"),
		PostgresSuperuserPassword:     env("POSTGRES_SUPERUSER_PASSWORD", "postgres"),
		PostgresReplicationPassword:   env("POSTGRES_REPLICATION_PASSWORD", "replication"),
		SSLEnabled:                    envBool("SSL_ENABLED", false),
		SSLPassphrase:                 env("SSL_PASSPHRASE", ""),
		SSLCertValidityDays:           envInt("SSL_CERT_VALIDITY_DAYS", 3650),
		HostPostgresPort:              envInt("HOST_POSTGRES_PORT", 5432),
		HostPatroniAPIPort:            envInt("HOST_PATRONI_API_PORT", 8008),
		HostEtcdClientPort:            envInt("HOST_ETCD_CLIENT_PORT", 2379),
		HostEtcdPeerPort:              envInt("HOST_ETCD_PEER_PORT", 2380),
		PostgresPort:                  envInt("POSTGRES_PORT", 5432),
		PatroniAPIPort:                envInt("PATRONI_API_PORT", 8008),
		EtcdClientPort:                envInt("ETCD_CLIENT_PORT", 2379),
		EtcdPeerPort:                  envInt("ETCD_PEER_PORT", 2380),
		AllowNewClusterBootstrap:      envBool("ALLOW_NEW_CLUSTER_BOOTSTRAP", false),
		AllowAnyNodeBootstrap:         envBool("ALLOW_ANY_NODE_BOOTSTRAP", false),
		AutoBootstrapIfFresh:          envBool("AUTO_BOOTSTRAP_IF_FRESH", true),
		DeadClusterRecovery:           envBool("DEAD_CLUSTER_RECOVERY", true),
		EtcdJoinMaxRetries:            envInt("ETCD_JOIN_MAX_RETRIES", 12),
		EtcdJoinRetryDelaySeconds:     envInt("ETCD_JOIN_RETRY_DELAY_SECONDS", 10),
		UpdateIntervalSeconds:         envInt("UPDATE_INTERVAL_SECONDS", 60),
		DesiredStateStabilityCycles:   envInt("DESIRED_STATE_STABILITY_CYCLES", 3),
		EtcdUnavailableRecoveryCycles: envInt("ETCD_UNAVAILABLE_RECOVERY_CYCLES", 2),
		PatroniTTL:                    envInt("PATRONI_TTL", 30),
		PatroniLoopWait:               envInt("PATRONI_LOOP_WAIT", 10),
		PatroniRetryTimeout:           envInt("PATRONI_RETRY_TIMEOUT", 30),
		PatroniMaxLag:                 envInt("PATRONI_MAX_LAG", 33554432),
		PatroniMasterStartTimeout:     envInt("PATRONI_MASTER_START_TIMEOUT", 300),
		PatroniMasterStopTimeout:      envInt("PATRONI_MASTER_STOP_TIMEOUT", 300),
		PatroniUseSlots:               envBool("PATRONI_USE_SLOTS", false),
		PatroniSynchronousMode:        envBool("PATRONI_SYNCHRONOUS_MODE", false),
		PatroniSynchronousModeStrict:  envBool("PATRONI_SYNCHRONOUS_MODE_STRICT", false),
		PatroniSynchronousNodeCount:   envInt("PATRONI_SYNCHRONOUS_NODE_COUNT", 1),
		FluxAPIURL:                    env("FLUX_API_URL", "https://api.runonflux.io"),
		ProxyEnabled:                  envBool("PROXY_ENABLED", true),
		ProxyListenPort:               envInt("PROXY_LISTEN_PORT", 5433),
		ProxyHealthInterval:           envInt("PROXY_HEALTH_INTERVAL_SECONDS", 3),
	}
	return c
}

// LoadClusterEnv reads /etc/cluster_env (KEY=VALUE per line) and applies the
// values to the given Config, overriding env-derived defaults. This file is
// written by `flux-agent init` so subsequent subcommands inherit consistent
// values even across container restarts.
func LoadClusterEnv(c *Config) error {
	f, err := os.Open(ClusterEnvFile)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		c.applyKV(key, val)
	}
	return scanner.Err()
}

func (c *Config) applyKV(key, val string) {
	switch key {
	case "MY_NAME":
		c.MyName = val
	case "MY_IP":
		c.MyIP = val
	case "APP_NAME":
		c.AppName = val
	case "ETCD_HOSTS":
		c.EtcdHosts = val
	case "ETCD_INITIAL_CLUSTER":
		c.EtcdInitialCluster = val
	case "SSL_ENABLED":
		c.SSLEnabled = strings.EqualFold(val, "true")
	case "POSTGRES_SUPERUSER_PASSWORD":
		c.PostgresSuperuserPassword = val
	case "POSTGRES_REPLICATION_PASSWORD":
		c.PostgresReplicationPassword = val
	case "HOST_POSTGRES_PORT":
		c.HostPostgresPort, _ = strconv.Atoi(val)
	case "HOST_PATRONI_API_PORT":
		c.HostPatroniAPIPort, _ = strconv.Atoi(val)
	case "HOST_ETCD_CLIENT_PORT":
		c.HostEtcdClientPort, _ = strconv.Atoi(val)
	case "HOST_ETCD_PEER_PORT":
		c.HostEtcdPeerPort, _ = strconv.Atoi(val)
	case "POSTGRES_PORT":
		c.PostgresPort, _ = strconv.Atoi(val)
	case "PATRONI_API_PORT":
		c.PatroniAPIPort, _ = strconv.Atoi(val)
	case "ETCD_CLIENT_PORT":
		c.EtcdClientPort, _ = strconv.Atoi(val)
	case "ETCD_PEER_PORT":
		c.EtcdPeerPort, _ = strconv.Atoi(val)
	case "ALLOW_NEW_CLUSTER_BOOTSTRAP":
		c.AllowNewClusterBootstrap = strings.EqualFold(val, "true")
	case "ALLOW_ANY_NODE_BOOTSTRAP":
		c.AllowAnyNodeBootstrap = strings.EqualFold(val, "true")
	case "AUTO_BOOTSTRAP_IF_FRESH":
		c.AutoBootstrapIfFresh = strings.EqualFold(val, "true")
	case "DEAD_CLUSTER_RECOVERY":
		c.DeadClusterRecovery = strings.EqualFold(val, "true")
	case "ETCD_JOIN_MAX_RETRIES":
		c.EtcdJoinMaxRetries, _ = strconv.Atoi(val)
	case "ETCD_JOIN_RETRY_DELAY_SECONDS":
		c.EtcdJoinRetryDelaySeconds, _ = strconv.Atoi(val)
	case "UPDATE_INTERVAL_SECONDS":
		c.UpdateIntervalSeconds, _ = strconv.Atoi(val)
	case "DESIRED_STATE_STABILITY_CYCLES":
		c.DesiredStateStabilityCycles, _ = strconv.Atoi(val)
	case "ETCD_UNAVAILABLE_RECOVERY_CYCLES":
		c.EtcdUnavailableRecoveryCycles, _ = strconv.Atoi(val)
	case "PATRONI_TTL":
		c.PatroniTTL, _ = strconv.Atoi(val)
	case "PATRONI_LOOP_WAIT":
		c.PatroniLoopWait, _ = strconv.Atoi(val)
	case "PATRONI_RETRY_TIMEOUT":
		c.PatroniRetryTimeout, _ = strconv.Atoi(val)
	case "PATRONI_MAX_LAG":
		c.PatroniMaxLag, _ = strconv.Atoi(val)
	case "PATRONI_MASTER_START_TIMEOUT":
		c.PatroniMasterStartTimeout, _ = strconv.Atoi(val)
	case "PATRONI_MASTER_STOP_TIMEOUT":
		c.PatroniMasterStopTimeout, _ = strconv.Atoi(val)
	case "PATRONI_USE_SLOTS":
		c.PatroniUseSlots = strings.EqualFold(val, "true")
	case "PATRONI_SYNCHRONOUS_MODE":
		c.PatroniSynchronousMode = strings.EqualFold(val, "true")
	case "PATRONI_SYNCHRONOUS_MODE_STRICT":
		c.PatroniSynchronousModeStrict = strings.EqualFold(val, "true")
	case "PATRONI_SYNCHRONOUS_NODE_COUNT":
		c.PatroniSynchronousNodeCount, _ = strconv.Atoi(val)
	case "SSL_CERT_VALIDITY_DAYS":
		c.SSLCertValidityDays, _ = strconv.Atoi(val)
	}
}

// WriteClusterEnv persists the relevant fields to /etc/cluster_env so subsequent
// subcommands and the legacy shell scripts can read them.
func (c *Config) WriteClusterEnv() error {
	lines := []string{
		"MY_NAME=" + c.MyName,
		"MY_IP=" + c.MyIP,
		"ETCD_HOSTS=" + c.EtcdHosts,
		"ETCD_INITIAL_CLUSTER=" + c.EtcdInitialCluster,
		fmt.Sprintf("HOST_POSTGRES_PORT=%d", c.HostPostgresPort),
		fmt.Sprintf("HOST_PATRONI_API_PORT=%d", c.HostPatroniAPIPort),
		fmt.Sprintf("HOST_ETCD_CLIENT_PORT=%d", c.HostEtcdClientPort),
		fmt.Sprintf("HOST_ETCD_PEER_PORT=%d", c.HostEtcdPeerPort),
		fmt.Sprintf("POSTGRES_PORT=%d", c.PostgresPort),
		fmt.Sprintf("PATRONI_API_PORT=%d", c.PatroniAPIPort),
		fmt.Sprintf("ETCD_CLIENT_PORT=%d", c.EtcdClientPort),
		fmt.Sprintf("ETCD_PEER_PORT=%d", c.EtcdPeerPort),
		"POSTGRES_SUPERUSER_PASSWORD=" + c.PostgresSuperuserPassword,
		"POSTGRES_REPLICATION_PASSWORD=" + c.PostgresReplicationPassword,
		fmt.Sprintf("PATRONI_SYNCHRONOUS_MODE=%s", strconv.FormatBool(c.PatroniSynchronousMode)),
		fmt.Sprintf("PATRONI_SYNCHRONOUS_MODE_STRICT=%s", strconv.FormatBool(c.PatroniSynchronousModeStrict)),
		fmt.Sprintf("PATRONI_SYNCHRONOUS_NODE_COUNT=%d", c.PatroniSynchronousNodeCount),
		"APP_NAME=" + c.AppName,
		fmt.Sprintf("SSL_ENABLED=%s", strconv.FormatBool(c.SSLEnabled)),
		fmt.Sprintf("SSL_CERT_VALIDITY_DAYS=%d", c.SSLCertValidityDays),
		fmt.Sprintf("ALLOW_NEW_CLUSTER_BOOTSTRAP=%s", strconv.FormatBool(c.AllowNewClusterBootstrap)),
		fmt.Sprintf("ALLOW_ANY_NODE_BOOTSTRAP=%s", strconv.FormatBool(c.AllowAnyNodeBootstrap)),
		fmt.Sprintf("AUTO_BOOTSTRAP_IF_FRESH=%s", strconv.FormatBool(c.AutoBootstrapIfFresh)),
		fmt.Sprintf("DEAD_CLUSTER_RECOVERY=%s", strconv.FormatBool(c.DeadClusterRecovery)),
		fmt.Sprintf("ETCD_JOIN_MAX_RETRIES=%d", c.EtcdJoinMaxRetries),
		fmt.Sprintf("ETCD_JOIN_RETRY_DELAY_SECONDS=%d", c.EtcdJoinRetryDelaySeconds),
		fmt.Sprintf("UPDATE_INTERVAL_SECONDS=%d", c.UpdateIntervalSeconds),
		fmt.Sprintf("DESIRED_STATE_STABILITY_CYCLES=%d", c.DesiredStateStabilityCycles),
		fmt.Sprintf("ETCD_UNAVAILABLE_RECOVERY_CYCLES=%d", c.EtcdUnavailableRecoveryCycles),
		fmt.Sprintf("PATRONI_TTL=%d", c.PatroniTTL),
		fmt.Sprintf("PATRONI_LOOP_WAIT=%d", c.PatroniLoopWait),
		fmt.Sprintf("PATRONI_RETRY_TIMEOUT=%d", c.PatroniRetryTimeout),
		fmt.Sprintf("PATRONI_MAX_LAG=%d", c.PatroniMaxLag),
		fmt.Sprintf("PATRONI_MASTER_START_TIMEOUT=%d", c.PatroniMasterStartTimeout),
		fmt.Sprintf("PATRONI_MASTER_STOP_TIMEOUT=%d", c.PatroniMasterStopTimeout),
		fmt.Sprintf("PATRONI_USE_SLOTS=%s", strconv.FormatBool(c.PatroniUseSlots)),
	}
	return os.WriteFile(ClusterEnvFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// EtcdProtocol returns "https" if SSL is enabled, otherwise "http".
func (c *Config) EtcdProtocol() string {
	if c.SSLEnabled {
		return "https"
	}
	return "http"
}
