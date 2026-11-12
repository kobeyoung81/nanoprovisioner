// Package portalclient asks the portal the one question the terminal needs
// answered: does this agent belong to this user?
//
// The portal owns agent rows; the provisioner only knows containers. Rather
// than reading another module's collection (and having to keep its indexes in
// sync), the check is one internal HTTP call.
package portalclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is where the portal listens inside the compose network.
const DefaultBaseURL = "http://nanoportal:80"

// Client is the provisioner's door to the portal.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client. An empty base URL leaves it disabled, and every call
// then fails instead of dialing "http://".
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether a base URL was configured.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

// AgentOwnedBy reports whether agentID belongs to userID. A missing agent is
// "not owned" rather than an error: the terminal then answers 403.
func (c *Client) AgentOwnedBy(ctx context.Context, userID, agentID string) (bool, error) {
	if !c.Enabled() {
		return false, fmt.Errorf("portal is not configured")
	}
	endpoint := c.baseURL + "/internal/v1/agents?user_id=" + url.QueryEscape(strings.TrimSpace(userID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("ask portal for agents: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("portal answered %d for the agent list", resp.StatusCode)
	}
	var payload struct {
		Agents []struct {
			ID string `json:"id"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, fmt.Errorf("decode portal agents: %w", err)
	}
	for _, a := range payload.Agents {
		if a.ID == agentID {
			return true, nil
		}
	}
	return false, nil
}
