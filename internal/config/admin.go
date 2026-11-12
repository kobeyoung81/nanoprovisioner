package config

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// DefaultAdminURL is where the admin module listens inside the compose network.
const DefaultAdminURL = "http://nanoadmin:8080"

// DefaultPortalURL is where the portal listens inside the compose network.
const DefaultPortalURL = "http://nanoportal:80"

// PortalURL returns the portal's base URL. The terminal checks agent ownership
// against the module that owns the rows.
func PortalURL() string {
	if v := strings.TrimSpace(os.Getenv("NANOSEC_PORTAL_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultPortalURL
}

// AdminURL returns the admin module's base URL. Configuration belongs to that
// module; the provisioner asks for it over HTTP like every other service.
func AdminURL() string {
	if v := strings.TrimSpace(os.Getenv("NANOSEC_ADMIN_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultAdminURL
}

// FetchItems returns one component's configuration.
//
// A non-200 is an error rather than an empty list, so a broken admin is never
// mistaken for "no settings" — the runner would otherwise quietly provision
// every container with defaults.
func FetchItems(component string) ([]nanoprotocol.ConfigItem, error) {
	return fetchItemsFrom(AdminURL(), component, 5*time.Second)
}

func fetchItemsFrom(baseURL, component string, timeout time.Duration) ([]nanoprotocol.ConfigItem, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/config/"+url.PathEscape(component), nil)
	if err != nil {
		return nil, err
	}
	if token := InternalToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ask admin for %s config: %w", component, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin answered %d for %s config", resp.StatusCode, component)
	}
	var payload struct {
		Items []nanoprotocol.ConfigItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode %s config: %w", component, err)
	}
	return payload.Items, nil
}
