// Package patroni provides a thin client for the Patroni REST API used to
// determine cluster role (primary/replica) and member liveness.
package patroni

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	HTTP   *http.Client
	Scheme string
	Port   int
}

func New(useSSL bool, port int) *Client {
	tr := &http.Transport{
		// Patroni REST API uses self-signed certs from our internal CA; skipping
		// verify is acceptable for in-cluster polling because the proxy lives
		// on the same trusted host. Same approach as shell `curl -k`.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	scheme := "http"
	if useSSL {
		scheme = "https"
	}
	return &Client{
		HTTP:   &http.Client{Transport: tr, Timeout: 5 * time.Second},
		Scheme: scheme,
		Port:   port,
	}
}

// Role represents the Patroni member role.
type Role string

const (
	RolePrimary Role = "primary"
	RoleReplica Role = "replica"
	RoleUnknown Role = "unknown"
)

// CheckRole returns the role of the node at the given IP using the
// /primary and /replica endpoints (HTTP 200 = match).
func (c *Client) CheckRole(ctx context.Context, ip string) (Role, error) {
	if c.isHealthy(ctx, ip, "/primary") {
		return RolePrimary, nil
	}
	if c.isHealthy(ctx, ip, "/replica") {
		return RoleReplica, nil
	}
	return RoleUnknown, nil
}

func (c *Client) isHealthy(ctx context.Context, ip, path string) bool {
	url := fmt.Sprintf("%s://%s:%d%s", c.Scheme, ip, c.Port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// IsAlive checks if the Patroni REST API responds with any status on /health.
func (c *Client) IsAlive(ctx context.Context, ip string) bool {
	url := fmt.Sprintf("%s://%s:%d/health", c.Scheme, ip, c.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Patroni returns 200 for primary, 503 for unhealthy/role-mismatch
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusServiceUnavailable
}

// ClusterInfo is the subset of /cluster response we use.
type ClusterInfo struct {
	Members []ClusterMember `json:"members"`
}

type ClusterMember struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Host  string `json:"host"`
	Port  int    `json:"port"`
	State string `json:"state"`
}

// Cluster fetches /cluster from the given Patroni endpoint.
func (c *Client) Cluster(ctx context.Context, ip string) (*ClusterInfo, error) {
	url := fmt.Sprintf("%s://%s:%d/cluster", c.Scheme, ip, c.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("patroni /cluster: status %d", resp.StatusCode)
	}
	var info ClusterInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}
