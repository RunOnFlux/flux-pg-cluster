// Package etcdmgr wraps the v2 etcdctl binary to manage cluster membership.
// We shell out to etcdctl rather than using the Go client library because the
// existing scripts use v2 API output formatting (no name= for unstarted, etc.)
// and exact output parity simplifies side-by-side validation.
package etcdmgr

import (
	"bytes"
	"context"
	"fmt"
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
	IsLeader   bool
	Unstarted  bool
}

// Client wraps etcdctl with SSL options and an endpoint.
type Client struct {
	Endpoint string
	SSLOpts  []string // e.g. ["--cert-file=...", "--key-file=...", "--ca-file=..."]
}

// New creates a client for the given endpoint with optional SSL options.
func New(endpoint string, sslOpts []string) *Client {
	return &Client{Endpoint: endpoint, SSLOpts: sslOpts}
}

// run executes etcdctl with the given args and returns stdout (trimmed).
func (c *Client) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	full := append([]string{}, c.SSLOpts...)
	full = append(full, "--endpoints="+c.Endpoint, "--timeout="+timeout.String())
	full = append(full, args...)
	cctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "etcdctl", full...)
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

// MemberAdd registers a new peer with the cluster.
func (c *Client) MemberAdd(ctx context.Context, name, peerURL string) error {
	_, err := c.run(ctx, 10*time.Second, "member", "add", name, peerURL)
	return err
}

// MemberRemove removes a member by ID.
func (c *Client) MemberRemove(ctx context.Context, id string) error {
	_, err := c.run(ctx, 10*time.Second, "member", "remove", id)
	return err
}

// SetWithTTL sets a key with a TTL (seconds). Used as a write-quorum probe.
func (c *Client) SetWithTTL(ctx context.Context, key, value string, ttlSecs int) error {
	_, err := c.run(ctx, 5*time.Second, "set", key, value, "--ttl", fmt.Sprintf("%d", ttlSecs))
	return err
}

// Get retrieves a key's value.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	return c.run(ctx, 5*time.Second, "get", key)
}

// RM removes a key (single).
func (c *Client) RM(ctx context.Context, key string) error {
	_, err := c.run(ctx, 5*time.Second, "rm", key)
	return err
}

// RMRecursive removes a key recursively.
func (c *Client) RMRecursive(ctx context.Context, key string) error {
	_, err := c.run(ctx, 5*time.Second, "rm", "--recursive", key)
	return err
}

// ParseMemberList parses etcdctl v2 member list output into Member structs.
// Active line:    "7d0225fa214aabdb: name=node-1 peerURLs=https://1.2.3.4:2380 clientURLs=https://1.2.3.4:2379 isLeader=true"
// Unstarted line: "98583ded33e2a2c[unstarted]: peerURLs=https://1.2.3.5:2380 clientURLs="
func ParseMemberList(output string) []Member {
	var members []Member
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		idPart := strings.TrimSpace(line[:idx])
		rest := strings.TrimSpace(line[idx+1:])

		m := Member{}
		if strings.HasSuffix(idPart, "[unstarted]") {
			m.Unstarted = true
			idPart = strings.TrimSuffix(idPart, "[unstarted]")
		}
		m.ID = idPart

		for _, tok := range strings.Fields(rest) {
			if k, v, ok := strings.Cut(tok, "="); ok {
				switch k {
				case "name":
					m.Name = v
				case "peerURLs":
					m.PeerURLs = v
				case "clientURLs":
					m.ClientURLs = v
				case "isLeader":
					m.IsLeader = strings.EqualFold(v, "true")
				}
			}
		}

		// Treat empty clientURLs as ghost regardless of [unstarted] marker
		if m.ClientURLs == "" {
			m.Unstarted = true
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
