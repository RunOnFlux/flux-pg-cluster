package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/etcdmgr"
	"github.com/RunOnFlux/flux-pg-cluster/internal/patroni"
)

func TestLegacyReplacementRequiresTheClusterCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(patroni.NodeInfo{State: "running", Role: "replica", DatabaseSystemIdentifier: testSystemID})
	}))
	defer server.Close()
	cfg := bootstrapTestConfig()
	cfg.SSLEnabled = true
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	cfg.HostPatroniAPIPort, _ = strconv.Atoi(port)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := legacyReplacementInfoAt(context.Background(), cfg, "127.0.0.1", caPath)
	if err != nil || !runningReplacementDatabase(info, testSystemID) {
		t.Fatal(info, err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(2), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyReplacementInfoAt(context.Background(), cfg, "127.0.0.1", caPath); err == nil {
		t.Fatal("foreign TLS authority accepted")
	}
}

func TestReplacementEvidenceAuthenticatesNonceAndRequiresQuorum(t *testing.T) {
	for _, mode := range []string{"valid", "unsigned", "replayed", "foreign-app", "foreign-id", "foreign-dcs", "missing-proof", "no-quorum", "empty-peer", "partial-peer"} {
		t.Run(mode, func(t *testing.T) {
			cfg := bootstrapTestConfig()
			dir := t.TempDir()
			script := `#!/bin/sh
case "$*" in
  *"get "*)
    if [ "$NO_QUORUM" = 1 ]; then exit 1; fi
    case "$*" in *initialize*) echo 7658016226426196166;; *leader*) echo node-127-0-0-1;; esac;;
  *"endpoint status"*) echo '[{"Status":{"header":{"cluster_id":43981}}}]';;
  *"member list"*) echo 'a, started, node-127-0-0-1, http://127.0.0.1:2380, http://127.0.0.1:2379, false';;
  *) exit 1;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "etcdctl"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			if mode == "no-quorum" {
				t.Setenv("NO_QUORUM", "1")
			} else {
				t.Setenv("NO_QUORUM", "0")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Flux-Join-Probe") != "1" || !validIdentityProbeToken(cfg, r.Header.Get("X-Flux-Cluster-Probe")) {
					t.Error("join probe was not authenticated")
				}
				identity := nodeIdentity{AppName: cfg.AppName, PatroniScope: cfg.PatroniScope, NodeName: "node-127-0-0-1", PostgresSystemIdentifier: testSystemID, Join: &replacementProof{SystemID: testSystemID, DCSID: "abcd", Leader: "node-127-0-0-1"}}
				switch mode {
				case "foreign-app":
					identity.AppName = "foreign"
				case "foreign-id":
					identity.PostgresSystemIdentifier = "7658016226426196167"
				case "foreign-dcs":
					identity.Join.DCSID = "foreign"
				case "missing-proof":
					identity.Join = nil
				case "empty-peer", "partial-peer":
					identity.Join = nil
					identity.PostgresSystemIdentifier = ""
					identity.PGDataEmpty = mode == "empty-peer"
				}
				body, _ := json.Marshal(identity)
				nonce := r.Header.Get("X-Flux-Probe-Nonce")
				if mode == "replayed" {
					nonce = "old-request"
				}
				if mode != "unsigned" {
					w.Header().Set("X-Flux-Identity-Signature", identityResponseSignature(cfg, nonce, body))
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
			cfg.HostProxyPort, _ = strconv.Atoi(port)
			ec := etcdmgr.New("http://127.0.0.1:2379", nil)
			_, err := replacementEvidence(context.Background(), cfg, "127.0.0.1", ec)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("mode %s: %v", mode, err)
			}
			if (mode == "empty-peer" || mode == "partial-peer") && errors.Is(err, errConflictingReplacement) {
				t.Fatal("a peer still cloning must not veto a healthy cluster's identity")
			}
		})
	}
}

func TestReplacementLearningPersistsWithoutDeploymentPin(t *testing.T) {
	cfg := bootstrapTestConfig()
	data := t.TempDir()
	marker := filepath.Join(t.TempDir(), ".flux-pg-identity.json")
	if _, err := expectedSystemIDAt(cfg, data); !errors.Is(err, errUnknownIdentity) {
		t.Fatalf("empty replacement: %v", err)
	}
	if err := saveReplacementIdentity(cfg, data, marker, testSystemID, []string{"10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if !directoryIsEmpty(data) {
		t.Fatal("learning identity must not pollute PGDATA or prevent basebackup")
	}
	restarted := bootstrapTestConfig()
	id, err := learnedSystemID(restarted, data, marker)
	if err != nil || id != testSystemID || restarted.ExpectedSystemID != "" {
		t.Fatalf("restart without ENV: id=%q err=%v", id, err)
	}
	if err := restoreReplacementIdentity(restarted, marker); err != nil || restarted.ClusterSeedIPs != "10.0.0.1" {
		t.Fatalf("remembered peer discovery: %v", err)
	}
}

func TestReplacementLearningPreservesPartialOrConflictingState(t *testing.T) {
	for _, kind := range []string{"partial", "deployment", "history", "corrupt", "scope"} {
		t.Run(kind, func(t *testing.T) {
			cfg := bootstrapTestConfig()
			data, dir := t.TempDir(), t.TempDir()
			marker := filepath.Join(dir, "identity.json")
			switch kind {
			case "partial":
				if err := os.WriteFile(filepath.Join(data, "preserve-me"), []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
			case "deployment":
				cfg.ExpectedSystemID = "7658016226426196167"
			case "history", "scope":
				if err := saveReplacementIdentity(cfg, data, marker, "7658016226426196167", nil); err != nil {
					t.Fatal(err)
				}
				cfg.ExpectedSystemID = ""
				if kind == "scope" {
					cfg.PatroniScope = "another-cluster"
				}
			case "corrupt":
				if err := os.WriteFile(marker, []byte("{bad"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(marker)
			if err := saveReplacementIdentity(cfg, data, marker, testSystemID, nil); err == nil {
				t.Fatal("unsafe identity accepted")
			}
			after, _ := os.ReadFile(marker)
			if string(before) != string(after) {
				t.Fatal("existing identity changed")
			}
		})
	}
}

func TestReplacementEvidenceRequiresOneConsistentInitializedCluster(t *testing.T) {
	valid := replacementProof{SystemID: testSystemID, DCSID: "abcd", Leader: "node-10-0-0-1"}
	if got, err := agreeReplacementEvidence([]replacementProof{valid, valid}); err != nil || got != valid {
		t.Fatal(got, err)
	}
	for _, proofs := range [][]replacementProof{
		nil, {{SystemID: testSystemID}},
		{valid, {SystemID: "7658016226426196167", DCSID: valid.DCSID, Leader: valid.Leader}},
		{valid, {SystemID: valid.SystemID, DCSID: "different", Leader: valid.Leader}},
		{valid, {SystemID: valid.SystemID, DCSID: valid.DCSID, Leader: "changed-leader"}},
	} {
		if _, err := agreeReplacementEvidence(proofs); err == nil {
			t.Fatal("ambiguous evidence accepted", proofs)
		}
	}
}

func TestReplacementCannotTrustPlaintextLegacyOrStoppedDatabase(t *testing.T) {
	if _, err := legacyReplacementInfo(context.Background(), bootstrapTestConfig(), "192.0.2.1"); err == nil {
		t.Fatal("unsigned plaintext legacy peer trusted")
	}
	for _, info := range []*patroni.NodeInfo{
		nil,
		{DatabaseSystemIdentifier: testSystemID, Role: "primary", State: "stopped"},
		{DatabaseSystemIdentifier: "7658016226426196167", Role: "primary", State: "running"},
	} {
		if runningReplacementDatabase(info, testSystemID) {
			t.Fatal("non-authoritative database accepted")
		}
	}
	if !runningReplacementDatabase(&patroni.NodeInfo{DatabaseSystemIdentifier: testSystemID, Role: "replica", State: "running"}, testSystemID) {
		t.Fatal("healthy replica cannot authenticate its existing initialized database")
	}
}
