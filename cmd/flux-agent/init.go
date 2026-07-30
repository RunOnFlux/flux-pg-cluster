package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
	"github.com/RunOnFlux/flux-pg-cluster/internal/fluxapi"
	pkglog "github.com/RunOnFlux/flux-pg-cluster/internal/log"
)

// runInit performs the one-shot cluster initialization that entrypoint.sh used
// to do: discover MY_IP, query Flux API for cluster members, render patroni.yml
// from template, generate SSL certificates, write /etc/cluster_env.
func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	templatePath := fs.String("template", "/app/patroni.yml.tpl", "Patroni config template")
	outPath := fs.String("out", "/etc/patroni/patroni.yml", "Patroni config output path")
	certsScript := fs.String("certs-script", "/app/generate-certs.sh", "shell script for cert generation")
	_ = fs.Parse(args)

	pkglog.Section("FLUX PG CLUSTER — DYNAMIC PATRONI CLUSTER (Go agent)")
	versionFile := "/app/VERSION"
	if data, err := os.ReadFile(versionFile); err == nil {
		pkglog.Infof("version: %s", strings.TrimSpace(string(data)))
	}

	cfg := config.FromEnv()

	// Resolve APP_NAME from Flux hostinfo if available
	if appName := tryGetHostInfoAppName(); appName != "" {
		cfg.AppName = appName
		pkglog.Infof("APP_NAME resolved from hostinfo API: %s", appName)
	} else {
		pkglog.Infof("APP_NAME from env/default: %s", cfg.AppName)
	}

	// Validate passwords for SQL safety (mirrors entrypoint.sh)
	if strings.Contains(cfg.PostgresSuperuserPassword, "$") {
		pkglog.Fatalf("POSTGRES_SUPERUSER_PASSWORD contains '$' which breaks Patroni's SQL")
	}
	if strings.Contains(cfg.PostgresReplicationPassword, "$") {
		pkglog.Fatalf("POSTGRES_REPLICATION_PASSWORD contains '$' which breaks Patroni's SQL")
	}

	pkglog.Section("IP DISCOVERY")
	myIP, err := discoverMyIP(cfg)
	if err != nil {
		pkglog.Fatalf("could not discover MY_IP: %v", err)
	}
	cfg.MyIP = myIP
	pkglog.Infof("MY_IP: %s", myIP)

	pkglog.Section("FLUX API DISCOVERY")
	ips, err := discoverClusterIPs(cfg)
	if err != nil {
		pkglog.Warnf("Flux API lookup failed: %v — falling back to MY_IP only", err)
		ips = []string{myIP}
	}
	if len(ips) == 0 {
		pkglog.Warnf("No cluster IPs from API, using MY_IP only")
		ips = []string{myIP}
	}
	cfg.ClusterIPs = ips
	pkglog.Infof("Cluster IPs: %v", ips)

	pkglog.Section("CLUSTER CONFIGURATION GENERATION")
	cfg.MyName = nodeNameFromIP(myIP)
	buildEtcdConfig(cfg)
	pkglog.Infof("MY_NAME: %s", cfg.MyName)
	pkglog.Infof("ETCD_HOSTS: %s", cfg.EtcdHosts)
	pkglog.Infof("ETCD_INITIAL_CLUSTER: %s", cfg.EtcdInitialCluster)

	// SSL certificate generation
	if cfg.SSLEnabled {
		pkglog.Section("SSL CERTIFICATE GENERATION")
		if cfg.SSLPassphrase == "" {
			pkglog.Fatalf("SSL_ENABLED=true but SSL_PASSPHRASE is empty")
		}
		if err := runCertsScript(*certsScript, cfg); err != nil {
			pkglog.Fatalf("certificate generation failed: %v", err)
		}
		pkglog.Infof("certificate generation complete")
	}

	pkglog.Section("PATRONI CONFIGURATION GENERATION")
	pgBinDir, err := discoverPostgresBinDir()
	if err != nil {
		pkglog.Fatalf("discover postgres bin dir: %v", err)
	}
	pkglog.Infof("PostgreSQL bin_dir: %s", pgBinDir)
	if err := checkPostgresDataVersion("/var/lib/postgresql/data", os.Getenv("POSTGRES_MAJOR")); err != nil {
		pkglog.Fatalf("postgres data version mismatch: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		pkglog.Fatalf("mkdir patroni dir: %v", err)
	}
	if err := renderPatroniConfig(*templatePath, *outPath, cfg, pgBinDir); err != nil {
		pkglog.Fatalf("render patroni config: %v", err)
	}
	pkglog.Infof("patroni.yml written to %s", *outPath)

	// Postgres data directory permissions
	if err := os.MkdirAll("/var/lib/postgresql/data", 0o700); err != nil {
		pkglog.Errorf("mkdir postgres data: %v", err)
	}
	_ = exec.Command("chown", "-R", "postgres:postgres", "/var/lib/postgresql/data").Run()
	_ = exec.Command("chmod", "700", "/var/lib/postgresql/data").Run()

	// etcd data directory
	if err := os.MkdirAll("/var/lib/etcd", 0o700); err != nil {
		pkglog.Errorf("mkdir etcd data: %v", err)
	}

	pkglog.Section("WRITING CLUSTER ENV FILE")
	if err := cfg.WriteClusterEnv(); err != nil {
		pkglog.Fatalf("write cluster env: %v", err)
	}
	pkglog.Infof("wrote %s", config.ClusterEnvFile)
	pkglog.Section("INITIALIZATION COMPLETE")
}

// discoverMyIP replicates entrypoint.sh's IP detection logic, with the
// hostname-mapping shortcut for known docker-compose containers.
func discoverMyIP(cfg *config.Config) (string, error) {
	host, _ := os.Hostname()
	// Local testing shortcut: if FLUX_API_URL points at the test mock,
	// use container hostname mapping.
	if strings.Contains(cfg.FluxAPIURL, "172.20.0.5") {
		switch host {
		case "postgres-cluster-node1":
			return "172.20.0.10", nil
		case "postgres-cluster-node2":
			return "172.20.0.11", nil
		case "postgres-cluster-node3":
			return "172.20.0.12", nil
		}
		// Unknown hostname — filter for cluster subnet (172.20.x.x) to avoid
		// picking the Docker default bridge IP when both interfaces are attached.
		if ip := pickIPInSubnet("172.20."); ip != "" {
			return ip, nil
		}
		if ip := pickFirstNonLoopbackIP(); ip != "" {
			return ip, nil
		}
		return "", errors.New("no IP discovered (local testing mode)")
	}

	// Production: query external echo services
	for _, url := range []string{"http://ifconfig.me", "http://ipinfo.io/ip"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ip := strings.TrimSpace(string(body))
		if net.ParseIP(ip) != nil {
			return ip, nil
		}
	}
	// Fall back to first non-loopback
	if ip := pickFirstNonLoopbackIP(); ip != "" {
		return ip, nil
	}
	return "", errors.New("no IP discovered (production mode)")
}

func pickIPInSubnet(prefix string) string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ip, _, err := net.ParseCIDR(a.String())
		if err != nil {
			continue
		}
		v4 := ip.To4()
		if v4 != nil && strings.HasPrefix(v4.String(), prefix) {
			return v4.String()
		}
	}
	return ""
}

func pickFirstNonLoopbackIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ip, _, err := net.ParseCIDR(a.String())
		if err != nil {
			continue
		}
		if ip.IsLoopback() {
			continue
		}
		v4 := ip.To4()
		if v4 != nil {
			return v4.String()
		}
	}
	return ""
}

// tryGetHostInfoAppName queries the Flux node hostinfo API for the running app name.
func tryGetHostInfoAppName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://fluxnode.service:16101/hostinfo", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// crude parse: look for "appName":"VAL"
	s := string(body)
	idx := strings.Index(s, `"appName":"`)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(`"appName":"`):]
	end := strings.Index(rest, `"`)
	if end <= 0 {
		return ""
	}
	return rest[:end]
}

func discoverClusterIPs(cfg *config.Config) ([]string, error) {
	// Retry with backoff: at container start the Flux API container may not
	// yet be accepting connections. Without this retry we fall back to MY_IP
	// only, causing a 1-node etcd bootstrap that fights with co-booting peers.
	c := fluxapi.New(cfg.FluxAPIURL)
	var lastErr error
	deadline := time.Now().Add(60 * time.Second)
	delay := 1 * time.Second
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ips, err := c.ListIPs(ctx, cfg.AppName)
		cancel()
		if err == nil && len(ips) > 0 {
			return ips, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		pkglog.Infof("Flux API retry in %s (last error: %v)", delay, lastErr)
		time.Sleep(delay)
		if delay < 5*time.Second {
			delay += 1 * time.Second
		}
	}
}

func nodeNameFromIP(ip string) string {
	return "node-" + strings.ReplaceAll(ip, ".", "-")
}

func buildEtcdConfig(cfg *config.Config) {
	ips := make([]string, len(cfg.ClusterIPs))
	copy(ips, cfg.ClusterIPs)
	// Ensure MY_IP is included (mirrors entrypoint.sh)
	have := false
	for _, ip := range ips {
		if ip == cfg.MyIP {
			have = true
			break
		}
	}
	if !have {
		ips = append(ips, cfg.MyIP)
		pkglog.Warnf("MY_IP %s not in Flux API list — added it", cfg.MyIP)
	}
	sort.Strings(ips)
	cfg.ClusterIPs = ips

	cfg.EtcdHosts, cfg.PatroniEtcdHosts, cfg.EtcdInitialCluster = buildEtcdTopology(cfg, ips)
}

func buildEtcdTopology(cfg *config.Config, ips []string) (string, string, string) {
	var hostsParts, initialParts []string
	for _, ip := range ips {
		name := nodeNameFromIP(ip)
		clientURL := fmt.Sprintf("%s:%d", ip, cfg.HostEtcdClientPort)
		peerURL := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), ip, cfg.HostEtcdPeerPort)
		hostsParts = append(hostsParts, clientURL)
		initialParts = append(initialParts, name+"="+peerURL)
	}
	patroniHosts := fmt.Sprintf("127.0.0.1:%d", cfg.EtcdClientPort)
	return strings.Join(hostsParts, ","), patroniHosts, strings.Join(initialParts, ",")
}

func runCertsScript(script string, cfg *config.Config) error {
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(),
		"SSL_PASSPHRASE="+cfg.SSLPassphrase,
		"SSL_CERT_VALIDITY_DAYS="+strconv.Itoa(cfg.SSLCertValidityDays),
		"APP_NAME="+cfg.AppName,
		"MY_IP="+cfg.MyIP,
		"CLUSTER_IPS="+strings.Join(cfg.ClusterIPs, " "),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// discoverPostgresBinDir returns the PostgreSQL binary directory for the
// version installed in this image. POSTGRES_MAJOR (set at image build time) is
// preferred; otherwise common install paths are probed.
func discoverPostgresBinDir() (string, error) {
	if major := strings.TrimSpace(os.Getenv("POSTGRES_MAJOR")); major != "" {
		dir := filepath.Join("/usr/lib/postgresql", major, "bin")
		if _, err := os.Stat(filepath.Join(dir, "postgres")); err == nil {
			return dir, nil
		}
	}
	for _, major := range []string{"17", "16", "15", "14", "13"} {
		dir := filepath.Join("/usr/lib/postgresql", major, "bin")
		if _, err := os.Stat(filepath.Join(dir, "postgres")); err == nil {
			return dir, nil
		}
	}
	return "", errors.New("postgres binary not found under /usr/lib/postgresql/*/bin")
}

// checkPostgresDataVersion rejects starting when an existing data directory was
// initialized by a different PostgreSQL major version than this image provides.
func checkPostgresDataVersion(dataDir, expectedMajor string) error {
	expectedMajor = strings.TrimSpace(expectedMajor)
	if expectedMajor == "" {
		return nil
	}
	pgVersionFile := filepath.Join(dataDir, "PG_VERSION")
	data, err := os.ReadFile(pgVersionFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	existingMajor := strings.TrimSpace(string(data))
	if existingMajor != expectedMajor {
		return fmt.Errorf(
			"data directory has PostgreSQL %s but this image provides PostgreSQL %s; use a matching image tag or fresh volumes",
			existingMajor, expectedMajor,
		)
	}
	return nil
}

// renderPatroniConfig reads the legacy "__VAR__" placeholder template and
// substitutes it with cfg values. We keep the legacy template format so the
// same file can be used by the shell scripts during the transition.
func renderPatroniConfig(in, out string, cfg *config.Config, pgBinDir string) error {
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	postgresSSL := "off"
	if cfg.SSLEnabled {
		postgresSSL = "on"
	}
	replacements := map[string]string{
		"__MY_NAME__":                       cfg.MyName,
		"__MY_IP__":                         cfg.MyIP,
		"__PATRONI_ETCD_HOSTS__":            cfg.PatroniEtcdHosts,
		"__ETCD_PROTOCOL__":                 cfg.EtcdProtocol(),
		"__HOST_POSTGRES_PORT__":            strconv.Itoa(cfg.HostPostgresPort),
		"__HOST_PATRONI_API_PORT__":         strconv.Itoa(cfg.HostPatroniAPIPort),
		"__POSTGRES_PORT__":                 strconv.Itoa(cfg.PostgresPort),
		"__PATRONI_API_PORT__":              strconv.Itoa(cfg.PatroniAPIPort),
		"__POSTGRES_DB__":                   cfg.PostgresDB,
		"__POSTGRES_SUPERUSER_PASSWORD__":   escapeYAMLSingle(cfg.PostgresSuperuserPassword),
		"__POSTGRES_REPLICATION_PASSWORD__": escapeYAMLSingle(cfg.PostgresReplicationPassword),
		"__SSL_ENABLED__":                   postgresSSL,
		"__SYNCHRONOUS_MODE__":              strconv.FormatBool(cfg.PatroniSynchronousMode),
		"__SYNCHRONOUS_MODE_STRICT__":       strconv.FormatBool(cfg.PatroniSynchronousModeStrict),
		"__SYNCHRONOUS_NODE_COUNT__":        strconv.Itoa(cfg.PatroniSynchronousNodeCount),
		"__PATRONI_FAILSAFE_MODE__":         strconv.FormatBool(cfg.PatroniFailsafeMode),
		"__PATRONI_TTL__":                   strconv.Itoa(cfg.PatroniTTL),
		"__PATRONI_LOOP_WAIT__":             strconv.Itoa(cfg.PatroniLoopWait),
		"__PATRONI_RETRY_TIMEOUT__":         strconv.Itoa(cfg.PatroniRetryTimeout),
		"__PATRONI_MAX_LAG__":               strconv.Itoa(cfg.PatroniMaxLag),
		"__PATRONI_MASTER_START_TIMEOUT__":  strconv.Itoa(cfg.PatroniMasterStartTimeout),
		"__PATRONI_MASTER_STOP_TIMEOUT__":   strconv.Itoa(cfg.PatroniMasterStopTimeout),
		"__PATRONI_USE_SLOTS__":             strconv.FormatBool(cfg.PatroniUseSlots),
		"__PATRONI_LOG_LEVEL__":             cfg.PatroniLogLevel,
		"__POSTGRES_BIN_DIR__":              pgBinDir,
	}
	s := string(data)
	for k, v := range replacements {
		s = strings.ReplaceAll(s, k, v)
	}
	return os.WriteFile(out, []byte(s), 0o644)
}

func escapeYAMLSingle(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
