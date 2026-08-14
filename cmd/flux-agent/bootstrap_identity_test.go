package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
)

func bootstrapTestConfig() *config.Config {
	return &config.Config{
		AppName:                     "capsule-postgres-prod",
		PatroniScope:                "postgres-cluster",
		MyName:                      "node-10-0-0-1",
		MyIP:                        "10.0.0.1",
		PostgresReplicationPassword: "test-replication-secret",
		EtcdInitialCluster: "node-10-0-0-1=https://10.0.0.1:12380," +
			"node-10-0-0-2=https://10.0.0.2:12380," +
			"node-10-0-0-3=https://10.0.0.3:12380",
	}
}

func emptyPeer(cfg *config.Config, name string) nodeIdentity {
	return nodeIdentity{
		AppName:        cfg.AppName,
		PatroniScope:   cfg.PatroniScope,
		NodeName:       name,
		PGDataEmpty:    true,
		EtcdDataEmpty:  true,
		MembershipView: membershipNames(cfg.EtcdInitialCluster),
	}
}

func TestMembershipNamesSorted(t *testing.T) {
	initial := "node-c=https://10.0.0.3:2380,node-a=https://10.0.0.1:2380,node-b=https://10.0.0.2:2380"
	got := membershipNames(initial)
	want := []string{"node-a", "node-b", "node-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("membershipNames() = %v, want %v", got, want)
	}
}

func TestFreshPeerEvidenceAllowsOnlyUnanimousEmptyPeers(t *testing.T) {
	cfg := bootstrapTestConfig()
	results := []peerIdentityResult{
		{IP: "10.0.0.2", Identity: emptyPeer(cfg, "node-10-0-0-2")},
		{IP: "10.0.0.3", Identity: emptyPeer(cfg, "node-10-0-0-3")},
	}
	ok, reason := evaluateFreshPeerEvidence(cfg, results)
	if !ok {
		t.Fatalf("expected unanimous empty peers to allow bootstrap: %s", reason)
	}
}

func TestFreshPeerEvidenceBlocksUnreachablePeer(t *testing.T) {
	cfg := bootstrapTestConfig()
	results := []peerIdentityResult{
		{IP: "10.0.0.2", Identity: emptyPeer(cfg, "node-10-0-0-2")},
		{IP: "10.0.0.3", Err: errors.New("timeout")},
	}
	ok, _ := evaluateFreshPeerEvidence(cfg, results)
	if ok {
		t.Fatal("unreachable peer must never count as empty")
	}
}

func TestFreshPeerEvidenceBlocksPreservedPGData(t *testing.T) {
	cfg := bootstrapTestConfig()
	old := emptyPeer(cfg, "node-10-0-0-3")
	old.PGDataEmpty = false
	old.PostgresSystemIdentifier = "7658016226426196166"
	results := []peerIdentityResult{
		{IP: "10.0.0.2", Identity: emptyPeer(cfg, "node-10-0-0-2")},
		{IP: "10.0.0.3", Identity: old},
	}
	ok, _ := evaluateFreshPeerEvidence(cfg, results)
	if ok {
		t.Fatal("peer with preserved PGDATA must block a fresh epoch")
	}
}

func TestFreshPeerEvidenceBlocksExistingEtcd(t *testing.T) {
	cfg := bootstrapTestConfig()
	existing := emptyPeer(cfg, "node-10-0-0-3")
	existing.EtcdDataEmpty = false
	results := []peerIdentityResult{
		{IP: "10.0.0.2", Identity: emptyPeer(cfg, "node-10-0-0-2")},
		{IP: "10.0.0.3", Identity: existing},
	}
	ok, _ := evaluateFreshPeerEvidence(cfg, results)
	if ok {
		t.Fatal("peer with existing etcd data must block a fresh epoch")
	}
}

func TestFreshPeerEvidenceBlocksMembershipChurn(t *testing.T) {
	cfg := bootstrapTestConfig()
	churning := emptyPeer(cfg, "node-10-0-0-3")
	churning.MembershipView = []string{"node-10-0-0-1", "node-10-0-0-3", "node-10-0-0-4"}
	results := []peerIdentityResult{
		{IP: "10.0.0.2", Identity: emptyPeer(cfg, "node-10-0-0-2")},
		{IP: "10.0.0.3", Identity: churning},
	}
	ok, _ := evaluateFreshPeerEvidence(cfg, results)
	if ok {
		t.Fatal("disagreeing membership views must block bootstrap during churn")
	}
}

func TestFreshPeerEvidenceBlocksWrongApp(t *testing.T) {
	cfg := bootstrapTestConfig()
	wrong := emptyPeer(cfg, "node-10-0-0-3")
	wrong.AppName = "another-app"
	results := []peerIdentityResult{
		{IP: "10.0.0.2", Identity: emptyPeer(cfg, "node-10-0-0-2")},
		{IP: "10.0.0.3", Identity: wrong},
	}
	ok, _ := evaluateFreshPeerEvidence(cfg, results)
	if ok {
		t.Fatal("peer from another application must block bootstrap")
	}
}

func TestFreshPeerEvidenceBlocksWrongNodeIdentity(t *testing.T) {
	cfg := bootstrapTestConfig()
	wrong := emptyPeer(cfg, "node-10-0-0-99")
	results := []peerIdentityResult{
		{IP: "10.0.0.2", Identity: wrong},
		{IP: "10.0.0.3", Identity: emptyPeer(cfg, "node-10-0-0-3")},
	}
	ok, _ := evaluateFreshPeerEvidence(cfg, results)
	if ok {
		t.Fatal("peer identity that does not match its expected IP must block bootstrap")
	}
}

func TestDirectoryIsEmptyIsStrict(t *testing.T) {
	dir := t.TempDir()
	if !directoryIsEmpty(dir) {
		t.Fatal("new temporary directory should be empty")
	}
	if err := os.WriteFile(filepath.Join(dir, "partial-basebackup"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if directoryIsEmpty(dir) {
		t.Fatal("any leftover file must count as preserved state")
	}
}

func TestEtcdDataDirectoryIsFreshAllowsOnlyAgentMarker(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".etcd3_api")
	if err := os.WriteFile(marker, []byte("etcd3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !etcdDataDirectoryIsFresh(dir) {
		t.Fatal("the agent-owned marker must not count as persisted etcd state")
	}

	if err := os.Mkdir(filepath.Join(dir, "member"), 0o700); err != nil {
		t.Fatal(err)
	}
	if etcdDataDirectoryIsFresh(dir) {
		t.Fatal("any etcd state alongside the marker must block fresh bootstrap")
	}
}

func TestEtcdDataDirectoryIsFreshRejectsMarkerDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".etcd3_api"), 0o700); err != nil {
		t.Fatal(err)
	}
	if etcdDataDirectoryIsFresh(dir) {
		t.Fatal("only the regular marker file may be ignored")
	}
}

func TestPostgresDurableRoleUsesRecoverySignalFiles(t *testing.T) {
	dir := t.TempDir()
	if role := postgresDurableRole(dir); role != "" {
		t.Fatalf("empty PGDATA role = %q, want unknown", role)
	}
	if err := os.Mkdir(filepath.Join(dir, "global"), 0o700); err != nil {
		t.Fatal(err)
	}
	if role := postgresDurableRole(dir); role != "primary" {
		t.Fatalf("PGDATA without a recovery signal role = %q, want primary", role)
	}
	if err := os.WriteFile(filepath.Join(dir, "standby.signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if role := postgresDurableRole(dir); role != "replica" {
		t.Fatalf("PGDATA with standby.signal role = %q, want replica", role)
	}
}

func TestIdentityEndpointRequiresSharedProbeToken(t *testing.T) {
	cfg := bootstrapTestConfig()

	unauthorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, clusterIdentityPath, nil)
	writeIdentityResponse(unauthorized, cfg, req)
	if unauthorized.Code != http.StatusForbidden {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusForbidden)
	}

	authorized := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, clusterIdentityPath, nil)
	req.Header.Set("X-Flux-Cluster-Probe", identityProbeToken(cfg))
	writeIdentityResponse(authorized, cfg, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d", authorized.Code, http.StatusOK)
	}
	var identity nodeIdentity
	if err := json.Unmarshal(authorized.Body.Bytes(), &identity); err != nil {
		t.Fatalf("decode identity response: %v", err)
	}
	if identity.AppName != cfg.AppName || identity.NodeName != cfg.MyName {
		t.Fatalf("identity = %+v, want app=%q node=%q", identity, cfg.AppName, cfg.MyName)
	}
}

func TestIdentityEndpointReloadsCurrentCredentialsAndMembership(t *testing.T) {
	startup := bootstrapTestConfig()
	startup.PostgresReplicationPassword = "stale-secret"
	startup.EtcdInitialCluster = "node-10-0-0-1=https://10.0.0.1:12380"

	current := *startup
	current.PostgresReplicationPassword = "current-secret"
	current.EtcdInitialCluster = "node-10-0-0-1=https://10.0.0.1:12380," +
		"node-10-0-0-2=https://10.0.0.2:12380," +
		"node-10-0-0-3=https://10.0.0.3:12380"

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, clusterIdentityPath, nil)
	req.Header.Set("X-Flux-Cluster-Probe", identityProbeToken(&current))
	loads := 0
	writeCurrentIdentityResponse(recorder, startup, req, func(cfg *config.Config) error {
		loads++
		*cfg = current
		return nil
	})

	if loads != 1 {
		t.Fatalf("cluster_env reloads = %d, want 1", loads)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("identity status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var identity nodeIdentity
	if err := json.Unmarshal(recorder.Body.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	wantView := membershipNames(current.EtcdInitialCluster)
	if !reflect.DeepEqual(identity.MembershipView, wantView) {
		t.Fatalf("membership view = %v, want %v", identity.MembershipView, wantView)
	}
	if startup.PostgresReplicationPassword != "stale-secret" {
		t.Fatal("identity reload mutated the proxy's shared startup config")
	}
}
