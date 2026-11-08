package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
	"github.com/RunOnFlux/flux-pg-cluster/internal/etcdmgr"
)

const testSystemID = "7658016226426196166"

func writeTestHistory(t *testing.T, dir string, cfg *config.Config, ips []string) {
	t.Helper()
	b, err := json.Marshal(clusterHistory{Version: 1, App: cfg.AppName, Scope: cfg.PatroniScope, SystemID: testSystemID, Members: ips})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(historyPath(dir), b); err != nil {
		t.Fatal(err)
	}
}

func TestIncidentEmptyReplacementCannotBootstrapEvenWithLegacyOverrides(t *testing.T) {
	for _, override := range []bool{false, true} {
		cfg := bootstrapTestConfig()
		cfg.AutoBootstrapIfFresh = true
		cfg.AllowNewClusterBootstrap = override
		cfg.AllowAnyNodeBootstrap = true
		cfg.EtcdInitialCluster = "node-10-0-0-1=http://10.0.0.1:2380,node-10-0-0-2=http://10.0.0.2:2380"
		if state := decideBootstrap(cfg, t.TempDir()); state != "" {
			t.Fatalf("empty replacements authorized %s", state)
		}
	}
}

func TestPinnedIdentityFailsClosedForBlankConflictingAndCorruptVolumes(t *testing.T) {
	cfg := bootstrapTestConfig()
	dir := t.TempDir()
	if _, err := expectedSystemIDAt(cfg, dir); err == nil {
		t.Fatal("blank replacement trusted an unknown identity")
	}
	cfg.ExpectedSystemID = testSystemID
	if id, err := expectedSystemIDAt(cfg, dir); err != nil || id != testSystemID {
		t.Fatalf("deployment pin: %s %v", id, err)
	}
	writeTestHistory(t, dir, cfg, []string{"10.0.0.3"})
	cfg.ExpectedSystemID = "7658016226426196167"
	if _, err := expectedSystemIDAt(cfg, dir); err == nil {
		t.Fatal("conflicting history accepted")
	}
	cfg.ExpectedSystemID = testSystemID
	if err := os.WriteFile(historyPath(dir), []byte("{truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := expectedSystemIDAt(cfg, dir); err == nil {
		t.Fatal("corrupt history treated as absent")
	}
}

func TestDiscoveryRemembersOmittedMembersAndSelf(t *testing.T) {
	cfg := bootstrapTestConfig()
	dir := t.TempDir()
	writeTestHistory(t, dir, cfg, []string{"10.0.0.3"})
	cfg.EtcdInitialCluster = ""
	cfg.EtcdHosts = "10.0.0.2:2379"
	cfg.ClusterSeedIPs = "10.0.0.4"
	got, err := discoveryIPsAt(cfg, dir, []string{"10.0.0.5"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v want %v", got, want)
	}
	cfg.RetiredClusterIPs = "10.0.0.1,10.0.0.3"
	got, err = discoveryIPsAt(cfg, dir, nil)
	if err != nil || !containsString(got, cfg.MyIP) || containsString(got, "10.0.0.3") {
		t.Fatalf("retirement/self guard: %v %v", got, err)
	}
}

func TestProxyRejectsForeignAndMultiplePrimariesAndClearsSessions(t *testing.T) {
	for _, candidates := range []map[string]string{nil, {"a": "foreign"}, {"a": testSystemID, "b": testSystemID}, {"a": testSystemID, "b": "foreign"}} {
		if got := selectPinnedPrimary(testSystemID, candidates); got != "" {
			t.Fatalf("unsafe primary %s", got)
		}
	}
	if got := selectPinnedPrimary(testSystemID, map[string]string{"a": testSystemID}); got != "a" {
		t.Fatal(got)
	}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	sessions := &proxySessions{connections: map[net.Conn]string{}}
	sessions.apply("a")
	if !sessions.add(server, "a") {
		t.Fatal("verified route rejected")
	}
	sessions.apply("")
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("invalid route left established connection open")
	}
	if sessions.add(server, "a") {
		t.Fatal("stale acceptance raced route invalidation")
	}
}

func TestEtcdStatePreservesWALWithoutSnapshot(t *testing.T) {
	dir := t.TempDir()
	if etcdHasLocalState(dir) {
		t.Fatal("empty state")
	}
	if err := os.MkdirAll(filepath.Join(dir, "member", "wal"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "member", "wal", "000.wal"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if !etcdHasLocalState(dir) {
		t.Fatal("WAL-only state discarded")
	}
	if err := archiveEtcdData(dir); err != nil {
		t.Fatal(err)
	}
	if !etcdRecoveryDataEmpty(dir) {
		t.Fatal("inactive archived WAL incorrectly blocks control-plane recovery")
	}
	files, err := filepath.Glob(filepath.Join(dir, ".flux-etcd-archives", "*", "member", "wal", "000.wal"))
	if err != nil || len(files) != 1 {
		t.Fatalf("archived WAL missing: %v %v", files, err)
	}
}

func TestLostEtcdStateRejoinsAsLearnerAndForeignClusterIsUntouched(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost-state", true: "foreign-cluster"}[foreign], func(t *testing.T) {
			cfg := bootstrapTestConfig()
			cfg.ExpectedSystemID = testSystemID
			cfg.HostEtcdPeerPort = 2380
			cfg.HostEtcdClientPort = 2379
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			t.Setenv("TEST_ETCD_CALLS", calls)
			id := testSystemID
			if foreign {
				id = "7658016226426196167"
			}
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$TEST_ETCD_CALLS"
case "$*" in
 *'member list'*) printf '%s\n' 'abc, started, node-10-0-0-1, http://10.0.0.1:2380, http://10.0.0.1:2379, false';;
 *'get --print-value-only'*) printf '%s\n' '` + id + `';;
 *'member remove'*|*'member add'*) exit 0;;
 *) exit 1;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "etcdctl"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			state, initial, err := tryJoinExistingWithLoader(cfg, nil, nil, true, 1, func(*config.Config) error { return nil })
			b, readErr := os.ReadFile(calls)
			if readErr != nil {
				t.Fatal(readErr)
			}
			commands := string(b)
			if foreign {
				if err == nil || strings.Contains(commands, "member remove") || strings.Contains(commands, "member add") {
					t.Fatalf("foreign cluster mutated: %s %v", commands, err)
				}
			} else {
				if err != nil || state != "existing" || initial == "" || !strings.Contains(commands, "member remove") || !strings.Contains(commands, "--learner") {
					t.Fatalf("missing learner replacement: %s %s %v", commands, state, err)
				}
			}
		})
	}
}

func TestOperatorCreationRequiresConfirmationTokenAndEmptyData(t *testing.T) {
	cfg := bootstrapTestConfig()
	dir := t.TempDir()
	token := filepath.Join(t.TempDir(), "token")
	if err := createCluster(cfg, dir, "wrong", token); err == nil {
		t.Fatal("wrong app authorized")
	}
	if err := createCluster(cfg, dir, cfg.AppName, token); err == nil {
		t.Fatal("missing token authorized")
	}
	if err := os.WriteFile(token, []byte(strings.Repeat("x", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "valuable-data"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := createCluster(cfg, dir, cfg.AppName, token); err == nil {
		t.Fatal("nonempty PGDATA overwritten")
	}
	if _, err := os.Stat(token); err != nil {
		t.Fatal("token consumed before safety validation")
	}
}

func TestDisabledRecoveryDoesNotWriteForceNewHint(t *testing.T) {
	cfg := bootstrapTestConfig()
	cfg.DeadClusterRecovery = false
	dir := t.TempDir()
	flag := filepath.Join(dir, "force-new")
	triggerForceNewCluster(cfg, nil, testSystemID, flag, filepath.Join(dir, "cooldown"), filepath.Join(dir, "no-quorum"))
	if fileExists(flag) {
		t.Fatal("disabled recovery created a force-new-cluster hint")
	}
}

func TestOperatorTokenCannotBeReusedAfterConsumption(t *testing.T) {
	cfg := bootstrapTestConfig()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte(strings.Repeat("x", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token+".consumed", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := createCluster(cfg, t.TempDir(), cfg.AppName, token); err == nil {
		t.Fatal("used token authorized initdb")
	}
}

func TestRecoveryCannotSelectAnotherDeploymentIdentity(t *testing.T) {
	cfg := recoveryTestConfig()
	cfg.ExpectedSystemID = testSystemID
	local := recoveryIdentity(cfg, cfg.MyIP, false, "7658016226426196167")
	peers := []peerIdentityResult{
		{IP: "192.0.2.10", Identity: recoveryIdentity(cfg, "192.0.2.10", true, "")},
		{IP: "192.0.2.12", Identity: recoveryIdentity(cfg, "192.0.2.12", true, "")},
	}
	if _, ok, _ := evaluateRecoveryAuthority(cfg, local, peers); ok {
		t.Fatal("sole foreign data copy overrode deployment identity")
	}
}

func TestFirstCreationSQLQuotesDatabaseAndPassword(t *testing.T) {
	cfg := recoveryTestConfig()
	cfg.PostgresDB = "customer's db"
	sql := initialApplicationSQL(cfg)
	for _, want := range []string{"IF NOT EXISTS", "PASSWORD 'super''secret'", "format('CREATE DATABASE %I OWNER admin', 'customer''s db')", "\\gexec"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("missing %q in %s", want, sql)
		}
	}
}

func TestSignedIdentityCannotBeReplayedOrAltered(t *testing.T) {
	cfg := bootstrapTestConfig()
	body := []byte(`{"pgdata_empty":false}`)
	sig := identityResponseSignature(cfg, "nonce-a", body)
	if sig == identityResponseSignature(cfg, "nonce-b", body) || sig == identityResponseSignature(cfg, "nonce-a", []byte(`{"pgdata_empty":true}`)) {
		t.Fatal("nonce/body not authenticated")
	}
}

func TestNewControlPlaneReceivesUniqueIdentity(t *testing.T) {
	a, err := newDCSBootstrapToken("cluster")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newDCSBootstrapToken("cluster")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.HasPrefix(a, "cluster-") {
		t.Fatalf("control-plane identities reused: %s %s", a, b)
	}
}

func TestPatroniClientResetsOnlyForVerifiedChangedDCS(t *testing.T) {
	for _, scenario := range []string{"unchanged", "changed", "foreign", "restart-failed"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := bootstrapTestConfig()
			cfg.ExpectedSystemID = testSystemID
			dir := t.TempDir()
			marker := filepath.Join(dir, "epoch")
			if err := os.WriteFile(marker, []byte("1\n"), 0600); err != nil {
				t.Fatal(err)
			}
			id := testSystemID
			if scenario == "foreign" {
				id = "7658016226426196167"
			}
			epoch := "2"
			if scenario == "unchanged" {
				epoch = "1"
			}
			script := `#!/bin/sh
case "$*" in
 *'get --print-value-only'*) printf '%s\n' '` + id + `';;
 *'endpoint status'*) printf '%s\n' '[{"Status":{"header":{"cluster_id":` + epoch + `}}}]';;
 *) exit 1;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "etcdctl"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			calls := 0
			err := reconcilePatroniDCSEpoch(cfg, etcdmgr.New("http://unused", nil), marker, func() error {
				calls++
				if scenario == "restart-failed" {
					return errors.New("supervisor unavailable")
				}
				return nil
			})
			data, _ := os.ReadFile(marker)
			switch scenario {
			case "unchanged":
				if err != nil || calls != 0 || string(data) != "1\n" {
					t.Fatalf("unexpected reset: %d %s %v", calls, data, err)
				}
			case "changed":
				if err != nil || calls != 1 || string(data) != "2\n" {
					t.Fatalf("client not reset: %d %s %v", calls, data, err)
				}
			case "foreign":
				if err == nil || calls != 0 || string(data) != "1\n" {
					t.Fatalf("foreign DCS accepted: %d %s %v", calls, data, err)
				}
			case "restart-failed":
				if err == nil || string(data) != "1\n" {
					t.Fatal("failed restart consumed identity change")
				}
			}
		})
	}
}
