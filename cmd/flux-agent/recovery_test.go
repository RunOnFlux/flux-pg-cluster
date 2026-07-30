package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
)

func recoveryTestConfig() *config.Config {
	return &config.Config{
		AppName:                     "postgres-cluster",
		PatroniScope:                "postgres-cluster",
		MyName:                      "node-82-67-53-6",
		MyIP:                        "82.67.53.6",
		SSLEnabled:                  true,
		HostEtcdClientPort:          12379,
		HostEtcdPeerPort:            12380,
		EtcdClientPort:              2379,
		EtcdInitialCluster:          "node-78-117-242-56=https://78.117.242.56:12380,node-82-67-53-6=https://82.67.53.6:12380,node-90-70-74-189=https://90.70.74.189:12380",
		PatroniTTL:                  30,
		PatroniLoopWait:             10,
		PatroniRetryTimeout:         30,
		PatroniMaxLag:               33554432,
		PostgresSuperuserPassword:   "super'secret",
		PostgresReplicationPassword: "repl'secret",
	}
}

func recoveryIdentity(cfg *config.Config, ip string, empty bool, systemID string) nodeIdentity {
	return nodeIdentity{
		AppName:                  cfg.AppName,
		PatroniScope:             cfg.PatroniScope,
		NodeName:                 nodeNameFromIP(ip),
		PGDataEmpty:              empty,
		PostgresSystemIdentifier: systemID,
		EtcdDataEmpty:            empty,
		MembershipView:           membershipNames(cfg.EtcdInitialCluster),
	}
}

func TestRecoveryAuthorityAllowsOnlySoleDataNode(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryIdentity(cfg, cfg.MyIP, true, "")
	results := []peerIdentityResult{
		{IP: "78.117.242.56", Identity: recoveryIdentity(cfg, "78.117.242.56", true, "")},
		{IP: "90.70.74.189", Identity: recoveryIdentity(cfg, "90.70.74.189", false, "7658016226426196166")},
	}

	authority, ok, reason := evaluateRecoveryAuthority(cfg, local, results)
	if !ok {
		t.Fatalf("expected safe recovery authority: %s", reason)
	}
	if authority.IP != "90.70.74.189" || authority.SystemID != "7658016226426196166" {
		t.Fatalf("unexpected authority: %#v", authority)
	}
}

func TestNonCandidateCanOnlyEvaluateDataBearingRecovery(t *testing.T) {
	if !mayEvaluateBootstrap(false, true, true) {
		t.Fatal("a non-deterministic node with restored PGDATA must be allowed to prove recovery authority")
	}
	if mayEvaluateBootstrap(false, true, false) {
		t.Fatal("a fresh non-candidate must not reach bootstrap")
	}
	if mayEvaluateBootstrap(false, false, true) {
		t.Fatal("a non-candidate must not bypass disabled dead-cluster recovery")
	}
	if !mayEvaluateBootstrap(true, false, false) {
		t.Fatal("the deterministic candidate must still be able to evaluate fresh bootstrap")
	}
}

func TestRecoveryAuthorityBlocksMultipleDataNodes(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryIdentity(cfg, cfg.MyIP, false, "same-system-id")
	results := []peerIdentityResult{
		{IP: "78.117.242.56", Identity: recoveryIdentity(cfg, "78.117.242.56", true, "")},
		{IP: "90.70.74.189", Identity: recoveryIdentity(cfg, "90.70.74.189", false, "same-system-id")},
	}

	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("multiple data-bearing nodes must block automatic force-new, even with matching system IDs")
	}
}

func TestRecoveryAuthorityBlocksUnreachableOrInconsistentPeer(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryIdentity(cfg, cfg.MyIP, true, "")
	results := []peerIdentityResult{
		{IP: "78.117.242.56", Err: errTestUnreachable{}},
		{IP: "90.70.74.189", Identity: recoveryIdentity(cfg, "90.70.74.189", false, "7658016226426196166")},
	}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("an unreachable peer must block recovery")
	}

	results[0] = peerIdentityResult{IP: "78.117.242.56", Identity: recoveryIdentity(cfg, "78.117.242.56", true, "")}
	results[0].Identity.MembershipView = []string{"node-78-117-242-56"}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("a conflicting membership view must block recovery")
	}
}

type errTestUnreachable struct{}

func (errTestUnreachable) Error() string { return "unreachable" }

func TestEtcdTopologyUsesLoopbackOnlyForPatroniSelf(t *testing.T) {
	cfg := recoveryTestConfig()
	ips := []string{"78.117.242.56", "82.67.53.6", "90.70.74.189"}
	publicHosts, patroniHosts, initial := buildEtcdTopology(cfg, ips)

	if publicHosts != "78.117.242.56:12379,82.67.53.6:12379,90.70.74.189:12379" {
		t.Fatalf("unexpected public hosts: %s", publicHosts)
	}
	if patroniHosts != "127.0.0.1:2379" {
		t.Fatalf("unexpected Patroni hosts: %s", patroniHosts)
	}
	if !strings.Contains(initial, "node-82-67-53-6=https://82.67.53.6:12380") {
		t.Fatalf("etcd peer advertisement must remain public: %s", initial)
	}
}

func TestMergePatroniRecoveryConfigPreservesCustomSettings(t *testing.T) {
	cfg := recoveryTestConfig()
	raw := `{"ttl":45,"postgresql":{"parameters":{"max_connections":321},"pg_hba":["local all custom peer"]},"custom":{"keep":"me"}}`
	merged, changed, err := mergePatroniRecoveryConfig(raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected missing recovery invariants to be added")
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(merged), &got); err != nil {
		t.Fatal(err)
	}
	if got["ttl"].(float64) != 45 {
		t.Fatal("existing dynamic ttl was overwritten")
	}
	if got["custom"].(map[string]interface{})["keep"] != "me" {
		t.Fatal("unrelated dynamic configuration was not preserved")
	}
	pg := got["postgresql"].(map[string]interface{})
	if pg["parameters"].(map[string]interface{})["max_connections"].(float64) != 321 {
		t.Fatal("existing PostgreSQL parameters were overwritten")
	}
	if pg["use_pg_rewind"] != true || len(pg["pg_hba"].([]interface{})) != len(requiredPatroniHBA)+1 {
		t.Fatal("recovery invariants were not applied")
	}
	if pg["pg_hba"].([]interface{})[0] != "local all custom peer" {
		t.Fatal("custom HBA rules were overwritten")
	}

	again, changed, err := mergePatroniRecoveryConfig(merged, cfg)
	if err != nil || changed || again != merged {
		t.Fatalf("merge must be idempotent: changed=%v err=%v", changed, err)
	}
}

func TestPostgresCredentialSQLIsIdempotentAndEscapesPasswords(t *testing.T) {
	sql := postgresCredentialSQL(recoveryTestConfig())
	for _, want := range []string{
		"IF NOT EXISTS",
		"CREATE ROLE replicator WITH LOGIN REPLICATION",
		"ALTER ROLE replicator WITH LOGIN REPLICATION",
		"PASSWORD 'repl''secret'",
		"ALTER ROLE postgres WITH LOGIN SUPERUSER REPLICATION",
		"PASSWORD 'super''secret'",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("credential SQL missing %q:\n%s", want, sql)
		}
	}
}
