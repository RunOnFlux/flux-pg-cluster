package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
)

func TestJoinDiscoveryReloadsTopologyOnEveryAttempt(t *testing.T) {
	cfg := recoveryTestConfig()
	cfg.EtcdInitialCluster = "node-192-0-2-11=https://192.0.2.11:12380"
	cfg.EtcdJoinRetryDelaySeconds = 0

	loads := 0
	_, _, err := tryJoinExistingWithLoader(cfg, nil, nil, false, 3, func(cfg *config.Config) error {
		loads++
		// Keep this as a self-only view so the test performs no network I/O. The
		// important regression is that an initially empty peer list no longer
		// returns before loading, and that all retry attempts refresh the input.
		cfg.EtcdInitialCluster = "node-192-0-2-11=https://192.0.2.11:12380"
		return nil
	})
	if err == nil {
		t.Fatal("empty peer topology should still report no reachable peers")
	}
	if loads != 3 {
		t.Fatalf("cluster_env reloads = %d, want one for each of 3 attempts", loads)
	}
}

func TestJoinDiscoveryRetriesWhenReloadFailsWithNoTopology(t *testing.T) {
	cfg := recoveryTestConfig()
	cfg.EtcdJoinRetryDelaySeconds = 0

	loads := 0
	_, _, err := tryJoinExistingWithLoader(cfg, nil, nil, false, 2, func(*config.Config) error {
		loads++
		return errors.New("temporary read failure")
	})
	if err == nil {
		t.Fatal("unreachable fallback peers should report no reachable peers")
	}
	if loads != 2 {
		t.Fatalf("cluster_env reloads = %d, want 2", loads)
	}
}

func TestBootstrapRetryDelayOutlivesSupervisorStartupWindow(t *testing.T) {
	if got := bootstrapRetryDelay(0); got != 2*time.Second {
		t.Fatalf("minimum retry delay = %s, want 2s", got)
	}
	if got := bootstrapRetryDelay(10); got != 10*time.Second {
		t.Fatalf("configured retry delay = %s, want 10s", got)
	}
}

func recoveryTestConfig() *config.Config {
	return &config.Config{
		AppName:                     "postgres-cluster",
		PatroniScope:                "postgres-cluster",
		MyName:                      "node-192-0-2-11",
		MyIP:                        "192.0.2.11",
		SSLEnabled:                  true,
		HostEtcdClientPort:          12379,
		HostEtcdPeerPort:            12380,
		EtcdClientPort:              2379,
		EtcdInitialCluster:          "node-192-0-2-10=https://192.0.2.10:12380,node-192-0-2-11=https://192.0.2.11:12380,node-192-0-2-12=https://192.0.2.12:12380",
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
		{IP: "192.0.2.10", Identity: recoveryIdentity(cfg, "192.0.2.10", true, "")},
		{IP: "192.0.2.12", Identity: recoveryIdentity(cfg, "192.0.2.12", false, "7658016226426196166")},
	}

	authority, ok, reason := evaluateRecoveryAuthority(cfg, local, results)
	if !ok {
		t.Fatalf("expected safe recovery authority: %s", reason)
	}
	if authority.IP != "192.0.2.12" || authority.SystemID != "7658016226426196166" {
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

func TestRecoveryAuthorityAllowsUniqueDurablePrimaryAfterTotalDCSLoss(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryIdentity(cfg, cfg.MyIP, false, "same-system-id")
	local.EtcdDataEmpty = true
	local.PostgresDurableRole = "replica"
	primary := recoveryIdentity(cfg, "192.0.2.10", false, "same-system-id")
	primary.EtcdDataEmpty = true
	primary.PostgresDurableRole = "primary"
	replica := recoveryIdentity(cfg, "192.0.2.12", false, "same-system-id")
	replica.EtcdDataEmpty = true
	replica.PostgresDurableRole = "replica"
	results := []peerIdentityResult{
		{IP: "192.0.2.10", Identity: primary},
		{IP: "192.0.2.12", Identity: replica},
	}

	authority, ok, reason := evaluateRecoveryAuthority(cfg, local, results)
	if !ok {
		t.Fatalf("expected safe total-DCS-loss recovery: %s", reason)
	}
	if authority.IP != "192.0.2.10" || authority.SystemID != "same-system-id" {
		t.Fatalf("unexpected authority: %#v", authority)
	}
}

func TestRecoveryAuthorityBlocksMultipleDurablePrimaries(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryIdentity(cfg, cfg.MyIP, false, "same-system-id")
	local.EtcdDataEmpty = true
	local.PostgresDurableRole = "primary"
	results := []peerIdentityResult{
		{IP: "192.0.2.10", Identity: recoveryDataIdentity(cfg, "192.0.2.10", "same-system-id", "primary", true)},
		{IP: "192.0.2.12", Identity: recoveryDataIdentity(cfg, "192.0.2.12", "same-system-id", "replica", true)},
	}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("multiple durable primary candidates must block automatic recovery")
	}
}

func TestRecoveryAuthorityBlocksMismatchedSystemIDs(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryDataIdentity(cfg, cfg.MyIP, "system-a", "primary", true)
	results := []peerIdentityResult{
		{IP: "192.0.2.10", Identity: recoveryDataIdentity(cfg, "192.0.2.10", "system-a", "replica", true)},
		{IP: "192.0.2.12", Identity: recoveryDataIdentity(cfg, "192.0.2.12", "system-b", "replica", true)},
	}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("mismatched PostgreSQL system IDs must block automatic recovery")
	}
}

func TestRecoveryAuthorityBlocksMultipleCopiesWithRetainedEtcd(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryDataIdentity(cfg, cfg.MyIP, "same-system-id", "primary", true)
	results := []peerIdentityResult{
		{IP: "192.0.2.10", Identity: recoveryDataIdentity(cfg, "192.0.2.10", "same-system-id", "replica", false)},
		{IP: "192.0.2.12", Identity: recoveryDataIdentity(cfg, "192.0.2.12", "same-system-id", "replica", true)},
	}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("retained etcd state must block multi-copy total-DCS-loss recovery")
	}
}

func TestRecoveryAuthorityBlocksAmbiguousDurableRole(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryDataIdentity(cfg, cfg.MyIP, "same-system-id", "primary", true)
	results := []peerIdentityResult{
		{IP: "192.0.2.10", Identity: recoveryDataIdentity(cfg, "192.0.2.10", "same-system-id", "replica", true)},
		{IP: "192.0.2.12", Identity: recoveryDataIdentity(cfg, "192.0.2.12", "same-system-id", "", true)},
	}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("a missing durable role must block multi-copy recovery")
	}
}

func TestRecoveryAuthoritySelectsMostAdvancedReplicaWhenPrimaryIsGone(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryReplicaIdentity(cfg, cfg.MyIP, "same-system-id", 7, "0/500")
	results := []peerIdentityResult{
		{IP: "192.0.2.10", Identity: recoveryReplicaIdentity(cfg, "192.0.2.10", "same-system-id", 7, "0/700")},
		{IP: "192.0.2.12", Identity: recoveryReplicaIdentity(cfg, "192.0.2.12", "same-system-id", 7, "0/600")},
	}
	authority, ok, reason := evaluateRecoveryAuthority(cfg, local, results)
	if !ok {
		t.Fatalf("expected replica recovery authority: %s", reason)
	}
	if authority.IP != "192.0.2.10" {
		t.Fatalf("authority IP = %s, want most advanced replica 192.0.2.10", authority.IP)
	}
}

func TestRecoveryAuthorityUsesDeterministicNameForReplicaPositionTie(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryReplicaIdentity(cfg, cfg.MyIP, "same-system-id", 7, "0/700")
	results := []peerIdentityResult{
		{IP: "192.0.2.10", Identity: recoveryReplicaIdentity(cfg, "192.0.2.10", "same-system-id", 7, "0/700")},
		{IP: "192.0.2.12", Identity: recoveryReplicaIdentity(cfg, "192.0.2.12", "same-system-id", 7, "0/700")},
	}
	authority, ok, reason := evaluateRecoveryAuthority(cfg, local, results)
	if !ok {
		t.Fatalf("expected deterministic tied replica authority: %s", reason)
	}
	if authority.NodeName != "node-192-0-2-10" {
		t.Fatalf("authority = %s, want lexicographically smallest node name", authority.NodeName)
	}
}

func TestRecoveryAuthorityBlocksDivergentReplicaTimelines(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryReplicaIdentity(cfg, cfg.MyIP, "same-system-id", 7, "0/700")
	results := []peerIdentityResult{
		{IP: "192.0.2.10", Identity: recoveryReplicaIdentity(cfg, "192.0.2.10", "same-system-id", 8, "0/900")},
		{IP: "192.0.2.12", Identity: recoveryReplicaIdentity(cfg, "192.0.2.12", "same-system-id", 7, "0/800")},
	}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("replicas on divergent timelines must block automatic recovery")
	}
}

func TestRecoveryAuthorityBlocksReplicaWithoutWALPosition(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryReplicaIdentity(cfg, cfg.MyIP, "same-system-id", 7, "0/700")
	results := []peerIdentityResult{
		{IP: "192.0.2.10", Identity: recoveryReplicaIdentity(cfg, "192.0.2.10", "same-system-id", 7, "")},
		{IP: "192.0.2.12", Identity: recoveryReplicaIdentity(cfg, "192.0.2.12", "same-system-id", 7, "0/800")},
	}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("a replica without durable WAL progress must block automatic recovery")
	}
}

func recoveryDataIdentity(cfg *config.Config, ip, systemID, role string, etcdEmpty bool) nodeIdentity {
	identity := recoveryIdentity(cfg, ip, false, systemID)
	identity.PostgresDurableRole = role
	identity.EtcdDataEmpty = etcdEmpty
	return identity
}

func recoveryReplicaIdentity(cfg *config.Config, ip, systemID string, timeline uint64, lsn string) nodeIdentity {
	identity := recoveryDataIdentity(cfg, ip, systemID, "replica", true)
	identity.PostgresTimeline = timeline
	identity.PostgresWALPosition = lsn
	return identity
}

func TestPostgresControlDataSelectsGreatestDurablePosition(t *testing.T) {
	control := parsePostgresControlData(`
Database system identifier:           7673588256370979449
Database cluster state:               shut down in recovery
Latest checkpoint location:           0/500
Latest checkpoint's TimeLineID:       7
Minimum recovery ending location:     0/900
Min recovery ending loc's timeline:   7
`)
	if control.SystemID != "7673588256370979449" || control.State != "shut down in recovery" {
		t.Fatalf("unexpected control data: %#v", control)
	}
	position, err := control.recoveryPosition()
	if err != nil {
		t.Fatal(err)
	}
	if position.Timeline != 7 || formatPostgresLSN(position.LSN) != "0/900" {
		t.Fatalf("recovery position = %#v, want timeline 7 LSN 0/900", position)
	}
}

func TestPostgresLSNRoundTrip(t *testing.T) {
	parsed, err := parsePostgresLSN("16/B374D848")
	if err != nil {
		t.Fatal(err)
	}
	if got := formatPostgresLSN(parsed); got != "16/B374D848" {
		t.Fatalf("formatted LSN = %s", got)
	}
}

func TestRecoveryAuthorityBlocksUnreachableOrInconsistentPeer(t *testing.T) {
	cfg := recoveryTestConfig()
	local := recoveryIdentity(cfg, cfg.MyIP, true, "")
	results := []peerIdentityResult{
		{IP: "192.0.2.10", Err: errTestUnreachable{}},
		{IP: "192.0.2.12", Identity: recoveryIdentity(cfg, "192.0.2.12", false, "7658016226426196166")},
	}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("an unreachable peer must block recovery")
	}

	results[0] = peerIdentityResult{IP: "192.0.2.10", Identity: recoveryIdentity(cfg, "192.0.2.10", true, "")}
	results[0].Identity.MembershipView = []string{"node-192-0-2-10"}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, results); ok {
		t.Fatal("a conflicting membership view must block recovery")
	}
}

type errTestUnreachable struct{}

func (errTestUnreachable) Error() string { return "unreachable" }

func TestEtcdTopologyUsesLoopbackOnlyForPatroniSelf(t *testing.T) {
	cfg := recoveryTestConfig()
	ips := []string{"192.0.2.10", "192.0.2.11", "192.0.2.12"}
	publicHosts, patroniHosts, initial := buildEtcdTopology(cfg, ips)

	if publicHosts != "192.0.2.10:12379,192.0.2.11:12379,192.0.2.12:12379" {
		t.Fatalf("unexpected public hosts: %s", publicHosts)
	}
	if patroniHosts != "127.0.0.1:2379" {
		t.Fatalf("unexpected Patroni hosts: %s", patroniHosts)
	}
	if !strings.Contains(initial, "node-192-0-2-11=https://192.0.2.11:12380") {
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
