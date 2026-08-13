package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
	pkglog "github.com/RunOnFlux/flux-pg-cluster/internal/log"
	"github.com/RunOnFlux/flux-pg-cluster/internal/patroni"
)

const clusterIdentityPath = "/cluster-identity"

// nodeIdentity is deliberately small and contains no credentials. The proxy
// serves it even while Patroni is unavailable, allowing a fresh node to
// distinguish a genuinely new deployment from an older node whose PGDATA was
// preserved but whose database cannot currently start.
type nodeIdentity struct {
	AppName                  string   `json:"app_name"`
	PatroniScope             string   `json:"patroni_scope"`
	NodeName                 string   `json:"node_name"`
	PGDataEmpty              bool     `json:"pgdata_empty"`
	PostgresSystemIdentifier string   `json:"postgres_system_id,omitempty"`
	EtcdDataEmpty            bool     `json:"etcd_data_empty"`
	MembershipView           []string `json:"membership_view"`
}

type peerIdentityResult struct {
	IP       string
	Identity nodeIdentity
	Err      error
}

type recoveryAuthority struct {
	IP       string
	NodeName string
	SystemID string
}

func localNodeIdentity(cfg *config.Config) nodeIdentity {
	identity := nodeIdentity{
		AppName:        cfg.AppName,
		PatroniScope:   cfg.PatroniScope,
		NodeName:       cfg.MyName,
		PGDataEmpty:    directoryIsEmpty("/var/lib/postgresql/data"),
		EtcdDataEmpty:  etcdDataDirectoryIsFresh("/var/lib/etcd"),
		MembershipView: membershipNames(cfg.EtcdInitialCluster),
	}
	if !identity.PGDataEmpty {
		if systemID, err := readLocalPGSystemID(); err == nil {
			identity.PostgresSystemIdentifier = systemID
		}
	}
	return identity
}

func membershipNames(initial string) []string {
	var names []string
	for _, part := range strings.Split(initial, ",") {
		name, _, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// directoryIsEmpty is deliberately strict: a partial basebackup, an etcd WAL,
// or any other leftover file is preserved-state evidence and must block a new
// epoch. Only a missing directory or a directory with zero entries is empty.
func directoryIsEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && len(entries) == 0
}

// etcdDataDirectoryIsFresh applies the same strict empty-directory policy as
// directoryIsEmpty, except for the agent-owned .etcd3_api breadcrumb. The
// marker is written before execing etcd and is not etcd state; treating it as
// preserved state prevents an otherwise fresh node from ever retrying a safe
// bootstrap after an unsuccessful etcd launch.
func etcdDataDirectoryIsFresh(path string) bool {
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() != ".etcd3_api" || !entry.Type().IsRegular() {
			return false
		}
	}
	return true
}

// identityProbeToken authenticates the read-only peer probe without exposing
// the replication password itself. The endpoint still returns no secrets, but
// requiring a shared HMAC prevents arbitrary Internet clients from inventorying
// cluster state on the public proxy port.
func identityProbeToken(cfg *config.Config) string {
	mac := hmac.New(sha256.New, []byte(cfg.PostgresReplicationPassword))
	_, _ = mac.Write([]byte(cfg.AppName + "\x00" + cfg.PatroniScope))
	return hex.EncodeToString(mac.Sum(nil))
}

func validIdentityProbeToken(cfg *config.Config, got string) bool {
	want := identityProbeToken(cfg)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func writeIdentityResponse(w http.ResponseWriter, cfg *config.Config, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != clusterIdentityPath {
		http.NotFound(w, r)
		return
	}
	if !validIdentityProbeToken(cfg, r.Header.Get("X-Flux-Cluster-Probe")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(localNodeIdentity(cfg))
}

func probePeerIdentity(ctx context.Context, cfg *config.Config, ip string) (nodeIdentity, error) {
	url := fmt.Sprintf("http://%s:%d%s", ip, cfg.HostProxyPort, clusterIdentityPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nodeIdentity{}, err
	}
	req.Header.Set("X-Flux-Cluster-Probe", identityProbeToken(cfg))
	client := &http.Client{Timeout: time.Duration(cfg.BootstrapPeerProbeTimeout) * time.Second}
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nodeIdentity{}, fmt.Errorf("identity endpoint status %d", resp.StatusCode)
		}
		var identity nodeIdentity
		if err := json.NewDecoder(resp.Body).Decode(&identity); err != nil {
			return nodeIdentity{}, err
		}
		return identity, nil
	}

	// Compatibility/safety fallback for a peer still running an older image:
	// Patroni can prove that PGDATA is non-empty, which is sufficient to block a
	// fresh bootstrap. It can never provide positive "empty" evidence.
	pc := patroni.New(cfg.SSLEnabled, cfg.HostPatroniAPIPort)
	patroniCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.BootstrapPeerProbeTimeout)*time.Second)
	defer cancel()
	info, patroniErr := pc.GetInfo(patroniCtx, ip)
	if patroniErr == nil && info != nil && info.DatabaseSystemIdentifier != "" {
		return nodeIdentity{
			AppName:                  cfg.AppName,
			PatroniScope:             cfg.PatroniScope,
			NodeName:                 nodeNameFromIP(ip),
			PGDataEmpty:              false,
			PostgresSystemIdentifier: info.DatabaseSystemIdentifier,
			// Unknown is intentionally represented as non-empty. This result is
			// evidence to refuse initdb, never evidence to permit it.
			EtcdDataEmpty:  false,
			MembershipView: nil,
		}, nil
	}
	return nodeIdentity{}, fmt.Errorf("identity probe failed: %w", err)
}

func probeAllPeerIdentities(ctx context.Context, cfg *config.Config, ips []string) []peerIdentityResult {
	results := make([]peerIdentityResult, len(ips))
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			identity, err := probePeerIdentity(ctx, cfg, ip)
			results[i] = peerIdentityResult{IP: ip, Identity: identity, Err: err}
		}(i, ip)
	}
	wg.Wait()
	return results
}

// evaluateFreshPeerEvidence is pure so the safety policy can be exhaustively
// unit tested. Bootstrap requires positive, unanimous evidence: every expected
// peer must answer, identify the same app/scope and membership view, and report
// both PGDATA and etcd as empty.
func evaluateFreshPeerEvidence(cfg *config.Config, results []peerIdentityResult) (bool, string) {
	expectedPeers := len(otherIPsFromInitialCluster(cfg.EtcdInitialCluster, cfg.MyName))
	if len(results) != expectedPeers {
		return false, fmt.Sprintf("received %d peer results, expected %d", len(results), expectedPeers)
	}
	expectedView := membershipNames(cfg.EtcdInitialCluster)
	for _, result := range results {
		if result.Err != nil {
			return false, fmt.Sprintf("peer %s unreachable or ambiguous: %v", result.IP, result.Err)
		}
		identity := result.Identity
		if identity.AppName != cfg.AppName || identity.PatroniScope != cfg.PatroniScope {
			return false, fmt.Sprintf("peer %s belongs to app/scope %q/%q", result.IP, identity.AppName, identity.PatroniScope)
		}
		expectedName := nodeNameFromIP(result.IP)
		if identity.NodeName != expectedName {
			return false, fmt.Sprintf("peer %s identifies as %q, expected %q", result.IP, identity.NodeName, expectedName)
		}
		if !identity.PGDataEmpty || identity.PostgresSystemIdentifier != "" {
			return false, fmt.Sprintf("peer %s has PostgreSQL data (system_id=%s)", result.IP, identity.PostgresSystemIdentifier)
		}
		if !identity.EtcdDataEmpty {
			return false, fmt.Sprintf("peer %s has existing etcd data", result.IP)
		}
		if !equalStrings(identity.MembershipView, expectedView) {
			return false, fmt.Sprintf("peer %s has membership view %v, expected %v", result.IP, identity.MembershipView, expectedView)
		}
	}
	return true, "all expected peers explicitly confirmed empty state"
}

// evaluateRecoveryAuthority permits automatic dead-cluster recovery only when
// every expected node provides authenticated, mutually consistent evidence and
// exactly one node owns valid PostgreSQL data. It deliberately does not use
// etcd state to select the authority: etcd is the control plane being rebuilt,
// while PGDATA is the data that must be preserved.
func evaluateRecoveryAuthority(cfg *config.Config, local nodeIdentity, results []peerIdentityResult) (recoveryAuthority, bool, string) {
	expectedPeers := len(otherIPsFromInitialCluster(cfg.EtcdInitialCluster, cfg.MyName))
	if len(results) != expectedPeers {
		return recoveryAuthority{}, false, fmt.Sprintf("received %d peer results, expected %d", len(results), expectedPeers)
	}

	expectedView := membershipNames(cfg.EtcdInitialCluster)
	all := []peerIdentityResult{{IP: cfg.MyIP, Identity: local}}
	all = append(all, results...)
	var authority recoveryAuthority
	dataNodes := 0

	for _, result := range all {
		if result.Err != nil {
			return recoveryAuthority{}, false, fmt.Sprintf("peer %s unreachable or ambiguous: %v", result.IP, result.Err)
		}
		identity := result.Identity
		if identity.AppName != cfg.AppName || identity.PatroniScope != cfg.PatroniScope {
			return recoveryAuthority{}, false, fmt.Sprintf("node %s belongs to app/scope %q/%q", result.IP, identity.AppName, identity.PatroniScope)
		}
		expectedName := nodeNameFromIP(result.IP)
		if identity.NodeName != expectedName {
			return recoveryAuthority{}, false, fmt.Sprintf("node %s identifies as %q, expected %q", result.IP, identity.NodeName, expectedName)
		}
		if !equalStrings(identity.MembershipView, expectedView) {
			return recoveryAuthority{}, false, fmt.Sprintf("node %s has membership view %v, expected %v", result.IP, identity.MembershipView, expectedView)
		}
		if identity.PGDataEmpty {
			if identity.PostgresSystemIdentifier != "" {
				return recoveryAuthority{}, false, fmt.Sprintf("node %s reports empty PGDATA with system ID %s", result.IP, identity.PostgresSystemIdentifier)
			}
			continue
		}
		if identity.PostgresSystemIdentifier == "" {
			return recoveryAuthority{}, false, fmt.Sprintf("node %s has PostgreSQL data but its system ID is unreadable", result.IP)
		}
		dataNodes++
		authority = recoveryAuthority{
			IP:       result.IP,
			NodeName: identity.NodeName,
			SystemID: identity.PostgresSystemIdentifier,
		}
	}

	if dataNodes != 1 {
		return recoveryAuthority{}, false, fmt.Sprintf("expected exactly one PostgreSQL data-bearing node, found %d", dataNodes)
	}
	return authority, true, fmt.Sprintf("all expected nodes confirmed %s as the sole PostgreSQL authority", authority.NodeName)
}

func discoverRecoveryAuthority(cfg *config.Config) (recoveryAuthority, bool, string) {
	otherIPs := otherIPsFromInitialCluster(cfg.EtcdInitialCluster, cfg.MyName)
	timeout := time.Duration(cfg.BootstrapPeerProbeTimeout) * time.Second
	if timeout < time.Second {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Second)
	results := probeAllPeerIdentities(ctx, cfg, otherIPs)
	cancel()
	return evaluateRecoveryAuthority(cfg, localNodeIdentity(cfg), results)
}

// confirmRecoveryAuthorityWithPeers requires the same unique authority across
// repeated probes. This prevents a single transient view during Flux churn
// from authorizing a new etcd epoch.
func confirmRecoveryAuthorityWithPeers(cfg *config.Config) (recoveryAuthority, bool, string) {
	cycles := cfg.BootstrapPeerConfirmCycles
	if cycles < 1 {
		cycles = 1
	}
	interval := time.Duration(cfg.BootstrapPeerProbeInterval) * time.Second
	if interval < time.Second {
		interval = time.Second
	}

	var confirmed recoveryAuthority
	for cycle := 1; cycle <= cycles; cycle++ {
		authority, ok, reason := discoverRecoveryAuthority(cfg)
		if !ok {
			return recoveryAuthority{}, false, reason
		}
		if cycle > 1 && authority != confirmed {
			return recoveryAuthority{}, false, fmt.Sprintf(
				"recovery authority changed between probes: %#v != %#v", confirmed, authority)
		}
		confirmed = authority
		pkglog.Infof("recovery authority confirmation %d/%d: %s (%s)",
			cycle, cycles, authority.NodeName, authority.SystemID)
		if cycle < cycles {
			time.Sleep(interval)
		}
	}
	return confirmed, true, fmt.Sprintf(
		"all expected nodes repeatedly confirmed %s as the sole PostgreSQL authority", confirmed.NodeName)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func confirmFreshClusterWithPeers(cfg *config.Config) bool {
	otherIPs := otherIPsFromInitialCluster(cfg.EtcdInitialCluster, cfg.MyName)
	if len(otherIPs) == 0 {
		pkglog.Errorf("FRESH BOOTSTRAP BLOCKED: no peers exist to confirm this is a new deployment")
		return false
	}
	cycles := cfg.BootstrapPeerConfirmCycles
	if cycles < 1 {
		cycles = 1
	}
	interval := time.Duration(cfg.BootstrapPeerProbeInterval) * time.Second
	if interval < time.Second {
		interval = time.Second
	}

	confirmed := 0
	for confirmed < cycles {
		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(cfg.BootstrapPeerProbeTimeout+1)*time.Second)
		results := probeAllPeerIdentities(ctx, cfg, otherIPs)
		cancel()
		ok, reason := evaluateFreshPeerEvidence(cfg, results)
		if !ok {
			pkglog.Errorf("FRESH BOOTSTRAP BLOCKED: %s", reason)
			return false
		}
		confirmed++
		pkglog.Infof("fresh-state peer confirmation %d/%d: %s", confirmed, cycles, reason)
		if confirmed < cycles {
			time.Sleep(interval)
		}
	}
	return true
}
