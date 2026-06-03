// Package etcdmgr wraps the etcdctl v3 binary to manage cluster membership.
// We shell out to etcdctl (ETCDCTL_API=3) rather than using the Go client
// library to keep the binary self-contained without a gRPC dependency.
package etcdmgr

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Member represents a single member entry parsed from `etcdctl member list`.
type Member struct {
	ID         string
	Name       string // empty for unstarted (ghost) members
	PeerURLs   string
	ClientURLs string
	IsLeader   bool // NOTE: not available in etcd v3.5 CSV member list output
	IsLearner  bool
	Unstarted  bool
}

// Client wraps etcdctl with SSL options and an endpoint.
type Client struct {
	Endpoint string
	SSLOpts  []string // e.g. ["--cert=...", "--key=...", "--cacert=..."]
}

// New creates a client for the given endpoint with optional SSL options.
func New(endpoint string, sslOpts []string) *Client {
	return &Client{Endpoint: endpoint, SSLOpts: sslOpts}
}

// run executes etcdctl (v3 API) with the given args and returns stdout (trimmed).
func (c *Client) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	full := append([]string{}, c.SSLOpts...)
	full = append(full, "--endpoints="+c.Endpoint, "--dial-timeout="+timeout.String())
	full = append(full, args...)
	cctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "etcdctl", full...)
	cmd.Env = append(os.Environ(), "ETCDCTL_API=3")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("etcdctl %v: %w (stderr=%s)", args, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// MemberList queries the cluster and returns parsed members.
func (c *Client) MemberList(ctx context.Context) ([]Member, error) {
	out, err := c.run(ctx, 5*time.Second, "member", "list")
	if err != nil {
		return nil, err
	}
	return ParseMemberList(out), nil
}

// MemberAdd registers a new peer as a full voting member.
func (c *Client) MemberAdd(ctx context.Context, name, peerURL string) error {
	_, err := c.run(ctx, 10*time.Second, "member", "add", name, "--peer-urls="+peerURL)
	return err
}

// MemberAddLearner registers a new peer as a non-voting learner.
// The learner replicates data without counting toward quorum until promoted.
func (c *Client) MemberAddLearner(ctx context.Context, name, peerURL string) error {
	_, err := c.run(ctx, 10*time.Second, "member", "add", name, "--peer-urls="+peerURL, "--learner")
	return err
}

// MemberPromote promotes a learner member to a full voting member.
// Returns an error if the learner is not yet in sync with the leader.
func (c *Client) MemberPromote(ctx context.Context, id string) error {
	_, err := c.run(ctx, 10*time.Second, "member", "promote", id)
	return err
}

// MemberRemove removes a member by ID.
func (c *Client) MemberRemove(ctx context.Context, id string) error {
	_, err := c.run(ctx, 10*time.Second, "member", "remove", id)
	return err
}

// SetWithTTL sets a key with a TTL via a lease. Used as a write-quorum probe.
func (c *Client) SetWithTTL(ctx context.Context, key, value string, ttlSecs int) error {
	out, err := c.run(ctx, 5*time.Second, "lease", "grant", fmt.Sprintf("%d", ttlSecs))
	if err != nil {
		return err
	}
	// Output: "lease 694d5765fc15afcd granted with TTL(1s)"
	parts := strings.Fields(out)
	if len(parts) < 2 {
		return fmt.Errorf("unexpected lease grant output: %s", out)
	}
	leaseID := parts[1]
	_, err = c.run(ctx, 5*time.Second, "put", "--lease="+leaseID, key, value)
	return err
}

// Set sets a key to a value without a TTL (permanent key).
func (c *Client) Set(ctx context.Context, key, value string) error {
	_, err := c.run(ctx, 5*time.Second, "put", key, value)
	return err
}

// Get retrieves a key's value. Returns ("", nil) if the key does not exist.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	out, err := c.run(ctx, 5*time.Second, "get", "--print-value-only", key)
	if err != nil {
		return "", err
	}
	return out, nil
}

// RM removes a single key.
func (c *Client) RM(ctx context.Context, key string) error {
	_, err := c.run(ctx, 5*time.Second, "del", key)
	return err
}

// RMRecursive removes all keys with the given prefix.
func (c *Client) RMRecursive(ctx context.Context, prefix string) error {
	_, err := c.run(ctx, 5*time.Second, "del", "--prefix", prefix)
	return err
}

// ParseMemberList parses etcdctl v3 member list output into Member structs.
// etcd v3.5 CSV format has 6 fields (no IsLeader column):
//
//	"<hex-id>, started,   <name>, <peerURLs>, <clientURLs>, <isLearner>"
//	"<hex-id>, unstarted,       , <peerURLs>,             ,       false"
func ParseMemberList(output string) []Member {
	var members []Member
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Split into at most 6 fields on ", "
		fields := strings.SplitN(line, ", ", 6)
		if len(fields) < 2 {
			continue
		}
		m := Member{}
		m.ID = strings.TrimSpace(fields[0])
		status := strings.TrimSpace(fields[1])
		m.Unstarted = status == "unstarted"
		if len(fields) > 2 {
			m.Name = strings.TrimSpace(fields[2])
		}
		if len(fields) > 3 {
			m.PeerURLs = strings.TrimSpace(fields[3])
		}
		if len(fields) > 4 {
			m.ClientURLs = strings.TrimSpace(fields[4])
			if m.ClientURLs == "" {
				m.Unstarted = true
			}
		}
		// Field 5 is IsLearner (etcd v3.5 dropped IsLeader from CSV output).
		if len(fields) > 5 {
			m.IsLearner = strings.EqualFold(strings.TrimSpace(fields[5]), "true")
		}
		members = append(members, m)
	}
	return members
}

// FindByPeerURL returns the first member whose PeerURLs match. Returns nil if not found.
func FindByPeerURL(members []Member, peerURL string) *Member {
	for i := range members {
		if members[i].PeerURLs == peerURL {
			return &members[i]
		}
	}
	return nil
}

// FindByName returns the first member with the given name.
func FindByName(members []Member, name string) *Member {
	for i := range members {
		if members[i].Name == name {
			return &members[i]
		}
	}
	return nil
}

// FindByClientIP returns the first member whose client URL contains the IP:port.
func FindByClientIP(members []Member, ipPort string) *Member {
	for i := range members {
		if strings.Contains(members[i].ClientURLs, ipPort) {
			return &members[i]
		}
	}
	return nil
}
