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
		pkglog.Warnf("no IPs from Flux API, skipping cycle")
		return
	}
	desiredSignature := strings.Join(desiredIPs, ",")
	stable, stableCount := updateStateTracker(stateTrackFile, desiredSignature, cfg.DesiredStateStabilityCycles)
	pkglog.Infof("desired IPs (%d): %v — stable=%v (%d/%d)", len(desiredIPs), desiredIPs, stable, stableCount, cfg.DesiredStateStabilityCycles)

	// Always update cluster_env and patroni.yml with the latest peer list from
	// Flux API, regardless of stability or etcd health. This ensures a node
	// that starts while the list is in flux (or while etcd is down) always has
	// the correct ETCD_INITIAL_CLUSTER / etcd hosts on its next etcd restart.
	updateClusterEnv(cfg, desiredIPs)

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
	}

	// 4. Force-new-cluster check. Never choose a survivor merely because it has
	// PGDATA: during an ordinary quorum loss every replica has PGDATA. Automatic
	// recovery requires authenticated agreement on either a sole data copy or,
	// after total DCS loss, one durable primary among matching replicas.
	if !hasQuorum && noQuorumCount >= cfg.DesiredStateStabilityCycles && stable && dirExists("/var/lib/postgresql/data/global") {
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

	// Split-brain detection: if local has 1 member but multiple expected, restart etcd
	if len(members) <= 1 && len(desiredIPs) > 1 {
		pkglog.Warnf("local etcd shows %d member(s) but %d expected — checking for split-brain", len(members), len(desiredIPs))
		for _, ip := range desiredIPs {
			if ip == cfg.MyIP {
				continue
			}
			peerEndpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), ip, cfg.HostEtcdClientPort)
			pec := etcdmgr.New(peerEndpoint, sslOpts)
			pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
			peerMembers, _ := pec.MemberList(pctx)
			pcancel()
			if len(peerMembers) > len(members) {
				if !etcdRestartCooldownExpired() {
					pkglog.Infof("split-brain hint from peer %s but recent etcd restart — waiting", ip)
					return
				}
				pkglog.Warnf("SPLIT-BRAIN DETECTED — peer %s has %d members vs our %d, restarting etcd",
					ip, len(peerMembers), len(members))
				markEtcdRestart()
				supervisorctl("restart", "etcd")
				return
			}
		}
	}

	// Ghost member cleanup
	for _, m := range members {
		if !m.Unstarted {
			continue
		}
		ghostIP := extractHostFromURL(m.PeerURLs)
		if containsString(desiredIPs, ghostIP) {
			pkglog.Infof("ghost member %s (id=%s) still in desired state — may be starting up", ghostIP, m.ID)
			continue
		}
		pkglog.Infof("removing ghost member %s (id=%s) — not in desired state", ghostIP, m.ID)
		gctx, gcancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = ec.MemberRemove(gctx, m.ID)
		gcancel()
	}

	// Re-fetch member list after ghost cleanup
	mctx2, mcancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	members, _ = ec.MemberList(mctx2)
	mcancel2()

	// Cluster ID mismatch self-healing (majority vote among peers)
	if reachable, matching, mismatched := evaluatePeerVotes(cfg, sslOpts, desiredIPs); reachable > 0 {
		pkglog.Infof("peer verification: reachable=%d matching=%d mismatched=%d", reachable, matching, mismatched)
		if mismatched > matching {
			if !etcdRestartCooldownExpired() {
				pkglog.Infof("peers disagree but recent etcd restart — waiting")
				return
			}
			pkglog.Warnf("majority of peers disagree — restarting local etcd for self-healing rejoin")
			_ = wipeDir("/var/lib/etcd")
			markEtcdRestart()
			supervisorctl("restart", "etcd")
			return
		}
	}

	// Stale Patroni leader cleanup
	checkStalePatroniLeader(cfg, ec)

	// Patroni system ID mismatch self-healing.
	// If the etcd /initialize key doesn't match the running primary's system ID,
	// the cluster is in a split-identity state (e.g. after a dead-cluster recovery
	// where one node briefly bootstrapped a fresh PG then yielded to an existing
	// primary). Fix: update the etcd key to the primary's authoritative system ID
	// so replicas with the correct data can rejoin without wiping.
	// If this node's local PG data has a mismatched system ID (and a healthy
	// primary exists), wipe local data so Patroni will pg_basebackup fresh.
	checkPatroniSystemID(cfg, ec, desiredIPs, pgWipeCooldownFile, pgWipeCooldownSecs)

	// 6. Reconcile membership
	// Members not in the Flux API are removed:
	//   - Immediately if they are also unreachable (departed node — no reason to wait)
	//   - After state stability if they are still reachable (safety gate against transient API errors)
	currentMembers := membersIPs(members, cfg.HostEtcdClientPort)
	pkglog.Infof("current etcd members: %v", currentMembers)

	for _, current := range currentMembers {
		if containsString(desiredIPs, current) {
			continue
		}
		reachable := isPeerReachable(cfg, sslOpts, current)
		if !stable && reachable {
			pkglog.Infof("member %s not in desired state but still reachable and state not yet stable — skipping", current)
			continue
		}
		if !reachable {
			pkglog.Infof("member %s not in desired state AND unreachable — removing immediately", current)
		} else {
			pkglog.Infof("member %s NOT in desired state — removing (stable)", current)
		}
		m := etcdmgr.FindByClientIP(members, fmt.Sprintf("%s:%d", current, cfg.HostEtcdClientPort))
		if m != nil && m.ID != "" {
			rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := ec.MemberRemove(rctx, m.ID); err != nil {
				pkglog.Warnf("remove %s (%s): %v", current, m.ID, err)
			} else {
				pkglog.Infof("removed %s (id=%s)", current, m.ID)
			}
			rcancel()
		}
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
		if learnerIP == "" || !containsString(desiredIPs, learnerIP) {
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
	tr := &http.Transport{TLSClientConfig: tlsCfg}
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

	postgresql, ok := root["postgresql"].(map[string]interface{})
	if !ok {
		postgresql = map[string]interface{}{}
		root["postgresql"] = postgresql
	}
	postgresql["use_pg_rewind"] = true
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
	if err := os.WriteFile(postgresCredentialsMarker, []byte("ok\n"), 0o600); err != nil {
		pkglog.Warnf("write PostgreSQL credential reconciliation marker: %v", err)
	}
	pkglog.Infof("reconciled postgres and replicator roles from configured credentials")
}

// isPeerReachable returns true if the etcd client endpoint on ip responds within 3 seconds.
func isPeerReachable(cfg *config.Config, sslOpts []string, ip string) bool {
	endpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), ip, cfg.HostEtcdClientPort)
	ec := etcdmgr.New(endpoint, sslOpts)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := ec.MemberList(ctx)
	return err == nil
}

func evaluatePeerVotes(cfg *config.Config, sslOpts, desiredIPs []string) (reachable, matching, mismatched int) {
	for _, ip := range desiredIPs {
		if ip == cfg.MyIP {
			continue
		}
		endpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), ip, cfg.HostEtcdClientPort)
		ec := etcdmgr.New(endpoint, sslOpts)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		peerMembers, err := ec.MemberList(ctx)
		cancel()
		if err != nil {
			continue
		}
		reachable++
		if etcdmgr.FindByName(peerMembers, cfg.MyName) != nil {
			matching++
		} else {
			mismatched++
		}
	}
	return
}

func checkStalePatroniLeader(cfg *config.Config, ec *etcdmgr.Client) {
	if !dirExists("/var/lib/postgresql/data/global") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	leader, err := ec.Get(ctx, fmt.Sprintf("/patroni/%s/leader", cfg.PatroniScope))
	cancel()
	if err != nil || leader == "" || leader == cfg.MyName {
		return
	}
	leaderIP := strings.ReplaceAll(strings.TrimPrefix(leader, "node-"), "-", ".")
	pc := patroni.New(cfg.SSLEnabled, cfg.HostPatroniAPIPort)
	hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer hcancel()
	if pc.IsAlive(hctx, leaderIP) {
		return
	}
	pkglog.Warnf("Patroni leader %s unreachable at %s — clearing stale leader key", leader, leaderIP)
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = ec.RM(rctx, fmt.Sprintf("/patroni/%s/leader", cfg.PatroniScope))
	rcancel()
}

// checkPatroniSystemID detects and heals two related split-identity scenarios:
//
//  1. The etcd /initialize key doesn't match the running primary's system ID
//     (e.g. after a dead-cluster recovery where one node briefly bootstrapped a
//     fresh PG, set /initialize, then yielded to a surviving primary with an
//     older system ID). Action: update /initialize to the primary's system ID so
//     replicas with matching data can rejoin without wiping.
//
//  2. This node's local PG data has a system ID that doesn't match /initialize
//     AND there is at least one healthy cluster member (primary or replica) that
//     confirms the correct system ID. The data is from the wrong cluster epoch.
//     Action: wipe local PG data and restart Patroni so it pg_basebackup.
//
// Note: a running replica is sufficient evidence for case 2. We do not require
// the primary to be reachable — waiting for it would leave crash-looping nodes
// unhealed whenever the primary is on a different network segment.
func checkPatroniSystemID(cfg *config.Config, ec *etcdmgr.Client, desiredIPs []string, wipeCooldownFile string, wipeCooldownSecs int) {
	initKey := fmt.Sprintf("/patroni/%s/initialize", cfg.PatroniScope)

	// Read the etcd cluster-initialize system identifier.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	etcdSysID, err := ec.Get(ctx, initKey)
	cancel()
	if err != nil || etcdSysID == "" {
		// Not yet initialised — nothing to heal.
		return
	}

	// Poll all desired peers for Patroni info. Track the primary separately
	// (needed for Case 1) and any member whose system ID matches /initialize
	// (sufficient for Case 2).
	pc := patroni.New(cfg.SSLEnabled, cfg.HostPatroniAPIPort)
	primarySysID := ""
	primaryIP := ""
	anyMatchingMember := false // any peer running the cluster with etcdSysID

	for _, ip := range desiredIPs {
		ictx, icancel := context.WithTimeout(context.Background(), 5*time.Second)
		info, infoErr := pc.GetInfo(ictx, ip)
		icancel()
		if infoErr != nil || info == nil || info.DatabaseSystemIdentifier == "" {
			continue
		}
		// Accept both "primary" (Patroni 3+) and "master" (older Patroni).
		if info.Role == "primary" || info.Role == "master" {
			primarySysID = info.DatabaseSystemIdentifier
			primaryIP = ip
		}
		if info.DatabaseSystemIdentifier == etcdSysID {
			anyMatchingMember = true
		}
	}

	// Case 1: etcd /initialize key is stale (doesn't match the running primary).
	// Only update when we have confirmed the primary and at least one other
	// member also carries the primary's system ID (majority consensus).
	if primarySysID != "" && primarySysID != etcdSysID && anyMatchingMember {
		// anyMatchingMember means some peer already agreed with etcdSysID, so
		// the primary is the outlier — don't override yet.
		// If NO member matches etcdSysID, the initialize key is genuinely stale.
	} else if primarySysID != "" && primarySysID != etcdSysID && !anyMatchingMember {
		pkglog.Warnf("system ID mismatch: etcd /initialize=%s but primary %s reports %s and no member confirms /initialize — updating initialize key",
			etcdSysID, primaryIP, primarySysID)
		uctx, ucancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = ec.Set(uctx, initKey, primarySysID)
		ucancel()
		// Treat the updated value as authoritative going forward.
		etcdSysID = primarySysID
		anyMatchingMember = true
	}

	// Case 2: local PG data has a system ID from a different cluster epoch.
	//
	// This deletes the local PostgreSQL data directory, so getting the
	// "authoritative" system ID wrong destroys real data. A freshly-bootstrapped
	// EMPTY cluster publishes its own system ID to /initialize; if we then trust
	// a single re-cloned replica as confirmation, we will delete the *real* data
	// on every surviving node. That is exactly how a production cluster was
	// emptied. Two guards prevent a recurrence:
	//
	//   1. Require the live PRIMARY (not merely "any member") to confirm the
	//      authoritative system ID before we treat the local data as stale.
	//      A replica that was itself re-cloned from a bad epoch is not enough.
	//   2. Never auto-wipe unless ALLOW_PG_DATA_WIPE is explicitly enabled. By
	//      default we log a loud, actionable error and leave the data intact so
	//      a human can confirm the authoritative copy before anything is deleted.
	primaryConfirmsSysID := primaryIP != "" && primarySysID != "" && primarySysID == etcdSysID
	if !primaryConfirmsSysID {
		return
	}
	if cfg.MyIP == primaryIP {
		return
	}
	if !dirExists("/var/lib/postgresql/data/global") {
		return
	}
	localSysID, localErr := readLocalPGSystemID()
	if localErr != nil {
		pkglog.Infof("could not read local PG system ID: %v", localErr)
		return
	}
	if localSysID == etcdSysID {
		return
	}
	if !cfg.AllowPGDataWipe {
		pkglog.Errorf("DATA SAFETY: local PG system ID %s != cluster system ID %s (confirmed by primary %s), "+
			"but ALLOW_PG_DATA_WIPE is disabled — refusing to delete /var/lib/postgresql/data. "+
			"If this node genuinely holds stale data, verify the authoritative copy first, then either remove "+
			"this node's data dir manually or set ALLOW_PG_DATA_WIPE=true so Patroni can re-clone it.",
			localSysID, etcdSysID, primaryIP)
		return
	}
	pkglog.Warnf("local PG system ID %s != cluster system ID %s — wiping data for clean pg_basebackup (ALLOW_PG_DATA_WIPE=true)",
		localSysID, etcdSysID)
	if !shouldFNFNow(wipeCooldownFile, wipeCooldownSecs) {
		pkglog.Infof("PG wipe cooldown active — skipping wipe this cycle")
		return
	}
	_ = os.WriteFile(wipeCooldownFile, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
	supervisorctl("stop", "patroni")
	if err := wipeDir("/var/lib/postgresql/data"); err != nil {
		pkglog.Errorf("failed to wipe PG data: %v", err)
		return
	}
	supervisorctl("start", "patroni")
}

// readLocalPGSystemID reads the PostgreSQL system identifier from pg_controldata.
func readLocalPGSystemID() (string, error) {
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
		return "", fmt.Errorf("pg_controldata not found")
	}
	out, err := exec.Command(pgCtl, "/var/lib/postgresql/data").Output()
	if err != nil {
		return "", fmt.Errorf("pg_controldata: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Database system identifier") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1]), nil
			}
		}
	}
	return "", fmt.Errorf("system identifier not found in pg_controldata output")
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
	for _, ip := range desiredIPs {
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
			// Every non-authority node drops only its empty/obsolete local etcd
			// state so etcd-start can register it as a learner with the authority.
			// PGDATA is never removed here; Patroni will retain or rewind replicas.
			if !etcdRestartCooldownExpired() {
				pkglog.Infof("waiting for recent etcd restart to settle before joining authority %s", authority.NodeName)
				return
			}
			pkglog.Warnf("PostgreSQL recovery authority is %s — resetting local etcd to join it as a learner", authority.NodeName)
			if err := wipeDir("/var/lib/etcd"); err != nil {
				pkglog.Errorf("failed to reset local etcd follower state: %v", err)
				return
			}
			markEtcdRestart()
			supervisorctl("restart", "etcd")
			_ = os.WriteFile(unavailFile, []byte("0"), 0o644)
			return
		}
	}

	if count >= cfg.EtcdUnavailableRecoveryCycles && reachable > 0 {
		if dontKnowUs > knowUs {
			if !etcdRestartCooldownExpired() {
				pkglog.Infof("peers don't know us but recent etcd restart — skipping wipe")
				return
			}
			pkglog.Warnf("majority of peers don't know us — wiping local etcd and rejoining")
			_ = wipeDir("/var/lib/etcd")
			markEtcdRestart()
			supervisorctl("restart", "etcd")
			_ = os.WriteFile(unavailFile, []byte("0"), 0o644)
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
func updateClusterEnv(cfg *config.Config, desiredIPs []string) {
	newHosts, newPatroniHosts, newInitial := buildEtcdTopology(cfg, desiredIPs)

	if newInitial == cfg.EtcdInitialCluster && newHosts == cfg.EtcdHosts && newPatroniHosts == cfg.PatroniEtcdHosts {
		return
	}
	cfg.EtcdHosts = newHosts
	cfg.PatroniEtcdHosts = newPatroniHosts
	cfg.EtcdInitialCluster = newInitial
	if err := cfg.WriteClusterEnv(); err != nil {
		pkglog.Warnf("write cluster env: %v", err)
	}
	// Update patroni.yml etcd hosts line
	updatePatroniYAMLHosts("/etc/patroni/patroni.yml", newPatroniHosts)
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
