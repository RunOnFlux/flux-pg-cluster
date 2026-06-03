package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
	myerrors "github.com/RunOnFlux/flux-pg-cluster/internal/errors"
	"github.com/RunOnFlux/flux-pg-cluster/internal/etcdmgr"
	pkglog "github.com/RunOnFlux/flux-pg-cluster/internal/log"
)

// runEtcdStart implements the state machine from start-etcd.sh:
//
//  1. Load cluster_env
//  2. If data directory exists:
//     a. Fast peer check — if any peer reachable, wipe local data (avoids
//     disrupting running cluster) and force rejoin via member add.
//     b. If no peers reachable, validate local data with temp etcd; if
//     cluster-ID mismatch, wipe; otherwise start as existing.
//  3. If no data (or wiped): try to join existing cluster via member add.
//  4. If no peers, bootstrap new cluster (with safety guards).
//  5. Finally exec etcd with the resolved CLUSTER_STATE (and --force-new-cluster
//     if the flag file is present).
func runEtcdStart(args []string) {
	fs := flag.NewFlagSet("etcd-start", flag.ExitOnError)
	dataDir := fs.String("data-dir", "/var/lib/etcd", "etcd data directory")
	forceNewClusterFlagFile := fs.String("fnf-flag", "/tmp/force-new-cluster", "force-new-cluster flag file")
	clusterToken := fs.String("cluster-token", "postgres-cluster-token", "etcd cluster token")
	_ = fs.Parse(args)

	pkglog.Infof("ETCD STARTING (Go agent): %s", time.Now().Format(time.RFC3339))

	cfg := config.FromEnv()
	if err := config.LoadClusterEnv(cfg); err != nil {
		pkglog.Fatalf("cannot read %s: %v", config.ClusterEnvFile, err)
	}

	pkglog.Infof("NAME=%s SSL=%v INITIAL_CLUSTER=%s", cfg.MyName, cfg.SSLEnabled, cfg.EtcdInitialCluster)

	sslOpts := etcdSSLOpts(cfg)
	otherIPs := otherIPsFromInitialCluster(cfg.EtcdInitialCluster, cfg.MyName)

	clusterState := ""
	forceRejoin := false
	dataPresent := false
	if _, err := os.Stat(*dataDir + "/member/snap/db"); err == nil {
		dataPresent = true
	}

	if dataPresent {
		pkglog.Infof("data directory present; checking peers...")
		// Fast peer check: avoid starting temp etcd if any peer is reachable
		// (a stale etcd would emit high-term raft messages and disrupt the cluster).
		anyPeerReachable := false
		for _, ip := range otherIPs {
			endpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), ip, cfg.HostEtcdClientPort)
			ec := etcdmgr.New(endpoint, sslOpts)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			members, err := ec.MemberList(ctx)
			cancel()
			if err == nil && len(members) > 0 {
				anyPeerReachable = true
				if etcdmgr.FindByName(members, cfg.MyName) != nil {
					pkglog.Infof("peer %s knows about us", ip)
				} else {
					pkglog.Infof("peer %s does not know about us (we may have been removed)", ip)
				}
				break
			}
		}

		if anyPeerReachable {
			pkglog.Infof("live peer reachable — wiping local data to force a clean rejoin")
			if err := wipeDir(*dataDir); err != nil {
				pkglog.Errorf("wipe data dir: %v", err)
			}
			forceRejoin = true
		} else {
			pkglog.Infof("no peers reachable — starting temp etcd to verify local data...")
			tempState, err := verifyLocalDataWithTempEtcd(cfg, *dataDir, *clusterToken, sslOpts, otherIPs)
			if err != nil {
				pkglog.Warnf("temp etcd verification inconclusive: %v — preserving data", err)
				clusterState = "existing"
			} else {
				clusterState = tempState
			}
		}
	}

	if clusterState == "" {
		pkglog.Infof("no data directory — attempting to join existing cluster")
		candidate := bootstrapCandidate(cfg.EtcdInitialCluster)
		isCandidate := cfg.MyName == candidate
		// Candidate uses short retry — if no peer is up, it's a cold start and
		// the candidate should bootstrap quickly so others can join.
		// Non-candidates retry patiently (much longer) and never bootstrap on
		// their own — they must always join an existing cluster (split-brain safe).
		maxRetries := cfg.EtcdJoinMaxRetries
		if isCandidate {
			if maxRetries > 3 {
				maxRetries = 3
			}
		} else if maxRetries < 60 {
			maxRetries = 60
		}
		state, joinedInitial, err := tryJoinExisting(cfg, sslOpts, otherIPs, forceRejoin, maxRetries)
		if err == nil {
			clusterState = state
			if joinedInitial != "" {
				cfg.EtcdInitialCluster = joinedInitial
				pkglog.Infof("rebuilt ETCD_INITIAL_CLUSTER from actual members: %s", joinedInitial)
			}
		} else {
			if myerrors.IsSplitBrainRisk(err) {
				pkglog.Errorf("SPLIT-BRAIN GUARD: peers reachable but join failed — refusing to bootstrap new cluster")
				os.Exit(1)
			}
			if forceRejoin {
				pkglog.Errorf("rejoin was forced after wipe but no peer reachable — refusing unsafe bootstrap")
				os.Exit(1)
			}
			if !isCandidate {
				pkglog.Errorf("non-candidate %s could not reach any peer after %d attempts — exiting (supervisor will retry)", cfg.MyName, maxRetries)
				os.Exit(1)
			}
			state = decideBootstrap(cfg, *dataDir)
			if state == "" {
				os.Exit(1)
			}
			clusterState = state
			// Bootstrap as single-member so non-candidates can member-add
			// immediately. A multi-member --initial-cluster with state=new
			// requires ALL listed members to start simultaneously, which is
			// impossible when non-candidates are doing member-add joins.
			myPeerURL := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), cfg.MyIP, cfg.HostEtcdPeerPort)
			cfg.EtcdInitialCluster = cfg.MyName + "=" + myPeerURL
			pkglog.Infof("single-candidate bootstrap: ETCD_INITIAL_CLUSTER shrunk to %s", cfg.EtcdInitialCluster)
		}
	}

	pkglog.Infof("CLUSTER_STATE=%s", clusterState)

	// Check for force-new-cluster flag written by daemon's quorum recovery
	forceNewCluster := false
	if _, err := os.Stat(*forceNewClusterFlagFile); err == nil {
		pkglog.Infof("force-new-cluster flag detected — starting etcd with --force-new-cluster")
		_ = os.Remove(*forceNewClusterFlagFile)
		forceNewCluster = true
	}

	args2 := buildEtcdArgs(cfg, *dataDir, *clusterToken, clusterState, forceNewCluster, sslOpts)
	pkglog.Infof("starting etcd: etcd %s", strings.Join(args2, " "))
	// Write the restart marker before exec so the daemon's 90s cooldown kicks
	// in immediately and prevents a false "etcd unavailable" restart race.
	markEtcdRestart()
	if err := syscall.Exec("/usr/bin/etcd", append([]string{"etcd"}, args2...), os.Environ()); err != nil {
		// fallback: lookup PATH
		bin, lerr := exec.LookPath("etcd")
		if lerr != nil {
			pkglog.Fatalf("etcd binary not found: %v", lerr)
		}
		if err := syscall.Exec(bin, append([]string{"etcd"}, args2...), os.Environ()); err != nil {
			pkglog.Fatalf("exec etcd: %v", err)
		}
	}
}

func etcdSSLOpts(cfg *config.Config) []string {
	if !cfg.SSLEnabled {
		return nil
	}
	// etcdctl v3 uses --cert/--key/--cacert (server binary still uses --cert-file etc.)
	return []string{
		"--cert=/etc/ssl/cluster/etcd/client.crt",
		"--key=/etc/ssl/cluster/etcd/client.key",
		"--cacert=/etc/ssl/cluster/ca/ca.crt",
	}
}

// otherIPsFromInitialCluster parses "name=URL,name=URL" and returns the IPs of
// members whose name is NOT myName.
func otherIPsFromInitialCluster(initial, myName string) []string {
	var out []string
	for _, part := range strings.Split(initial, ",") {
		part = strings.TrimSpace(part)
		name, url, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if name == myName {
			continue
		}
		ip := extractHostFromURL(url)
		if ip != "" {
			out = append(out, ip)
		}
	}
	return out
}

// extractHostFromURL strips scheme:// prefix and :port suffix from a URL like
// "https://1.2.3.4:2380".
func extractHostFromURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.LastIndex(u, ":"); i > 0 {
		u = u[:i]
	}
	return u
}

func wipeDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(dir + "/" + e.Name()); err != nil {
			return err
		}
	}
	return nil
}

// verifyLocalDataWithTempEtcd starts a temp etcd to confirm local data is from
// the same cluster as the peers. Returns "existing" if data is valid, or
// empty string + error if mismatch (data should be wiped).
func verifyLocalDataWithTempEtcd(cfg *config.Config, dataDir, token string, sslOpts, otherIPs []string) (string, error) {
	args := buildEtcdArgs(cfg, dataDir, token, "existing", false, sslOpts)
	cmd := exec.Command("etcd", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start temp etcd: %w", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
		time.Sleep(2 * time.Second)
	}()

	time.Sleep(10 * time.Second)

	// Query peers; if majority don't know us, wipe (return error so caller wipes)
	localEndpoint := fmt.Sprintf("%s://127.0.0.1:%d", cfg.EtcdProtocol(), cfg.EtcdClientPort)
	ec := etcdmgr.New(localEndpoint, sslOpts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := ec.MemberList(ctx)
	cancel()
	if err != nil {
		return "existing", nil
	}

	matching, mismatched := 0, 0
	for _, ip := range otherIPs {
		endpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), ip, cfg.HostEtcdClientPort)
		peer := etcdmgr.New(endpoint, sslOpts)
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		members, err := peer.MemberList(pctx)
		cancel()
		if err != nil {
			continue
		}
		if etcdmgr.FindByName(members, cfg.MyName) != nil {
			matching++
		} else {
			mismatched++
		}
	}

	if mismatched > matching {
		return "", fmt.Errorf("cluster ID mismatch (matching=%d mismatched=%d)", matching, mismatched)
	}
	return "existing", nil
}

// tryJoinExisting attempts to contact each known peer and register via member add.
// Returns (state, rebuiltInitialCluster, error). joinedInitial is the new
// ETCD_INITIAL_CLUSTER string derived from actual registered members (needed
// to satisfy etcd v3.3's "member count is unequal" check).
func tryJoinExisting(cfg *config.Config, sslOpts, otherIPs []string, forceRejoin bool, maxRetries int) (string, string, error) {
	delay := time.Duration(cfg.EtcdJoinRetryDelaySeconds) * time.Second
	if maxRetries < 1 {
		maxRetries = 1
	}

	anyReachable := false
	peerURL := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), cfg.MyIP, cfg.HostEtcdPeerPort)

	if len(otherIPs) == 0 {
		return "", "", myerrors.NewNoPeersReachable()
	}

	for attempt := 1; attempt <= maxRetries; attempt++ {
		pkglog.Infof("peer discovery attempt %d/%d", attempt, maxRetries)
		for _, ip := range otherIPs {
			endpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), ip, cfg.HostEtcdClientPort)
			ec := etcdmgr.New(endpoint, sslOpts)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			members, err := ec.MemberList(ctx)
			cancel()
			if err != nil {
				continue
			}
			anyReachable = true

			existing := etcdmgr.FindByName(members, cfg.MyName)
			if existing == nil {
				existing = etcdmgr.FindByPeerURL(members, peerURL)
			}
			if existing != nil {
				isGhost := existing.Unstarted || existing.Name == ""
				if isGhost || forceRejoin {
					reason := "force-rejoin (data wiped)"
					if isGhost {
						reason = "ghost registration"
					}
					pkglog.Infof("removing stale member entry (%s) on %s id=%s", reason, ip, existing.ID)
					rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
					_ = ec.MemberRemove(rctx, existing.ID)
					rcancel()
					time.Sleep(2 * time.Second)
					// fall through to member add
				} else {
					pkglog.Infof("already registered — starting as existing")
					return "existing", "", nil
				}
			}

			pkglog.Infof("adding this node to the existing cluster as learner via %s", ip)
			actx, acancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = ec.MemberAddLearner(actx, cfg.MyName, peerURL)
			acancel()
			if err != nil {
				if strings.Contains(err.Error(), "too many learner members") {
					// Another node is already syncing as a learner. etcd v3 allows
					// only 1 learner at a time. Break the IP loop and retry after
					// the delay — the existing learner should be promoted soon.
					pkglog.Infof("another learner is already syncing — will retry after %s", delay)
					anyReachable = true
					break
				}
				pkglog.Warnf("member add-learner via %s failed: %v", ip, err)
				continue
			}

			// Rebuild ETCD_INITIAL_CLUSTER from actual registered members so
			// etcd doesn't reject startup with "member count is unequal".
			pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
			updated, _ := ec.MemberList(pctx)
			pcancel()
			rebuilt := rebuildInitialCluster(updated, cfg.MyName, peerURL)
			pkglog.Infof("successfully registered in existing cluster as learner")
			return "existing", rebuilt, nil
		}

		if attempt < maxRetries {
			pkglog.Infof("retrying in %s...", delay)
			time.Sleep(delay)
		}
	}

	if anyReachable {
		return "", "", myerrors.NewSplitBrainRisk()
	}
	return "", "", myerrors.NewNoPeersReachable()
}

// rebuildInitialCluster constructs an --initial-cluster value from the actual
// registered members. For unstarted (no name) members, we assign our own name
// if the peer URL matches ours; otherwise we skip them.
func rebuildInitialCluster(members []etcdmgr.Member, myName, myPeerURL string) string {
	var parts []string
	for _, m := range members {
		if m.PeerURLs == "" {
			continue
		}
		name := m.Name
		if name == "" {
			if m.PeerURLs == myPeerURL {
				name = myName
			} else {
				continue
			}
		}
		parts = append(parts, name+"="+m.PeerURLs)
	}
	return strings.Join(parts, ",")
}

// decideBootstrap implements the safety guards for bootstrapping a brand-new
// multi-member cluster: only allowed when data dirs are empty and either the
// caller is the deterministic candidate or override flags are set.
func decideBootstrap(cfg *config.Config, dataDir string) string {
	expectedCount := len(strings.Split(cfg.EtcdInitialCluster, ","))
	if expectedCount <= 1 {
		pkglog.Infof("single-member configuration — bootstrapping as new")
		return "new"
	}

	etcdDataEmpty := !fileExists(dataDir + "/member/snap/db")
	pgDataEmpty := !dirExists("/var/lib/postgresql/data/global")

	candidate := bootstrapCandidate(cfg.EtcdInitialCluster)

	if !cfg.AllowNewClusterBootstrap {
		if cfg.AutoBootstrapIfFresh && etcdDataEmpty && pgDataEmpty {
			if cfg.MyName != candidate {
				pkglog.Errorf("fresh non-candidate %s reached bootstrap path — refusing (only candidate %s may bootstrap)", cfg.MyName, candidate)
				return ""
			}
			pkglog.Infof("fresh multi-member install detected on deterministic candidate — auto-bootstrap")
			return "new"
		}
		// Dead cluster recovery: candidate lost its etcd data but has PG data,
		// and ALL peers were already confirmed unreachable (NoPeersReachable path).
		// Bootstrapping a fresh 1-node etcd is safe here — pg data is preserved
		// and Patroni will start from existing data. Other nodes will join after.
		if cfg.DeadClusterRecovery && etcdDataEmpty && !pgDataEmpty && cfg.MyName == candidate {
			pkglog.Warnf("DEAD CLUSTER RECOVERY: etcd data lost on all peers, PG data preserved — candidate bootstrapping new etcd cluster to recover")
			pkglog.Warnf("PG data will NOT be wiped; Patroni will start from existing data as primary")
			return "new"
		}
		pkglog.Errorf("multi-member cluster, no peer found, refusing automatic bootstrap (split-brain prevention)")
		pkglog.Errorf("conditions: candidate=%v etcd_empty=%v pg_empty=%v auto_fresh=%v dead_recovery=%v",
			cfg.MyName == candidate, etcdDataEmpty, pgDataEmpty, cfg.AutoBootstrapIfFresh, cfg.DeadClusterRecovery)
		return ""
	}

	if !cfg.AllowAnyNodeBootstrap && cfg.MyName != candidate {
		pkglog.Errorf("bootstrap restricted to deterministic candidate %s; this node is %s", candidate, cfg.MyName)
		return ""
	}
	pkglog.Infof("explicit bootstrap override enabled — bootstrapping new cluster")
	return "new"
}

// bootstrapCandidate returns the lexicographically smallest member name from
// ETCD_INITIAL_CLUSTER. This is deterministic across all nodes given the same
// initial cluster string.
func bootstrapCandidate(initial string) string {
	var names []string
	for _, p := range strings.Split(initial, ",") {
		if i := strings.Index(p, "="); i > 0 {
			names = append(names, strings.TrimSpace(p[:i]))
		}
	}
	if len(names) == 0 {
		return ""
	}
	smallest := names[0]
	for _, n := range names[1:] {
		if n < smallest {
			smallest = n
		}
	}
	return smallest
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// buildEtcdArgs constructs the argument list for the etcd binary.
func buildEtcdArgs(cfg *config.Config, dataDir, token, state string, forceNewCluster bool, sslOpts []string) []string {
	args := []string{
		"--name=" + cfg.MyName,
		fmt.Sprintf("--listen-client-urls=%s://0.0.0.0:%d", cfg.EtcdProtocol(), cfg.EtcdClientPort),
		fmt.Sprintf("--advertise-client-urls=%s://%s:%d", cfg.EtcdProtocol(), cfg.MyIP, cfg.HostEtcdClientPort),
		fmt.Sprintf("--listen-peer-urls=%s://0.0.0.0:%d", cfg.EtcdProtocol(), cfg.EtcdPeerPort),
		fmt.Sprintf("--initial-advertise-peer-urls=%s://%s:%d", cfg.EtcdProtocol(), cfg.MyIP, cfg.HostEtcdPeerPort),
		"--initial-cluster=" + cfg.EtcdInitialCluster,
		"--initial-cluster-state=" + state,
		"--initial-cluster-token=" + token,
		"--data-dir=" + dataDir,
	}
	if forceNewCluster {
		args = append(args, "--force-new-cluster")
	}
	if len(sslOpts) > 0 {
		// Reformat: etcd's client SSL flags differ from etcdctl's.
		// In SSL mode we mirror the shell script's explicit flag list.
		args = append(args,
			"--cert-file=/etc/ssl/cluster/etcd/client.crt",
			"--key-file=/etc/ssl/cluster/etcd/client.key",
			"--trusted-ca-file=/etc/ssl/cluster/ca/ca.crt",
			"--client-cert-auth",
			"--peer-cert-file=/etc/ssl/cluster/etcd/peer.crt",
			"--peer-key-file=/etc/ssl/cluster/etcd/peer.key",
			"--peer-trusted-ca-file=/etc/ssl/cluster/ca/ca.crt",
			"--peer-client-cert-auth",
		)
	}
	return args
}
