package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
	"github.com/RunOnFlux/flux-pg-cluster/internal/etcdmgr"
	"github.com/RunOnFlux/flux-pg-cluster/internal/patroni"
)

const replacementIdentityFile = "/var/lib/etcd/.flux-pg-identity.json"

var errUnknownIdentity = errors.New("no known PostgreSQL cluster identity")
var errConflictingReplacement = errors.New("conflicting replacement identity evidence")

// This proof is only added to nonce-signed join responses after a live local
// database and linearizable DCS reads agree. It never authorizes initdb.
type replacementProof struct {
	SystemID string `json:"system_id"`
	DCSID    string `json:"dcs_id"`
	Leader   string `json:"leader"`
}

func restoreReplacementIdentity(cfg *config.Config, path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var h clusterHistory
	if err := json.Unmarshal(b, &h); err != nil {
		return fmt.Errorf("replacement history: %w", err)
	}
	if h.Version != 1 || h.App != cfg.AppName || h.Scope != cfg.PatroniScope || !validSystemID(h.SystemID) {
		return errors.New("replacement history belongs to an invalid or different cluster")
	}
	if cfg.ExpectedSystemID != "" && cfg.ExpectedSystemID != h.SystemID {
		return errors.New("deployment identity conflicts with remembered replacement identity")
	}
	cfg.ExpectedSystemID = h.SystemID
	cfg.ClusterSeedIPs = strings.Join(unionIPs(splitCSV(cfg.ClusterSeedIPs), h.Members), ",")
	return nil
}

func saveReplacementIdentity(cfg *config.Config, dataDir, path, id string, peers []string) error {
	if !directoryIsEmpty(dataDir) || !validSystemID(id) {
		return errors.New("only a strictly empty replacement may learn a valid identity")
	}
	copyCfg := *cfg
	if err := restoreReplacementIdentity(&copyCfg, path); err != nil {
		return err
	}
	if copyCfg.ExpectedSystemID != "" && copyCfg.ExpectedSystemID != id {
		return errors.New("refusing to replace an existing cluster identity")
	}
	h := clusterHistory{Version: 1, App: cfg.AppName, Scope: cfg.PatroniScope, SystemID: id, Members: peers}
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(path, b); err != nil {
		return err
	}
	cfg.ExpectedSystemID = id
	return nil
}

// Get uses etcd's default linearizable consistency, so a member without quorum
// cannot supply the initialization/leader proof even if member list is cached.
func readReplacementDCS(ctx context.Context, cfg *config.Config, ec *etcdmgr.Client) (replacementProof, error) {
	var p replacementProof
	prefix := "/patroni/" + cfg.PatroniScope
	var err error
	if p.SystemID, err = ec.Get(ctx, prefix+"/initialize"); err != nil {
		return p, err
	}
	if p.Leader, err = ec.Get(ctx, prefix+"/leader"); err != nil {
		return p, err
	}
	if p.DCSID, err = ec.ClusterID(ctx); err != nil {
		return p, err
	}
	if !validSystemID(p.SystemID) || p.Leader == "" || p.DCSID == "" {
		return p, errors.New("DCS has no initialized cluster with a leader")
	}
	members, err := ec.MemberList(ctx)
	if err != nil {
		return p, err
	}
	leader := etcdmgr.FindByName(members, p.Leader)
	if leader == nil || leader.IsLearner || leader.Unstarted {
		return p, errors.New("DCS leader is not a registered voting member")
	}
	return p, nil
}

func localReplacementProof(ctx context.Context, cfg *config.Config, identity nodeIdentity) *replacementProof {
	if identity.IdentityError != "" || !validSystemID(identity.PostgresSystemIdentifier) {
		return nil
	}
	ec := etcdmgr.New(fmt.Sprintf("%s://127.0.0.1:%d", cfg.EtcdProtocol(), cfg.EtcdClientPort), etcdSSLOpts(cfg))
	p, err := readReplacementDCS(ctx, cfg, ec)
	if err != nil || p.SystemID != identity.PostgresSystemIdentifier {
		return nil
	}
	pc := patroni.New(cfg.SSLEnabled, cfg.PatroniAPIPort)
	info, err := pc.GetInfo(ctx, "127.0.0.1")
	if err != nil || !runningReplacementDatabase(info, p.SystemID) {
		return nil
	}
	return &p
}

func runningReplacementDatabase(info *patroni.NodeInfo, id string) bool {
	return info != nil && info.DatabaseSystemIdentifier == id &&
		(info.State == "running" || info.State == "streaming") &&
		(info.Role == "primary" || info.Role == "master" || info.Role == "replica")
}

// Older images do not sign identity responses. Only certificate-verified HTTPS
// may authenticate their live database; the ordinary curl-k polling client is
// deliberately not used as positive trust evidence here.
func legacyReplacementInfo(ctx context.Context, cfg *config.Config, ip string) (*patroni.NodeInfo, error) {
	return legacyReplacementInfoAt(ctx, cfg, ip, "/etc/ssl/cluster/ca/ca.crt")
}

func legacyReplacementInfoAt(ctx context.Context, cfg *config.Config, ip, caPath string) (*patroni.NodeInfo, error) {
	if !cfg.SSLEnabled {
		return nil, errors.New("legacy peer without TLS cannot authenticate a replacement identity")
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid cluster CA")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+net.JoinHostPort(ip, fmt.Sprint(cfg.HostPatroniAPIPort))+"/patroni", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("legacy Patroni status %d", resp.StatusCode)
	}
	var info patroni.NodeInfo
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info)
	return &info, err
}

func replacementEvidence(ctx context.Context, cfg *config.Config, ip string, ec *etcdmgr.Client) (replacementProof, error) {
	p, err := readReplacementDCS(ctx, cfg, ec)
	if err != nil {
		return p, err
	}
	identity, probeErr := probePeerIdentityWithJoin(ctx, cfg, ip, true)
	if probeErr == nil {
		if identity.AppName != cfg.AppName || identity.PatroniScope != cfg.PatroniScope ||
			identity.NodeName != nodeNameFromIP(ip) || identity.IdentityError != "" || identity.PGDataEmpty ||
			identity.PostgresSystemIdentifier != p.SystemID {
			return p, errConflictingReplacement
		}
		if identity.Join != nil {
			if *identity.Join != p {
				return p, errConflictingReplacement
			}
			return p, nil
		}
	}
	info, err := legacyReplacementInfo(ctx, cfg, ip)
	if err != nil {
		return p, err
	}
	if !runningReplacementDatabase(info, p.SystemID) {
		return p, errConflictingReplacement
	}
	return p, nil
}

func agreeReplacementEvidence(proofs []replacementProof) (replacementProof, error) {
	if len(proofs) == 0 {
		return replacementProof{}, errors.New("no authenticated healthy initialized peer")
	}
	first := proofs[0]
	if !validSystemID(first.SystemID) || first.DCSID == "" || first.Leader == "" {
		return replacementProof{}, errors.New("incomplete replacement proof")
	}
	for _, p := range proofs[1:] {
		if p != first {
			return replacementProof{}, errConflictingReplacement
		}
	}
	return first, nil
}

func learnReplacementIdentity(ctx context.Context, cfg *config.Config, seed *etcdmgr.Client) (string, error) {
	if cfg.PostgresReplicationPassword == "" || !directoryIsEmpty(postgresDataDir) {
		return "", errors.New("identity learning requires cluster credentials and empty PGDATA")
	}
	u, err := url.Parse(seed.Endpoint)
	if err != nil || net.ParseIP(u.Hostname()) == nil {
		return "", errors.New("invalid replacement seed")
	}
	seedIP := u.Hostname()
	ips := unionIPs(candidateIPs(cfg), []string{seedIP})
	probeOrder := []string{seedIP}
	for _, ip := range ips {
		if ip != seedIP {
			probeOrder = append(probeOrder, ip)
		}
	}
	var proofs []replacementProof
	seedVerified := false
	for _, ip := range probeOrder {
		if ip == cfg.MyIP {
			continue
		}
		ec := etcdmgr.New(fmt.Sprintf("%s://%s", cfg.EtcdProtocol(), net.JoinHostPort(ip, fmt.Sprint(cfg.HostEtcdClientPort))), etcdSSLOpts(cfg))
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		p, err := replacementEvidence(probeCtx, cfg, ip, ec)
		cancel()
		if errors.Is(err, errConflictingReplacement) {
			return "", err
		}
		if err != nil {
			continue
		}
		proofs = append(proofs, p)
		seedVerified = seedVerified || ip == seedIP
	}
	if !seedVerified {
		return "", errors.New("joining peer did not authenticate its initialized database")
	}
	p, err := agreeReplacementEvidence(proofs)
	if err != nil {
		return "", err
	}
	current, err := readReplacementDCS(ctx, cfg, seed)
	if err != nil || current != p {
		return "", errors.New("replacement authority changed during verification")
	}
	if err := saveReplacementIdentity(cfg, postgresDataDir, replacementIdentityFile, p.SystemID, ips); err != nil {
		return "", err
	}
	// Patroni starts as postgres and cannot traverse etcd's private directory.
	// Publish the learned value in the existing runtime config before joining.
	if err := cfg.WriteClusterEnv(); err != nil {
		return "", err
	}
	return p.SystemID, nil
}

func learnedSystemID(cfg *config.Config, dataDir, path string) (string, error) {
	copyCfg := *cfg
	if err := restoreReplacementIdentity(&copyCfg, filepath.Clean(path)); err != nil {
		return "", err
	}
	return expectedSystemIDAt(&copyCfg, dataDir)
}
