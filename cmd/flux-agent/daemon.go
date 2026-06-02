package main

import (
	"context"
	"crypto/tls"
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

	sslOpts := etcdSSLOpts(cfg)
	fc := fluxapi.New(cfg.FluxAPIURL)

	for {
		pkglog.Section(fmt.Sprintf("CLUSTER UPDATE CYCLE - %s", time.Now().Format(time.RFC3339)))
		runReconcile(cfg, fc, sslOpts, *stateTrackFile, *noQuorumFile, *unavailFile, *fnfCooldownFile, *fnfFlagFile, fnfCooldownSecs)
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
	stateTrackFile, noQuorumFile, unavailFile, fnfCooldownFile, fnfFlagFile string, fnfCooldownSecs int) {

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

	// 2. Probe local etcd health (write-quorum probe)
	// Local etcd listens on ETCD_CLIENT_PORT (container-internal port).
	// Use the external IP with HOST_ETCD_CLIENT_PORT as a fallback — etcd
	// also binds 0.0.0.0, so the external IP works from inside the container.
	localEndpoint := fmt.Sprintf("%s://127.0.0.1:%d", cfg.EtcdProtocol(), cfg.EtcdClientPort)
	externalEndpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), cfg.MyIP, cfg.HostEtcdClientPort)
	endpoint := pickHealthyEtcdEndpoint(cfg, localEndpoint, externalEndpoint)

	if endpoint == "" {
		pkglog.Warnf("etcd not reachable on local or external endpoint")
		handleEtcdUnreachable(cfg, sslOpts, desiredIPs, unavailFile)
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
	}

	// 4. Force-new-cluster check
	if !hasQuorum && noQuorumCount >= cfg.DesiredStateStabilityCycles && stable && dirExists("/var/lib/postgresql/data/global") {
		if shouldFNFNow(fnfCooldownFile, fnfCooldownSecs) {
			triggerForceNewCluster(cfg, ec, fnfFlagFile, fnfCooldownFile, noQuorumFile)
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
	checkPatroniSystemID(cfg, ec, desiredIPs)

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

	if !stable {
		pkglog.Infof("state not stable — skipping env update")
		return
	}

	// New members will self-add when they start up; just update env file
	updateClusterEnv(cfg, desiredIPs)
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

func triggerForceNewCluster(cfg *config.Config, ec *etcdmgr.Client, flagFile, cooldownFile, noQuorumFile string) {
	pkglog.Section("QUORUM RECOVERY: TRIGGERING FORCE-NEW-CLUSTER")
	_ = os.WriteFile(flagFile, []byte("1"), 0o644)
	_ = os.WriteFile(cooldownFile, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
	_ = os.WriteFile(noQuorumFile, []byte("0"), 0o644)
	supervisorctl("restart", "etcd")
	// Wait for etcd to come up
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := ec.MemberList(ctx)
		cancel()
		if err == nil {
			pkglog.Infof("etcd is up after force-new-cluster restart")
			break
		}
	}
	// Wipe stale Patroni DCS keys so the leader can be re-acquired immediately
	pkglog.Infof("wiping stale Patroni DCS keys")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = ec.RM(ctx, "/patroni/postgres-cluster/leader")
	cancel()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	_ = ec.RMRecursive(ctx2, "/patroni/postgres-cluster/members")
	cancel2()
	pkglog.Infof("quorum recovery complete — new nodes can now join")
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
	leader, err := ec.Get(ctx, "/patroni/postgres-cluster/leader")
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
	_ = ec.RM(rctx, "/patroni/postgres-cluster/leader")
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
//     AND there is a healthy primary. The data is from the wrong cluster epoch.
//     Action: wipe local PG data and restart Patroni so it pg_basebackup.
func checkPatroniSystemID(cfg *config.Config, ec *etcdmgr.Client, desiredIPs []string) {
	initKey := fmt.Sprintf("/patroni/%s/initialize", cfg.AppName)

	// Read the etcd cluster-initialize system identifier.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	etcdSysID, err := ec.Get(ctx, initKey)
	cancel()
	if err != nil || etcdSysID == "" {
		// Not yet initialised — nothing to heal.
		return
	}

	// Find a primary among desired peers and get its system ID.
	pc := patroni.New(cfg.SSLEnabled, cfg.HostPatroniAPIPort)
	primarySysID := ""
	primaryIP := ""
	for _, ip := range desiredIPs {
		ictx, icancel := context.WithTimeout(context.Background(), 5*time.Second)
		info, infoErr := pc.GetInfo(ictx, ip)
		icancel()
		if infoErr != nil || info == nil {
			continue
		}
		if info.Role == "primary" && info.DatabaseSystemIdentifier != "" {
			primarySysID = info.DatabaseSystemIdentifier
			primaryIP = ip
			break
		}
	}

	if primarySysID == "" {
		// No reachable primary — cannot make authoritative decisions.
		return
	}

	// Case 1: etcd /initialize key is stale (doesn't match the running primary).
	if etcdSysID != primarySysID {
		pkglog.Warnf("system ID mismatch: etcd /initialize=%s but primary %s reports %s — updating initialize key",
			etcdSysID, primaryIP, primarySysID)
		uctx, ucancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = ec.Set(uctx, initKey, primarySysID)
		ucancel()
		// Refresh etcdSysID for case 2 below.
		etcdSysID = primarySysID
	}

	// Case 2: local PG data has a system ID from a different cluster epoch.
	// Only attempt if we are NOT the primary and PG data actually exists.
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
	pkglog.Warnf("local PG system ID %s != cluster system ID %s — wiping data for clean pg_basebackup",
		localSysID, etcdSysID)
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

func handleEtcdUnreachable(cfg *config.Config, sslOpts, desiredIPs []string, unavailFile string) {
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
	var hostsParts, initialParts []string
	for _, ip := range desiredIPs {
		name := "node-" + strings.ReplaceAll(ip, ".", "-")
		hostsParts = append(hostsParts, fmt.Sprintf("%s:%d", ip, cfg.HostEtcdClientPort))
		initialParts = append(initialParts, name+"="+fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), ip, cfg.HostEtcdPeerPort))
	}
	newHosts := strings.Join(hostsParts, ",")
	newInitial := strings.Join(initialParts, ",")

	if newInitial == cfg.EtcdInitialCluster && newHosts == cfg.EtcdHosts {
		return
	}
	cfg.EtcdHosts = newHosts
	cfg.EtcdInitialCluster = newInitial
	if err := cfg.WriteClusterEnv(); err != nil {
		pkglog.Warnf("write cluster env: %v", err)
	}
	// Update patroni.yml etcd hosts line
	updatePatroniYAMLHosts("/etc/patroni/patroni.yml", newHosts)
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
