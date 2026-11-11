package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
	"github.com/RunOnFlux/flux-pg-cluster/internal/etcdmgr"
	pkglog "github.com/RunOnFlux/flux-pg-cluster/internal/log"
	"github.com/RunOnFlux/flux-pg-cluster/internal/patroni"
)

const postgresDataDir = "/var/lib/postgresql/data"
const patroniDCSIdentityFile = "/tmp/patroni-dcs-identity"

func newDCSBootstrapToken(base string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return base + "-" + hex.EncodeToString(nonce[:]), nil
}

// Patroni caches Raft terms in memory. A brand-new DCS can legitimately have a
// lower term; reset that client only after the pinned PostgreSQL identity has
// been verified. This marker belongs to the local Patroni process, not PGDATA.
func reconcilePatroniDCSEpoch(cfg *config.Config, ec *etcdmgr.Client, marker string, restart func() error) error {
	old, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := validatePeerCluster(ctx, cfg, ec); err != nil {
		return err
	}
	id, err := ec.ClusterID(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(old)) == id {
		return nil
	}
	if err := restart(); err != nil {
		return err
	}
	// The marker is owned by postgres in sticky /tmp. Root must not open it
	// with O_CREAT (fs.protected_regular rejects that), or every loop retries
	// an already successful restart. Preserve its owner for the next startup.
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(id + "\n"); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// This identity is inherited by physical replicas. It is evidence about this
// database, not an election service. EXPECTED_POSTGRES_SYSTEM_ID in deployment
// configuration is what protects entirely new replacement volumes.
type clusterHistory struct {
	Version  int      `json:"version"`
	App      string   `json:"app"`
	Scope    string   `json:"scope"`
	SystemID string   `json:"system_id"`
	Members  []string `json:"members"`
}

func historyPath(dataDir string) string { return filepath.Join(dataDir, ".flux-agent", "cluster.json") }

func readHistory(dataDir string) (clusterHistory, error) {
	var h clusterHistory
	b, err := os.ReadFile(historyPath(dataDir))
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, err
	}
	if h.Version != 1 || !validSystemID(h.SystemID) || h.App == "" || h.Scope == "" {
		return h, errors.New("invalid persisted cluster identity")
	}
	return h, nil
}

func validSystemID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n != 0 && strconv.FormatUint(n, 10) == id
}

func atomicWrite(path string, data []byte) error {
	// History contains no credentials. PostgreSQL must be able to read even
	// temporary entries while pg_basebackup traverses PGDATA concurrently.
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(path), ".cluster-"+hex.EncodeToString(nonce[:])), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func readPGSystemID(dataDir string) (string, error) {
	bin, err := discoverPostgresBinDir()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(filepath.Join(bin, "pg_controldata"), dataDir).Output()
	if err != nil {
		return "", fmt.Errorf("read PostgreSQL control data: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "Database system identifier" && validSystemID(strings.TrimSpace(val)) {
			return strings.TrimSpace(val), nil
		}
	}
	return "", errors.New("PostgreSQL system identifier unreadable")
}

func expectedSystemIDAt(cfg *config.Config, dataDir string) (string, error) {
	id := cfg.ExpectedSystemID
	if id != "" && !validSystemID(id) {
		return "", errors.New("EXPECTED_POSTGRES_SYSTEM_ID must be a PostgreSQL system identifier")
	}
	h, err := readHistory(dataDir)
	if err == nil {
		if h.App != cfg.AppName || h.Scope != cfg.PatroniScope {
			return "", errors.New("persisted cluster app/scope mismatch")
		}
		if id != "" && id != h.SystemID {
			return "", errors.New("deployment identity conflicts with persisted cluster identity; preserve data and restore the correct deployment configuration")
		}
		id = h.SystemID
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("cluster history: %w", err)
	}
	if fileExists(filepath.Join(dataDir, "global", "pg_control")) {
		local, err := readPGSystemID(dataDir)
		if err != nil {
			return "", err
		}
		if id != "" && local != id {
			return "", fmt.Errorf("PGDATA identity %s differs from expected %s", local, id)
		}
		id = local // Migration of existing volumes never trusts a peer's identity.
	}
	if id == "" {
		return "", fmt.Errorf("%w: join a verified initialized cluster, restore existing PGDATA, or explicitly create the first cluster", errUnknownIdentity)
	}
	return id, nil
}

func expectedSystemID(cfg *config.Config) (string, error) {
	id, err := expectedSystemIDAt(cfg, postgresDataDir)
	if errors.Is(err, errUnknownIdentity) {
		return learnedSystemID(cfg, postgresDataDir, replacementIdentityFile)
	}
	return id, err
}

func rememberClusterAt(cfg *config.Config, dataDir string, ips []string) error {
	if !fileExists(filepath.Join(dataDir, "global", "pg_control")) {
		return nil
	}
	id, err := expectedSystemIDAt(cfg, dataDir)
	if err != nil {
		return err
	}
	h, err := readHistory(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	h = clusterHistory{Version: 1, App: cfg.AppName, Scope: cfg.PatroniScope, SystemID: id, Members: unionIPs(h.Members, ips)}
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	if err = atomicWrite(historyPath(dataDir), b); err != nil {
		return err
	}
	// The agent runs as root; Patroni/basebackup must be able to copy history.
	if os.Geteuid() == 0 {
		info, err := os.Stat(dataDir)
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			if err := os.Chown(filepath.Dir(historyPath(dataDir)), int(st.Uid), int(st.Gid)); err != nil {
				return err
			}
			return os.Chown(historyPath(dataDir), int(st.Uid), int(st.Gid))
		}
	}
	return nil
}

func unionIPs(groups ...[]string) []string {
	seen := map[string]bool{}
	for _, group := range groups {
		for _, ip := range group {
			ip = strings.TrimSpace(ip)
			if net.ParseIP(ip) != nil {
				seen[ip] = true
			}
		}
	}
	var ips []string
	for ip := range seen {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	return ips
}

func discoveryIPsAt(cfg *config.Config, dataDir string, discovered []string) ([]string, error) {
	h, err := readHistory(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil && (h.App != cfg.AppName || h.Scope != cfg.PatroniScope) {
		return nil, errors.New("persisted membership belongs to another app/scope")
	}
	ips := unionIPs(discovered, candidateIPs(cfg), otherIPsFromInitialCluster(cfg.EtcdInitialCluster, ""), h.Members, splitCSV(cfg.ClusterSeedIPs), []string{cfg.MyIP})
	retired := splitCSV(cfg.RetiredClusterIPs)
	var active []string
	for _, ip := range ips {
		if ip == cfg.MyIP || !containsString(retired, ip) {
			active = append(active, ip)
		}
	}
	return active, nil
}

// A peer's live DCS must carry our already pinned identity. A larger etcd
// membership, a familiar name, or an empty /initialize key is never authority.
func validatePeerCluster(ctx context.Context, cfg *config.Config, ec *etcdmgr.Client) error {
	id, err := expectedSystemID(cfg)
	if errors.Is(err, errUnknownIdentity) {
		id, err = learnReplacementIdentity(ctx, cfg, ec)
	}
	if err != nil {
		return err
	}
	got, err := ec.Get(ctx, fmt.Sprintf("/patroni/%s/initialize", cfg.PatroniScope))
	if err != nil {
		return err
	}
	if got != id {
		return fmt.Errorf("peer DCS identity %q differs from expected %s", got, id)
	}
	return nil
}

func etcdHasLocalState(dataDir string) bool {
	// WAL-only state is still durable state: never silently discard a partially
	// written or recoverable member just because snap/db is absent.
	entries, err := os.ReadDir(filepath.Join(dataDir, "member"))
	return err == nil && len(entries) > 0
}

// Archived Raft data is retained for inspection, but cannot vote or serve DCS.
// Unknown files and any active member state still block total-DCS recovery.
func etcdRecoveryDataEmpty(dataDir string) bool {
	entries, err := os.ReadDir(dataDir)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	for _, entry := range entries {
		switch entry.Name() {
		case ".etcd3_api", ".flux-pg-identity.json":
			if !entry.Type().IsRegular() {
				return false
			}
		case ".flux-etcd-archives":
			if !entry.IsDir() {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func archiveEtcdData(dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	dest := filepath.Join(dataDir, ".flux-etcd-archives", strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".flux-etcd-archives" || entry.Name() == ".flux-pg-identity.json" {
			continue
		}
		if err := os.Rename(filepath.Join(dataDir, entry.Name()), filepath.Join(dest, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func clusterMemberReachable(cfg *config.Config, ip string) bool {
	host := ip
	port := cfg.HostEtcdClientPort
	if ip == cfg.MyIP {
		host = "127.0.0.1"
		port = cfg.EtcdClientPort
	}
	if etcdHealthReachable(fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), host, port), cfg) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := probePeerIdentity(ctx, cfg, ip)
	if err == nil {
		return true
	}
	// Positive legacy Patroni liveness also prevents eviction during rollout;
	// an unsigned response can never authorize recovery or identity adoption.
	pc := patroni.New(cfg.SSLEnabled, cfg.HostPatroniAPIPort)
	defer pc.Close()
	return pc.IsAlive(ctx, ip)
}

func anyDCSQuorum(cfg *config.Config) bool {
	for _, ip := range unionIPs(candidateIPs(cfg), []string{cfg.MyIP}) {
		host, port := ip, cfg.HostEtcdClientPort
		if ip == cfg.MyIP {
			host, port = "127.0.0.1", cfg.EtcdClientPort
		}
		ec := etcdmgr.New(fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), host, port), etcdSSLOpts(cfg))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := ec.Get(ctx, "/_cluster_mgmt/quorum_probe")
		cancel()
		if err == nil {
			return true
		}
	}
	return false
}

func runCreateCluster(args []string) {
	fs := flag.NewFlagSet("create-cluster", flag.ExitOnError)
	confirm := fs.String("confirm-new-cluster", "", "application name; authorizes first creation only")
	token := fs.String("operator-token-file", "", "one-time file containing at least 32 random characters; consumed before initdb")
	_ = fs.Parse(args)
	cfg := config.FromEnv()
	if err := createCluster(cfg, postgresDataDir, *confirm, *token); err != nil {
		pkglog.Fatalf("cluster creation refused: %v", err)
	}
	id, err := readPGSystemID(postgresDataDir)
	if err != nil {
		pkglog.Fatalf("read created identity: %v", err)
	}
	fmt.Printf("EXPECTED_POSTGRES_SYSTEM_ID=%s\n", id)
}

func createCluster(cfg *config.Config, dataDir, confirm, tokenFile string) error {
	if confirm == "" || confirm != cfg.AppName {
		return errors.New("--confirm-new-cluster must exactly match APP_NAME")
	}
	if cfg.ExpectedSystemID != "" {
		return errors.New("deployment already has an expected identity; restore it instead")
	}
	if !directoryIsEmpty(dataDir) {
		return errors.New("PGDATA is not strictly empty; refusing reinitialization")
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(token))) < 32 {
		return errors.New("operator token must contain at least 32 random characters")
	}
	// O_EXCL also prevents concurrent invocations and reuse after a failed initdb.
	used, err := os.OpenFile(tokenFile+".consumed", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("consume operator token: %w", err)
	}
	if err = used.Sync(); err != nil {
		used.Close()
		return err
	}
	if err = used.Close(); err != nil {
		return err
	}
	if err = os.Remove(tokenFile); err != nil {
		return err
	}
	bin, err := discoverPostgresBinDir()
	if err != nil {
		return err
	}
	pw, err := os.CreateTemp("", "flux-initdb-password-*")
	if err != nil {
		return err
	}
	defer os.Remove(pw.Name())
	if _, err = pw.WriteString(cfg.PostgresSuperuserPassword + "\n"); err != nil {
		pw.Close()
		return err
	}
	if err = pw.Close(); err != nil {
		return err
	}
	cmdArgs := []string{filepath.Join(bin, "initdb"), "-D", dataDir, "--username=postgres", "--encoding=UTF8", "--data-checksums", "--auth-host=md5", "--auth-local=peer", "--pwfile=" + pw.Name()}
	if os.Geteuid() == 0 {
		if out, err := exec.Command("chown", "postgres:postgres", pw.Name(), dataDir).CombinedOutput(); err != nil {
			return fmt.Errorf("set initdb ownership: %w: %s", err, out)
		}
		cmdArgs = append([]string{"runuser", "-u", "postgres", "--"}, cmdArgs...)
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	// Existing-PGDATA startup can receive DCS configuration while PostgreSQL is
	// still starting, before Patroni applies dynamic HBA changes. Seed the same
	// access rules as the normal bootstrap so cloning never depends on that race.
	if err := os.WriteFile(filepath.Join(dataDir, "pg_hba.conf"), []byte(strings.Join(requiredPatroniHBA, "\n")+"\n"), 0600); err != nil {
		return err
	}
	if err := rememberClusterAt(cfg, dataDir, splitCSV(cfg.ClusterSeedIPs)); err != nil {
		return err
	}
	// Existing-PGDATA startup bypasses Patroni's post_bootstrap callback. Record
	// the one-time application setup so the healthy primary performs it via SQL.
	return os.WriteFile(filepath.Join(dataDir, ".flux-agent", "first-creation"), []byte("pending\n"), 0644)
}

// There is deliberately no initdb fallback. Existing PGDATA can rebuild DCS;
// empty replicas can clone the pinned cluster. Neither may invent a database.
func runRefuseInitDB() {
	pkglog.Fatalf("initdb is disabled during normal startup; use flux-agent create-cluster for an intentional first deployment, or restore the pinned cluster")
}

func runPatroniStart() {
	cfg := config.FromEnv()
	if err := config.LoadClusterEnv(cfg); err != nil {
		pkglog.Fatalf("load cluster environment: %v", err)
	}
	for {
		if err := config.LoadClusterEnv(cfg); err != nil {
			pkglog.Fatalf("reload cluster environment: %v", err)
		}
		if _, err := expectedSystemID(cfg); err != nil {
			pkglog.Errorf("Patroni startup blocked: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}
		break
	}
	ec := etcdmgr.New(fmt.Sprintf("%s://127.0.0.1:%d", cfg.EtcdProtocol(), cfg.EtcdClientPort), etcdSSLOpts(cfg))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	id, err := ec.ClusterID(ctx)
	cancel()
	if err != nil {
		pkglog.Fatalf("read Patroni control-plane identity: %v", err)
	}
	if err := os.WriteFile(patroniDCSIdentityFile, []byte(id+"\n"), 0600); err != nil {
		pkglog.Fatalf("record Patroni control-plane identity: %v", err)
	}
	// CPython resolves a bare argv[0] through PATH to locate its venv. Preserve
	// the absolute interpreter name even when a legacy deployment supplies PATH.
	if err := syscall.Exec("/opt/patroni/bin/python3", []string{"/opt/patroni/bin/python3", "-m", "patroni", "/etc/patroni/patroni.yml"}, os.Environ()); err != nil {
		pkglog.Fatalf("start Patroni: %v", err)
	}
}

func runVerifyPromotion() {
	cfg := config.FromEnv()
	if err := config.LoadClusterEnv(cfg); err != nil {
		pkglog.Fatalf("promotion config: %v", err)
	}
	if !fileExists(filepath.Join(postgresDataDir, "global", "pg_control")) {
		pkglog.Fatalf("promotion requires existing PostgreSQL data")
	}
	ec := etcdmgr.New(fmt.Sprintf("%s://127.0.0.1:%d", cfg.EtcdProtocol(), cfg.EtcdClientPort), etcdSSLOpts(cfg))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := validatePeerCluster(ctx, cfg, ec); err != nil {
		pkglog.Fatalf("promotion refused: %v", err)
	}
}
