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

// runEtcdStart preserves durable Raft state, joins only the pinned database's
// DCS, and rebuilds a control plane only around proven existing PGDATA.
// PostgreSQL creation is never part of this state machine.
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

	// The .etcd3_api marker records that this data directory was written by the
	// etcd v3 setup (v3 gRPC API / patroni[etcd3]). It is used as a breadcrumb
	// for future tooling; no automated wipe is performed based on it because:
	//   - etcd 3.5 reads etcd 3.3 WAL data without issues (3.x backward compat)
	//   - Patroni's etcd3 DCS sees an empty v3 keyspace on first boot → re-elects
	//     fresh while preserving PostgreSQL data — no intervention required
	//   - Any wipe heuristic based on marker absence risks false positives when
	//     peers are unreachable (network blip, all nodes restarting together) or
	//     when otherIPs is empty (single-node / bootstrap candidate)
	// The marker is written just before exec below; existing data is untouched.
	markerFile := *dataDir + "/.etcd3_api"

	clusterState := ""
	forceRejoin := !etcdHasLocalState(*dataDir)
	dataPresent := false
	if etcdHasLocalState(*dataDir) {
		dataPresent = true
	}

	if dataPresent {
		pkglog.Infof("data directory present; checking peers...")
		// Fast peer check: avoid starting temp etcd if any peer is reachable
		// (a stale etcd would emit high-term raft messages and disrupt the cluster).
		anyPeerReachable := false
		peerKnowsUs := false
		for _, ip := range otherIPs {
			endpoint := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), ip, cfg.HostEtcdClientPort)
			ec := etcdmgr.New(endpoint, sslOpts)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			members, err := ec.MemberList(ctx)
			cancel()
			if err == nil && len(members) > 0 {
				anyPeerReachable = true
				vctx, vcancel := context.WithTimeout(context.Background(), 5*time.Second)
				identityErr := validatePeerCluster(vctx, cfg, ec)
				vcancel()
				if identityErr != nil {
					pkglog.Fatalf("refusing etcd convergence: %v", identityErr)
				}
				if etcdmgr.FindByName(members, cfg.MyName) != nil {
					pkglog.Infof("peer %s knows about us", ip)
					peerKnowsUs = true
				} else {
					pkglog.Infof("peer %s does not know about us (we may have been removed)", ip)
				}
				break
			}
		}

		if anyPeerReachable {
			if peerKnowsUs {
				pkglog.Infof("live peer confirms this member — preserving local etcd data")
				clusterState = "existing"
			} else {
				pkglog.Infof("live peer does not recognize this member — archiving local etcd data for an identity-checked rejoin")
				if err := archiveEtcdData(*dataDir); err != nil {
					pkglog.Fatalf("archive etcd data: %v", err)
				}
				forceRejoin = true
			}
		} else {
			pkglog.Infof("no peers reachable — preserving and starting existing etcd state")
			clusterState = "existing"
		}
	}

	// The daemon has already gathered repeated unanimous recovery evidence and
	// writes the force-new flag to wake the selected authority. When this node's
	// etcd directory is empty there is no old cluster to force; consume the flag
	// before the normal peer-discovery window, independently revalidate the
	// authority, and bootstrap a new single-member control plane immediately.
	// Consuming the flag here also prevents it from being misapplied later if a
	// peer happens to recover while validation is in progress.
	if clusterState == "" && !dataPresent && cfg.DeadClusterRecovery && dirExists("/var/lib/postgresql/data/global") {
		state, hinted := consumeEmptyDCSRecoveryHint(cfg, *dataDir, *forceNewClusterFlagFile, decideBootstrap)
		if hinted {
			if state == "new" {
				clusterState = state
				configureSingleMemberBootstrap(cfg)
				pkglog.Warnf("empty-DCS recovery hint authorized — bootstrapping selected PostgreSQL authority immediately")
			} else {
				pkglog.Errorf("empty-DCS recovery hint rejected by current peer evidence — returning to safe discovery")
			}
		}
	}

	if clusterState == "" {
		pkglog.Infof("no data directory — attempting to join existing cluster")
		candidate := bootstrapCandidate(cfg.EtcdInitialCluster)
		isCandidate := cfg.MyName == candidate
		// Every node waits through the configured discovery window. In
		// particular, the candidate must not infer "new deployment" from a few
		// fast connection failures during Flux placement churn.
		maxRetries := cfg.EtcdJoinMaxRetries
		if !isCandidate && maxRetries < 60 {
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
			if forceRejoin && dataPresent {
				pkglog.Errorf("rejoin was forced after archive but no peer reachable — refusing unsafe bootstrap")
				os.Exit(1)
			}
			// Fresh installation remains restricted to the deterministic
			// candidate. A data-bearing dead-cluster recovery candidate must be
			// allowed to reach decideBootstrap regardless of node ordering;
			// decideBootstrap will require unanimous proof that it is the sole
			// PostgreSQL authority before returning "new".
			if !mayEvaluateBootstrap(isCandidate, cfg.DeadClusterRecovery, dirExists("/var/lib/postgresql/data/global")) {
				pkglog.Errorf("non-candidate %s could not reach any peer after %d attempts — exiting (supervisor will retry)", cfg.MyName, maxRetries)
				os.Exit(1)
			}
			state = decideBootstrap(cfg, *dataDir)
			if state == "" {
				// An incomplete Flux view is expected briefly while a deployment is
				// being placed. Do not exit so quickly that Supervisor exhausts its
				// startup retries and leaves etcd permanently FATAL before the updater
				// can write the complete topology to cluster_env. Remaining alive past
				// Supervisor's startsecs moves us to RUNNING, after which autorestart
				// continues retrying with freshly loaded configuration.
				delay := bootstrapRetryDelay(cfg.EtcdJoinRetryDelaySeconds)
				pkglog.Infof("bootstrap not yet safe — retrying after %s", delay)
				time.Sleep(delay)
				os.Exit(1)
			}
			clusterState = state
			// Bootstrap as single-member so non-candidates can member-add
			// immediately. A multi-member --initial-cluster with state=new
			// requires ALL listed members to start simultaneously, which is
			// impossible when non-candidates are doing member-add joins.
			configureSingleMemberBootstrap(cfg)
		}
	}

	pkglog.Infof("CLUSTER_STATE=%s", clusterState)

	// Revalidate the one-shot recovery hint at execution time. A stale file must
	// never force a new Raft cluster after the network or policy has changed.
	forceNewCluster := false
	if fileExists(*forceNewClusterFlagFile) {
		_ = os.Remove(*forceNewClusterFlagFile)
		if !cfg.DeadClusterRecovery {
			pkglog.Fatalf("force-new-cluster blocked: DEAD_CLUSTER_RECOVERY=false")
		}
		authority, safe, reason := confirmRecoveryAuthorityWithPeers(cfg)
		if !safe || authority.IP != cfg.MyIP {
			pkglog.Fatalf("force-new-cluster blocked: %s", reason)
		}
		forceNewCluster = dataPresent && !anyDCSQuorum(cfg)
	}

	if clusterState == "new" {
		token, err := newDCSBootstrapToken(*clusterToken)
		if err != nil {
			pkglog.Fatalf("create control-plane identity: %v", err)
		}
		*clusterToken = token
	}
	args2 := buildEtcdArgs(cfg, *dataDir, *clusterToken, clusterState, forceNewCluster, sslOpts)
	pkglog.Infof("starting etcd: etcd %s", strings.Join(args2, " "))
	// Write the restart marker before exec so the daemon's 90s cooldown kicks
	// in immediately and prevents a false "etcd unavailable" restart race.
	markEtcdRestart()
	// Write the v3 API marker so future restarts recognise this data dir as v3
	// and skip the legacy-migration wipe.
	if err := os.MkdirAll(*dataDir, 0o755); err == nil {
		_ = os.WriteFile(markerFile, []byte("etcd3\n"), 0o644)
	}
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

func consumeEmptyDCSRecoveryHint(cfg *config.Config, dataDir, flagFile string,
	decide func(*config.Config, string) string) (string, bool) {
	if !fileExists(flagFile) {
		return "", false
	}
	// The hint is one-shot. A stale force-new flag must never survive a failed
	// validation and later apply to an etcd cluster that another node recovered.
	_ = os.Remove(flagFile)
	return decide(cfg, dataDir), true
}

func configureSingleMemberBootstrap(cfg *config.Config) {
	myPeerURL := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), cfg.MyIP, cfg.HostEtcdPeerPort)
	cfg.EtcdInitialCluster = cfg.MyName + "=" + myPeerURL
	pkglog.Infof("single-candidate bootstrap: ETCD_INITIAL_CLUSTER shrunk to %s", cfg.EtcdInitialCluster)
}

func bootstrapRetryDelay(configuredSeconds int) time.Duration {
	// Supervisor's default startsecs is one second. Keep this process alive for
	// at least two seconds so an intentional safety exit is treated as a runtime
	// failure and autorestart=true keeps retrying instead of entering FATAL.
	if configuredSeconds < 2 {
		configuredSeconds = 2
	}
	return time.Duration(configuredSeconds) * time.Second
}

func mayEvaluateBootstrap(isDeterministicCandidate, deadClusterRecovery, hasPGData bool) bool {
	return isDeterministicCandidate || (deadClusterRecovery && hasPGData)
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

// tryJoinExisting attempts to contact each known peer and register via member add.
// Returns (state, rebuiltInitialCluster, error). joinedInitial is the new
// ETCD_INITIAL_CLUSTER string derived from actual registered members (needed
// to satisfy etcd v3.3's "member count is unequal" check).
func tryJoinExisting(cfg *config.Config, sslOpts, otherIPs []string, forceRejoin bool, maxRetries int) (string, string, error) {
	return tryJoinExistingWithLoader(cfg, sslOpts, otherIPs, forceRejoin || !etcdHasLocalState("/var/lib/etcd"), maxRetries, config.LoadClusterEnv)
}

func tryJoinExistingWithLoader(cfg *config.Config, sslOpts, otherIPs []string, forceRejoin bool, maxRetries int,
	loadClusterEnv func(*config.Config) error) (string, string, error) {
	if maxRetries < 1 {
		maxRetries = 1
	}

	anyReachable := false

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// The reconciliation daemon may replace an incomplete startup topology
		// while this process is waiting for Flux placement to settle. Refresh the
		// inputs, not only the network operation, on every attempt. Keep the last
		// usable snapshot if a concurrent read temporarily fails.
		if err := loadClusterEnv(cfg); err != nil {
			pkglog.Warnf("cannot refresh %s for peer discovery: %v", config.ClusterEnvFile, err)
		} else {
			otherIPs = otherIPsFromInitialCluster(cfg.EtcdInitialCluster, cfg.MyName)
		}
		delay := time.Duration(cfg.EtcdJoinRetryDelaySeconds) * time.Second
		peerURL := fmt.Sprintf("%s://%s:%d", cfg.EtcdProtocol(), cfg.MyIP, cfg.HostEtcdPeerPort)
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
			vctx, vcancel := context.WithTimeout(context.Background(), 5*time.Second)
			identityErr := validatePeerCluster(vctx, cfg, ec)
			vcancel()
			if identityErr != nil {
				pkglog.Warnf("refusing peer %s: %v", ip, identityErr)
				continue
			}

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
					removeErr := ec.MemberRemove(rctx, existing.ID)
					rcancel()
					if removeErr != nil {
						pkglog.Warnf("remove stale member: %v", removeErr)
						continue
					}
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
			updated, listErr := ec.MemberList(pctx)
			pcancel()
			if listErr != nil {
				pkglog.Warnf("read registered members: %v", listErr)
				continue
			}
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

// decideBootstrap can rebuild only a control plane around preserved PGDATA.
// PostgreSQL first creation is exclusively an offline operator command.
func decideBootstrap(cfg *config.Config, dataDir string) string {
	etcdDataEmpty := !etcdHasLocalState(dataDir)
	pgDataEmpty := !fileExists(postgresDataDir + "/global/pg_control")
	// Legacy flags are retained for config compatibility but cannot authorize
	// PostgreSQL creation. Only create-cluster performs initdb, offline.
	if etcdDataEmpty && !pgDataEmpty && cfg.DeadClusterRecovery {
		authority, safe, reason := confirmRecoveryAuthorityWithPeers(cfg)
		if safe && authority.IP == cfg.MyIP {
			if anyDCSQuorum(cfg) {
				pkglog.Infof("DCS recovery cancelled: quorum has returned")
				return ""
			}
			pkglog.Warnf("rebuilding DCS for preserved PostgreSQL %s", authority.SystemID)
			return "new"
		}
		pkglog.Errorf("DCS recovery blocked: %s", reason)
	}
	pkglog.Errorf("empty-cluster bootstrap disabled; create-cluster or restore the pinned database")
	return ""
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
