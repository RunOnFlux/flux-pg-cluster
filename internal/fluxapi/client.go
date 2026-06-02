// Package fluxapi provides a client for the Flux apps location API used to
// discover the current cluster membership.
package fluxapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

type appLocationResp struct {
	Status string `json:"status"`
	Data   []struct {
		IP    string `json:"ip"`
		Name  string `json:"name"`
		Ports json.RawMessage `json:"ports"`
	} `json:"data"`
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// ListIPs queries the Flux API for the given app and returns the unique sorted
// list of node IPs (ports stripped). Returns nil (not error) when API returns
// empty data — matches shell behaviour where this gracefully falls back.
func (c *Client) ListIPs(ctx context.Context, appName string) ([]string, error) {
	url := fmt.Sprintf("%s/apps/location/%s", c.BaseURL, appName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("flux api: status %d body=%s", resp.StatusCode, string(body))
	}
	var parsed appLocationResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("flux api: decode: %w (body=%s)", err, string(body))
	}
	seen := map[string]bool{}
	out := []string{}
	for _, d := range parsed.Data {
		ip := d.IP
		// Strip trailing :port if present
		if idx := strings.LastIndex(ip, ":"); idx > 0 && !strings.Contains(ip[:idx], ":") {
			ip = ip[:idx]
		}
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, ip)
	}
	sort.Strings(out)
	return out, nil
}
