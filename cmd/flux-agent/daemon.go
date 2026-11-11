package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
	"github.com/RunOnFlux/flux-pg-cluster/internal/etcdmgr"
	"github.com/RunOnFlux/flux-pg-cluster/internal/fluxapi"
	pkglog "github.com/RunOnFlux/flux-pg-cluster/internal/log"
	"github.com/RunOnFlux/flux-pg-cluster/internal/patroni"
)

// runDaemon implements the long-running reconciliation loop from
// update-cluster.sh. It polls the Flux API for the desired cluster membership,
// removes departed members from etcd, triggers force-new-cluster recovery when
// quorum is lost for a sustained period, cleans up ghost members, and keeps
// /etc/cluster_env and patroni.yml in sync.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	stateTrackFile := fs.String("state-file", "/tmp/desired-state-tracker", "desired-state stability tracker file")
	noQuorumFile := fs.String("noquorum-file", "/tmp/etcd-no-quorum-count", "no-quorum cycle counter")
	unavailFile := fs.String("unavail-file", "/tmp/etcd-unavailable-count", "etcd unavailable counter")
	fnfCooldownFile := fs.String("fnf-cooldown-file", "/tmp/force-new-cluster-last-triggered", "FNF cooldown timestamp")
	fnfFlagFile := fs.String("fnf-flag", "/tmp/force-new-cluster", "force-new-cluster flag file written for etcd-start")
	pgWipeCooldownFile := fs.String("pg-wipe-cooldown-file", "/tmp/pg-wipe-last-triggered", "PG data wipe cooldown timestamp")
	_ = fs.Parse(args)

	pkglog.Section("CLUSTER UPDATE DAEMON STARTING (Go agent)")
	pkglog.Infof("time: %s", time.Now().Format(time.RFC3339))

	cfg := config.FromEnv()
	if err := config.LoadClusterEnv(cfg); err != nil {
		pkglog.Fatalf("cannot load %s: %v", config.ClusterEnvFile, err)
	}
	pkglog.Infof("MY_NAME=%s MY_IP=%s SSL=%v UPDATE_INTERVAL=%ds STABILITY=%d",
		cfg.MyName, cfg.MyIP, cfg.SSLEnabled, cfg.UpdateIntervalSeconds, cfg.DesiredStateStabilityCycles)

	fnfCooldownSecs := envIntOr("FNF_COOLDOWN_SECS", 300)
	pgWipeCooldownSecs := envIntOr("PG_WIPE_COOLDOWN_SECS", 300)

	sslOpts := etcdSSLOpts(cfg)
	fc := fluxapi.New(cfg.FluxAPIURL)

	for {
		pkglog.Section(fmt.Sprintf("CLUSTER UPDATE CYCLE - %s", time.Now().Format(time.RFC3339)))
		runReconcile(cfg, fc, sslOpts, *stateTrackFile, *noQuorumFile, *unavailFile, *fnfCooldownFile, *fnfFlagFile, fnfCooldownSecs, *pgWipeCooldownFile, pgWipeCooldownSecs)
		pkglog.Infof("sleeping for %d seconds", cfg.UpdateIntervalSeconds)
		time.Sleep(time.Duration(cfg.UpdateIntervalSeconds) * time.Second)
	}
}

func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// runReconcile is one iteration of the daemon loop. Extracted to keep it
// testable and to make each cycle's flow easier to follow.
func runReconcile(cfg *config.Config, fc *fluxapi.Client, sslOpts []string,
	stateTrackFile, noQuorumFile, unavailFile, fnfCooldownFile, fnfFlagFile string, fnfCooldownSecs int,
	pgWipeCooldownFile string, pgWipeCooldownSecs int) {

	// 1. Get desired state from Flux API
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	desiredIPs, err := fc.ListIPs(ctx, cfg.AppName)
	cancel()
	if err != nil {
		pkglog.Warnf("flux API error: %v — skipping cycle", err)
		return
	}
	if len(desiredIPs) == 0 {
		pkglog.Warnf("empty Flux view: preserving committed membership")
	}
	desiredSignature := strings.Join(desiredIPs, ",")
	stable, stableCount := updateStateTracker(stateTrackFile, desiredSignature, cfg.DesiredStateStabilityCycles)
	pkglog.Infof("desired IPs (%d): %v — stable=%v (%d/%d)", len(desiredIPs), desiredIPs, stable, stableCount, cfg.DesiredStateStabilityCycles)

	// Always update cluster_env and patroni.yml with the latest peer list from
	// Flux API, regardless of stability or etcd health. This ensures a node
	// that starts while the list is in flux (or while etcd is down) always has
	// the correct ETCD_INITIAL_CLUSTER / etcd hosts on its next etcd restart.
	if err := updateClusterEnv(cfg, desiredIPs); err != nil {
		pkglog.Errorf("membership update blocked: %v", err)
		return
	}
	if err := rememberClusterAt(cfg, postgresDataDir, candidateIPs(cfg)); err != nil {
		pkglog.Errorf("cluster identity blocked: %v", err)
		return
	}

	// 2. Probe local etcd health (write-quorum probe)
	// Local etcd listens on ETCD_CLIENT_PORT (container-internal port).
	// Use the external IP with HOST_ETCD_CLIENT_PORT as a fallback — etcd
	// also binds 0.0.0.0, so the external IP works from inside the container.
	localEndpoint := fmt.Sprintf("%s://127.0.0.1:%d", cfg.EtcdProtocol(), cfg.EtcdClientPort)
	externalEndpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), cfg.MyIP, cfg.HostEtcdClientPort)
	endpoint := pickHealthyEtcdEndpoint(cfg, localEndpoint, externalEndpoint)

	if endpoint == "" {
		pkglog.Warnf("etcd not reachable on local or external endpoint")
		handleEtcdUnreachable(cfg, sslOpts, desiredIPs, stable, unavailFile,
			fnfFlagFile, fnfCooldownFile, noQuorumFile, fnfCooldownSecs)
		return
	}
	pkglog.Infof("using etcd endpoint: %s", endpoint)
	_ = os.WriteFile(unavailFile, []byte("0"), 0o644)

	ec := etcdmgr.New(endpoint, sslOpts)

	// 3. Quorum probe (write-test)
	qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
	hasQuorum := ec.SetWithTTL(qctx, "/_cluster_mgmt/quorum_probe", "1", 1) == nil
	qcancel()
	pkglog.Infof("etcd write-quorum: %v", hasQuorum)

	noQuorumCount := 0
	if data, err := os.ReadFile(noQuorumFile); err == nil {
		noQuorumCount, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	if !hasQuorum {
		noQuorumCount++
		_ = os.WriteFile(noQuorumFile, []byte(strconv.Itoa(noQuorumCount)), 0o644)
		pkglog.Infof("no-quorum count: %d/%d", noQuorumCount, cfg.DesiredStateStabilityCycles)
	} else {
		_ = os.WriteFile(noQuorumFile, []byte("0"), 0o644)
		reconcilePatroniDCSConfig(cfg, ec)
		reconcilePostgresCredentials(cfg)
		if err := reconcilePatroniDCSEpoch(cfg, ec, patroniDCSIdentityFile, func() error {
			pkglog.Warnf("verified control-plane identity changed — restarting Patroni to reset its cached Raft term")
			return exec.Command("supervisorctl", "restart", "patroni").Run()
		}); err != nil {
			pkglog.Warnf("Patroni control-plane identity reconciliation: %v", err)
		}
	}

	// 4. Force-new-cluster check. Never choose a survivor merely because it has
	// PGDATA: during an ordinary quorum loss every replica has PGDATA. Automatic
	// recovery requires authenticated agreement on either a sole data copy or,
	// after total DCS loss, one durable primary among matching replicas.
	if cfg.DeadClusterRecovery && !hasQuorum && noQuorumCount >= cfg.DesiredStateStabilityCycles && stable && dirExists("/var/lib/postgresql/data/global") {
		authority, safe, reason := confirmRecoveryAuthorityWithPeers(cfg)
		if !safe {
			pkglog.Errorf("AUTOMATIC QUORUM RECOVERY BLOCKED: %s", reason)
		} else if authority.IP != cfg.MyIP {
			pkglog.Errorf("AUTOMATIC QUORUM RECOVERY BLOCKED: sole authority is %s, not this node", authority.NodeName)
		} else if shouldFNFNow(fnfCooldownFile, fnfCooldownSecs) {
			pkglog.Warnf("automatic recovery authorized: %s", reason)
			triggerForceNewCluster(cfg, sslOpts, authority.SystemID, fnfFlagFile, fnfCooldownFile, noQuorumFile)
			return
		}
	}

	// 5. Member list, ghost cleanup, mismatch detection, etc.
	mctx, mcancel := context.WithTimeout(context.Background(), 5*time.Second)
	members, err := ec.MemberList(mctx)
	mcancel()
	if err != nil {
		pkglog.Warnf("member list error: %v — skipping rest of cycle", err)
		return
	}

	// Preserve committed peers even when the initial Flux view was incomplete
	// during migration from a release without durable membership history.
	known := unionIPs(candidateIPs(cfg), membersIPs(members, cfg.HostEtcdClientPort))
	if err := updateClusterEnv(cfg, known); err != nil {
		pkglog.Errorf("remember committed membership: %v", err)
		return
	}
	if err := rememberClusterAt(cfg, postgresDataDir, candidateIPs(cfg)); err != nil {
		pkglog.Errorf("remember committed membership: %v", err)
		return
	}
	// Membership size is not authority. Repair stale initialization metadata
	// only toward the pinned identity; conflicting PGDATA is never deleted.
	checkPatroniSystemID(cfg, ec, desiredIPs, pgWipeCooldownFile, pgWipeCooldownSecs)

	// Remove absent members only with write quorum and repeated direct failures.
	// A stable API omission never overrides a reachable member, or removes self.
	for _, m := range members {
		ip := extractHostFromURL(m.PeerURLs)
		track := stateTrackFile + ".absent-" + strings.ReplaceAll(ip, ":", "_")
		if ip == "" || ip == cfg.MyIP || m.Name == cfg.MyName || containsString(desiredIPs, ip) || !stable || !hasQuorum {
			_ = os.Remove(track)
			continue
		}
		if clusterMemberReachable(cfg, ip) {
			_ = os.Remove(track)
			continue
		}
		failed, _ := updateStateTracker(track, ip, cfg.DesiredStateStabilityCycles)
		if !failed {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := ec.MemberRemove(ctx, m.ID); err != nil {
			pkglog.Warnf("remove failed member %s: %v", ip, err)
		}
		cancel()
	}

	// 7. Promote any learner members that are in the desired set and have caught up.
	// Learners join without disrupting quorum; the daemon promotes them once they
	// are reachable. etcd returns an error if the learner is not yet in sync —
	// we log a warning and retry on the next reconcile cycle.
	// NOTE: learners return "rpc not supported for learner" for MemberList gRPC
	// calls, so we use the HTTP /health endpoint to check liveness instead.
	for _, m := range members {
		if !m.IsLearner || m.Name == "" {
			continue
		}
		learnerIP := extractHostFromURL(m.ClientURLs)
		if learnerIP == "" || !containsString(candidateIPs(cfg), learnerIP) {
			pkglog.Infof("skipping learner promotion for %s: ip=%q not in desired set", m.Name, learnerIP)
			continue
		}
		healthEndpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), learnerIP, cfg.HostEtcdClientPort)
		if !etcdHealthReachable(healthEndpoint, cfg) {
			pkglog.Infof("learner %s (%s) not yet healthy — will retry promotion", m.Name, learnerIP)
			continue
		}
		pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := ec.MemberPromote(pctx, m.ID); err != nil {
			pkglog.Warnf("promote learner %s (id=%s): %v — will retry", m.Name, m.ID, err)
		} else {
			pkglog.Infof("promoted learner %s (id=%s) to full voter", m.Name, m.ID)
		}
		pcancel()
	}

	if !stable {
		pkglog.Infof("state not stable — skipping disruptive reconciliation")
		return
	}
}

// pickHealthyEtcdEndpoint tries local then external endpoint via the /health
// HTTP endpoint (200 or 503 both mean the process is up). Returns the first
// reachable endpoint or empty string.
func pickHealthyEtcdEndpoint(cfg *config.Config, local, external string) string {
	for _, ep := range []string{local, external} {
		if etcdHealthReachable(ep, cfg) {
			return ep
		}
	}
	return ""
}

func etcdHealthReachable(endpoint string, cfg *config.Config) bool {
	// Append /health to the endpoint as-is; the port is already correct
	// (EtcdClientPort for local, HostEtcdClientPort for external peers).
	url := strings.TrimRight(endpoint, "/") + "/health"

	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	if cfg.SSLEnabled {
		// etcd has --client-cert-auth; we must present a client certificate or
		// the TLS handshake will be rejected (making the process look "down").
		if cert, err := tls.LoadX509KeyPair(
			"/etc/ssl/cluster/etcd/client.crt",
			"/etc/ssl/cluster/etcd/client.key",
		); err == nil {
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
	}

	code, err := httpStatus(url, tlsCfg, 5*time.Second)
	if err != nil {
		return false
	}
	return code == 200 || code == 503
}

// httpStatus does a minimal GET and returns HTTP status code.
func httpStatus(url string, tlsCfg *tls.Config, timeout time.Duration) (int, error) {
	tr := &http.Transport{TLSClientConfig: tlsCfg, TLSHandshakeTimeout: 2 * time.Second}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// updateStateTracker reads/writes the stability tracker file and returns
// (isStable, stableCount).
func updateStateTracker(file, desired string, threshold int) (bool, int) {
	lastSig := ""
	count := 0
	if data, err := os.ReadFile(file); err == nil {
		lines := strings.SplitN(string(data), "\n", 3)
		if len(lines) >= 1 {
			lastSig = strings.TrimSpace(lines[0])
		}
		if len(lines) >= 2 {
			count, _ = strconv.Atoi(strings.TrimSpace(lines[1]))
		}
	}
	if desired == lastSig {
		count++
	} else {
		count = 1
	}
	_ = os.WriteFile(file, []byte(fmt.Sprintf("%s\n%d\n", desired, count)), 0o644)
	return count >= threshold, count
}

func shouldFNFNow(cooldownFile string, cooldownSecs int) bool {
	if cooldownSecs <= 0 {
		return true
	}
	data, err := os.ReadFile(cooldownFile)
	if err != nil {
		return true
	}
	last, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	elapsed := time.Now().Unix() - last
	if int(elapsed) < cooldownSecs {
		pkglog.Infof("force-new-cluster cooldown active (%ds/%ds), skipping", elapsed, cooldownSecs)
		return false
	}
	return true
}

func triggerForceNewCluster(cfg *config.Config, sslOpts []string, systemID, flagFile, cooldownFile, noQuorumFile string) {
	if !cfg.DeadClusterRecovery || anyDCSQuorum(cfg) {
		pkglog.Infof("control-plane rebuild cancelled: recovery disabled or quorum has returned")
		return
	}

	pkglog.Section("QUORUM RECOVERY: TRIGGERING FORCE-NEW-CLUSTER")
	_ = os.WriteFile(flagFile, []byte("1"), 0o644)
	_ = os.WriteFile(cooldownFile, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
	_ = os.WriteFile(noQuorumFile, []byte("0"), 0o644)
	supervisorctl("restart", "etcd")
	localEndpoint := fmt.Sprintf("%s://127.0.0.1:%d", cfg.EtcdProtocol(), cfg.EtcdClientPort)
	ec := etcdmgr.New(localEndpoint, sslOpts)
	// Wait for etcd to come up
	etcdReady := false
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := ec.MemberList(ctx)
		cancel()
		if err == nil {
			pkglog.Infof("etcd is up after force-new-cluster restart")
			etcdReady = true
			break
		}
	}
	if !etcdReady {
		pkglog.Errorf("force-new-cluster restart did not become ready; preserving Patroni DCS keys for the next recovery attempt")
		return
	}
	// Preserve durable dynamic configuration but remove all ephemeral Patroni
	// election/health state from the dead epoch.
	prefix := fmt.Sprintf("/patroni/%s", cfg.PatroniScope)
	pkglog.Infof("wiping stale Patroni DCS election state")
	for _, key := range []string{"leader", "status", "failsafe", "failover", "sync"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = ec.RM(ctx, prefix+"/"+key)
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = ec.RMRecursive(ctx, prefix+"/members")
	cancel()
	if systemID != "" {
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		if err := ec.Set(ctx, prefix+"/initialize", systemID); err != nil {
			pkglog.Errorf("failed to set recovered PostgreSQL system ID in DCS: %v", err)
		}
		cancel()
	}
	pkglog.Infof("quorum recovery complete — new nodes can now join")
}

var requiredPatroniHBA = []string{
	"local all postgres peer",
	"hostssl replication replicator 0.0.0.0/0 cert clientcert=verify-full",
	"hostssl all all 0.0.0.0/0 md5",
	"host replication replicator 0.0.0.0/0 md5",
	"host all all 0.0.0.0/0 md5",
}

// mergePatroniRecoveryConfig repairs settings that are otherwise applied only
// by Patroni's first bootstrap. A physical restore preserves PGDATA but may be
// paired with an older or incomplete DCS snapshot, so these invariants must be
// continuously reconciled.
func mergePatroniRecoveryConfig(raw string, cfg *config.Config) (string, bool, error) {
	root := map[string]interface{}{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &root); err != nil {
			return "", false, fmt.Errorf("decode Patroni DCS config: %w", err)
		}
	}
	before, err := json.Marshal(root)
	if err != nil {
		return "", false, fmt.Errorf("encode original Patroni DCS config: %w", err)
	}

	setDefault := func(key string, value interface{}) {
		if _, ok := root[key]; !ok {
			root[key] = value
		}
	}
	setDefault("ttl", cfg.PatroniTTL)
	setDefault("loop_wait", cfg.PatroniLoopWait)
	setDefault("retry_timeout", cfg.PatroniRetryTimeout)
	setDefault("maximum_lag_on_failover", cfg.PatroniMaxLag)
	// A preinitialized seed may reach DCS before Patroni's bootstrap callback.
	// Preserve configured durability/HA policy even when we create /config first.
	setDefault("synchronous_mode", cfg.PatroniSynchronousMode)
	setDefault("synchronous_mode_strict", cfg.PatroniSynchronousModeStrict)
	setDefault("synchronous_node_count", cfg.PatroniSynchronousNodeCount)
	setDefault("failsafe_mode", cfg.PatroniFailsafeMode)
	setDefault("primary_start_timeout", cfg.PatroniMasterStartTimeout)
	setDefault("primary_stop_timeout", cfg.PatroniMasterStopTimeout)

	postgresql, ok := root["postgresql"].(map[string]interface{})
	if !ok {
		postgresql = map[string]interface{}{}
		root["postgresql"] = postgresql
	}
	postgresql["use_pg_rewind"] = true
	if _, ok := postgresql["use_slots"]; !ok {
		postgresql["use_slots"] = cfg.PatroniUseSlots
	}
	existingHBA := make([]string, 0)
	switch values := postgresql["pg_hba"].(type) {
	case []interface{}:
		for _, value := range values {
			if rule, ok := value.(string); ok {
				existingHBA = append(existingHBA, rule)
			}
		}
	case []string:
		existingHBA = append(existingHBA, values...)
	}
	for _, required := range requiredPatroniHBA {
		if !containsString(existingHBA, required) {
			existingHBA = append(existingHBA, required)
		}
	}
	postgresql["pg_hba"] = existingHBA

	encoded, err := json.Marshal(root)
	if err != nil {
		return "", false, fmt.Errorf("encode Patroni DCS config: %w", err)
	}
	if string(before) == string(encoded) {
		return raw, false, nil
	}
	return string(encoded), true, nil
}

func reconcilePatroniDCSConfig(cfg *config.Config, ec *etcdmgr.Client) {
	key := fmt.Sprintf("/patroni/%s/config", cfg.PatroniScope)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	raw, err := ec.Get(ctx, key)
	cancel()
	if err != nil {
		pkglog.Warnf("read Patroni DCS config for recovery reconciliation: %v", err)
		return
	}
	merged, changed, err := mergePatroniRecoveryConfig(raw, cfg)
	if err != nil {
		pkglog.Errorf("Patroni DCS recovery reconciliation blocked: %v", err)
		return
	}
	if !changed {
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	err = ec.Set(ctx, key, merged)
	cancel()
	if err != nil {
		pkglog.Warnf("write reconciled Patroni DCS config: %v", err)
		return
	}
	pkglog.Infof("reconciled Patroni DCS pg_hba and pg_rewind settings")
}

const postgresCredentialsMarker = "/tmp/postgres-credentials-reconciled"

func sqlLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func postgresCredentialSQL(cfg *config.Config) string {
	return fmt.Sprintf(`
DO $do$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'replicator') THEN
    CREATE ROLE replicator WITH LOGIN REPLICATION;
  END IF;
END
$do$;
ALTER ROLE replicator WITH LOGIN REPLICATION PASSWORD %s;
ALTER ROLE postgres WITH LOGIN SUPERUSER REPLICATION PASSWORD %s;
`, sqlLiteral(cfg.PostgresReplicationPassword), sqlLiteral(cfg.PostgresSuperuserPassword))
}

func localPSQL(port int, sql string) ([]byte, error) {
	cmd := exec.Command("su", "-s", "/bin/bash", "postgres", "-c",
		fmt.Sprintf("exec psql -X -h /var/run/postgresql -p %d -d postgres -v ON_ERROR_STOP=1 -At", port))
	cmd.Stdin = strings.NewReader(sql)
	return cmd.CombinedOutput()
}

// reconcilePostgresCredentials runs once per container lifetime, and only on
// the writable primary. Roles are database state and therefore come from the
// backup; environment secrets are deployment state. Re-applying them after a
// restore lets replicas clone without operator SQL.
func reconcilePostgresCredentials(cfg *config.Config) {
	if fileExists(postgresCredentialsMarker) || !dirExists("/var/lib/postgresql/data/global") {
		return
	}
	out, err := localPSQL(cfg.PostgresPort, "SELECT CASE WHEN pg_is_in_recovery() THEN 'replica' ELSE 'primary' END;\n")
	if err != nil || strings.TrimSpace(string(out)) != "primary" {
		return
	}
	out, err = localPSQL(cfg.PostgresPort, postgresCredentialSQL(cfg))
	if err != nil {
		pkglog.Errorf("failed to reconcile PostgreSQL cluster roles after restore: %v (output=%s)", err, strings.TrimSpace(string(out)))
		return
	}
	creationMarker := filepath.Join(postgresDataDir, ".flux-agent", "first-creation")
	if fileExists(creationMarker) {
		out, err := localPSQL(cfg.PostgresPort, initialApplicationSQL(cfg))
		if err != nil {
			pkglog.Errorf("first-creation application setup failed: %v (output=%s)", err, strings.TrimSpace(string(out)))
			return
		}
		if err := os.Remove(creationMarker); err != nil {
			pkglog.Errorf("record first-creation application setup: %v", err)
			return
		}
	}
	if err := os.WriteFile(postgresCredentialsMarker, []byte("ok\n"), 0o600); err != nil {
		pkglog.Warnf("write PostgreSQL credential reconciliation marker: %v", err)
	}
	pkglog.Infof("reconciled postgres and replicator roles from configured credentials")
}

func initialApplicationSQL(cfg *config.Config) string {
	sql := fmt.Sprintf(`
DO $do$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin') THEN
    CREATE ROLE admin WITH LOGIN CREATEROLE CREATEDB;
  END IF;
END
$do$;
ALTER ROLE admin WITH PASSWORD %s;
`, sqlLiteral(cfg.PostgresSuperuserPassword))
	if cfg.PostgresDB != "" && cfg.PostgresDB != "postgres" {
		sql += fmt.Sprintf("SELECT format('CREATE DATABASE %%I OWNER admin', %s) WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname=%s) \\gexec\n", sqlLiteral(cfg.PostgresDB), sqlLiteral(cfg.PostgresDB))
	}
	return sql
}

// checkPatroniSystemID repairs only stale DCS metadata toward the pinned identity.
func checkPatroniSystemID(cfg *config.Config, ec *etcdmgr.Client, _ []string, _ string, _ int) {
	expected, err := expectedSystemID(cfg)
	if err != nil {
		pkglog.Errorf("system-ID reconciliation blocked: %v", err)
		return
	}
	key := fmt.Sprintf("/patroni/%s/initialize", cfg.PatroniScope)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	old, err := ec.Get(ctx, key)
	cancel()
	if err != nil || old == "" || old == expected {
		return
	}
	pc := patroni.New(cfg.SSLEnabled, cfg.HostPatroniAPIPort)
	defer pc.Close()
	primaries := map[string]string{}
	for _, ip := range candidateIPs(cfg) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		host, port := patroniProbeTarget(cfg, ip)
		client := pc
		if port == cfg.PatroniAPIPort {
			client = patroni.New(cfg.SSLEnabled, port)
		}
		info, err := client.GetInfo(ctx, host)
		if client != pc {
			client.Close()
		}
		if err == nil && info.State == "running" && (info.Role == "primary" || info.Role == "master") {
			primaries[ip] = info.DatabaseSystemIdentifier
		}
		cancel()
	}
	if selectPinnedPrimary(expected, primaries) == "" {
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ec.CompareAndSwap(ctx, key, old, expected); err != nil {
		pkglog.Warnf("repair pinned initialize identity: %v", err)
	}
	// Conflicting PGDATA is always preserved. Neither a peer's role nor the
	// legacy ALLOW_PG_DATA_WIPE flag authorizes deleting a different database.
}

type postgresControlData struct {
	SystemID                string
	State                   string
	CheckpointLSN           string
	CheckpointTimeline      uint64
	MinimumRecoveryLSN      string
	MinimumRecoveryTimeline uint64
}

type postgresRecoveryPosition struct {
	Timeline uint64
	LSN      uint64
}

func parsePostgresLSN(value string) (uint64, error) {
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid PostgreSQL LSN %q", value)
	}
	high, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid PostgreSQL LSN %q: %w", value, err)
	}
	low, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid PostgreSQL LSN %q: %w", value, err)
	}
	return high<<32 | low, nil
}

func formatPostgresLSN(value uint64) string {
	return fmt.Sprintf("%X/%X", value>>32, value&0xffffffff)
}

func parsePostgresControlData(out string) postgresControlData {
	var control postgresControlData
	for _, raw := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(raw), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "Database system identifier":
			control.SystemID = value
		case "Database cluster state":
			control.State = value
		case "Latest checkpoint location":
			control.CheckpointLSN = value
		case "Latest checkpoint's TimeLineID":
			control.CheckpointTimeline, _ = strconv.ParseUint(value, 10, 64)
		case "Minimum recovery ending location":
			control.MinimumRecoveryLSN = value
		case "Min recovery ending loc's timeline":
			control.MinimumRecoveryTimeline, _ = strconv.ParseUint(value, 10, 64)
		}
	}
	return control
}

func (control postgresControlData) recoveryPosition() (postgresRecoveryPosition, error) {
	var best postgresRecoveryPosition
	consider := func(timeline uint64, lsnText string) {
		lsn, err := parsePostgresLSN(lsnText)
		if err != nil || timeline == 0 || lsn == 0 {
			return
		}
		if timeline > best.Timeline || (timeline == best.Timeline && lsn > best.LSN) {
			best = postgresRecoveryPosition{Timeline: timeline, LSN: lsn}
		}
	}
	consider(control.CheckpointTimeline, control.CheckpointLSN)
	consider(control.MinimumRecoveryTimeline, control.MinimumRecoveryLSN)
	if best.Timeline == 0 || best.LSN == 0 {
		return postgresRecoveryPosition{}, fmt.Errorf("no durable PostgreSQL recovery position in pg_controldata")
	}
	return best, nil
}

// readLocalPGControlData reads durable PostgreSQL identity and recovery
// progress without starting PostgreSQL. This remains available while etcd and
// Patroni are down during control-plane recovery.
func readLocalPGControlData() (postgresControlData, error) {
	// Find pg_controldata across common PostgreSQL versions.
	candidates := []string{
		"/usr/lib/postgresql/16/bin/pg_controldata",
		"/usr/lib/postgresql/15/bin/pg_controldata",
		"/usr/lib/postgresql/14/bin/pg_controldata",
		"/usr/lib/postgresql/13/bin/pg_controldata",
	}
	var pgCtl string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			pgCtl = c
			break
		}
	}
	if pgCtl == "" {
		return postgresControlData{}, fmt.Errorf("pg_controldata not found")
	}
	out, err := exec.Command(pgCtl, "/var/lib/postgresql/data").Output()
	if err != nil {
		return postgresControlData{}, fmt.Errorf("pg_controldata: %w", err)
	}
	control := parsePostgresControlData(string(out))
	if control.SystemID == "" {
		return postgresControlData{}, fmt.Errorf("system identifier not found in pg_controldata output")
	}
	return control, nil
}

// readLocalPGSystemID reads the PostgreSQL system identifier from pg_controldata.
func readLocalPGSystemID() (string, error) {
	control, err := readLocalPGControlData()
	if err != nil {
		return "", err
	}
	return control.SystemID, nil
}

func membersIPs(members []etcdmgr.Member, clientPort int) []string {
	var out []string
	for _, m := range members {
		if m.ClientURLs == "" {
			continue
		}
		ip := extractHostFromURL(m.ClientURLs)
		if ip != "" {
			out = append(out, ip)
		}
	}
	return out
}

func handleEtcdUnreachable(cfg *config.Config, sslOpts, desiredIPs []string, desiredStable bool,
	unavailFile, fnfFlagFile, fnfCooldownFile, noQuorumFile string, fnfCooldownSecs int) {
	count := 0
	if data, err := os.ReadFile(unavailFile); err == nil {
		count, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	count++
	_ = os.WriteFile(unavailFile, []byte(strconv.Itoa(count)), 0o644)
	pkglog.Infof("etcd unavailable counter: %d/%d", count, cfg.EtcdUnavailableRecoveryCycles)

	// Check peer evidence to decide action
	reachable, knowUs, dontKnowUs := 0, 0, 0
	for _, ip := range candidateIPs(cfg) {
		if ip == cfg.MyIP {
			continue
		}
		endpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), ip, cfg.HostEtcdClientPort)
		ec := etcdmgr.New(endpoint, sslOpts)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		members, err := ec.MemberList(ctx)
		cancel()
		if err != nil {
			continue
		}
		reachable++
		if etcdmgr.FindByName(members, cfg.MyName) != nil {
			knowUs++
		} else {
			dontKnowUs++
		}
	}
	pkglog.Infof("peer evidence while etcd unavailable: reachable=%d know_us=%d dont_know_us=%d", reachable, knowUs, dontKnowUs)

	// A restored cluster or total loss of ephemeral etcd commonly leaves no
	// working endpoint. Recover only after the Flux membership is stable and
	// every authenticated identity endpoint agrees on a safe authority. This
	// blocks force-new during partitions and ambiguous multi-primary states.
	if count >= cfg.EtcdUnavailableRecoveryCycles && desiredStable && cfg.DeadClusterRecovery && reachable == 0 {
		authority, safe, reason := confirmRecoveryAuthorityWithPeers(cfg)
		if !safe {
			pkglog.Errorf("AUTOMATIC DEAD-CLUSTER RECOVERY BLOCKED: %s", reason)
		} else if authority.IP == cfg.MyIP {
			if shouldFNFNow(fnfCooldownFile, fnfCooldownSecs) {
				pkglog.Warnf("automatic dead-cluster recovery authorized: %s", reason)
				triggerForceNewCluster(cfg, sslOpts, authority.SystemID,
					fnfFlagFile, fnfCooldownFile, noQuorumFile)
			}
			return
		} else {
			if !etcdRestartCooldownExpired() {
				return
			}
			pkglog.Infof("waiting for recovery authority %s; restarting etcd for identity-checked rejoin", authority.NodeName)
			markEtcdRestart()
			supervisorctl("restart", "etcd")
			return
		}
	}

	if count >= cfg.EtcdUnavailableRecoveryCycles && reachable > 0 {
		if dontKnowUs > knowUs {
			if !etcdRestartCooldownExpired() {
				return
			}
			pkglog.Warnf("peer membership differs; restarting for identity-checked rejoin without deleting live etcd data")
			markEtcdRestart()
			supervisorctl("restart", "etcd")
			return
		}
		if knowUs > 0 {
			if !etcdRestartCooldownExpired() {
				pkglog.Infof("peers know us but recent etcd restart — waiting for it to settle")
				return
			}
			pkglog.Infof("peers know us — restarting etcd")
			markEtcdRestart()
			supervisorctl("restart", "etcd")
			_ = os.WriteFile(unavailFile, []byte("0"), 0o644)
			return
		}
	}
}

const etcdRestartMarkerFile = "/tmp/etcd-last-restart"
const etcdRestartCooldownSecs = 90

func etcdRestartCooldownExpired() bool {
	data, err := os.ReadFile(etcdRestartMarkerFile)
	if err != nil {
		return true
	}
	t, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return true
	}
	return time.Now().Unix()-t > etcdRestartCooldownSecs
}

func markEtcdRestart() {
	_ = os.WriteFile(etcdRestartMarkerFile, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
}

// updateClusterEnv keeps /etc/cluster_env and patroni.yml in sync with the
// desired IPs so a Patroni restart picks up the right hosts.
func updateClusterEnv(cfg *config.Config, desiredIPs []string) error {
	previousIdentity := cfg.ExpectedSystemID
	// Another process may have authenticated an empty replacement since this
	// daemon started. Never overwrite its learned identity with our old snapshot.
	if err := restoreReplacementIdentity(cfg, replacementIdentityFile); err != nil {
		return err
	}
	ips, err := discoveryIPsAt(cfg, postgresDataDir, desiredIPs)
	if err != nil {
		return err
	}
	newHosts, newPatroniHosts, newInitial := buildEtcdTopology(cfg, ips)
	if newInitial == cfg.EtcdInitialCluster && newHosts == cfg.EtcdHosts && newPatroniHosts == cfg.PatroniEtcdHosts && cfg.ExpectedSystemID == previousIdentity {
		return nil
	}
	cfg.EtcdHosts = newHosts
	cfg.PatroniEtcdHosts = newPatroniHosts
	cfg.EtcdInitialCluster = newInitial
	if err := cfg.WriteClusterEnv(); err != nil {
		return err
	}
	updatePatroniYAMLHosts("/etc/patroni/patroni.yml", newPatroniHosts)
	return nil
}

func updatePatroniYAMLHosts(path, hosts string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	out := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "hosts:") {
			out = append(out, "  hosts: "+hosts)
		} else {
			out = append(out, line)
		}
	}
	_ = os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644)
	// Send SIGHUP to Patroni to reload
	pid := findPatroniPID()
	if pid > 0 {
		_ = exec.Command("kill", "-HUP", strconv.Itoa(pid)).Run()
		pkglog.Infof("sent SIGHUP to Patroni (pid=%d)", pid)
	}
}

func findPatroniPID() int {
	out, err := exec.Command("pgrep", "-f", "patroni").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil {
			return pid
		}
	}
	return 0
}

func supervisorctl(args ...string) {
	cmd := exec.Command("supervisorctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		pkglog.Warnf("supervisorctl %v: %v", args, err)
	}
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
