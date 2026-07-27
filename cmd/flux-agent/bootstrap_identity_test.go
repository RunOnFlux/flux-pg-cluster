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
