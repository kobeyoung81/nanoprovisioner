// Package k8s provides a Provisioner backend that manages one Deployment per
// user on a Kubernetes cluster. The runner runs as a Pod and talks to the
// kube-apiserver using its in-cluster service account token.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/config"
	"gitee.com/kobeyoung81/nanorunner/internal/runner"
)

const (
	saTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	defaultNS       = "default"
)

var invalidNameChars = regexp.MustCompile(`[^a-z0-9-]`)

// Backend provisions per-user nanosec agent Deployments on Kubernetes.
type Backend struct {
	client    *http.Client
	baseURL   string
	token     string
	namespace string
}

// New creates a k8s backend using the in-cluster service account.
func New() (*Backend, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST/PORT not set (runner must run in-cluster)")
	}
	baseURL := "https://" + host + ":" + port

	token, err := os.ReadFile(saTokenPath)
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca, err := os.ReadFile(saCAPath); err == nil {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(ca) {
			tlsConfig.RootCAs = pool
		}
	} else {
		slog.Warn("no service account CA found, falling back to system roots", "error", err)
	}

	ns, err := os.ReadFile(saNamespacePath)
	if err != nil {
		ns = []byte(defaultNS)
	}

	return &Backend{
		client:    &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}},
		baseURL:   baseURL,
		token:     strings.TrimSpace(string(token)),
		namespace: strings.TrimSpace(string(ns)),
	}, nil
}

// Provision creates a single-replica Deployment for the agent container.
func (b *Backend) Provision(ctx context.Context, req nanoprotocol.ContainerRequest, opts runner.ProvisionOptions) error {
	name := DeploymentName(req)
	labels := map[string]string{"app": "nanosec-agent", "nanosec.io/user": SanitizeName(req.UserID)}

	args := []string{"--user", req.UserID, "--bus-url", opts.BusURL}
	if req.BindingID > 0 {
		args = append(args, "--binding", fmt.Sprintf("%d", req.BindingID))
	}

	// Attach a persistent data volume unless storage explicitly uses a bind.
	username := runner.UserName(req)
	containerSpec := map[string]any{
		"name":  "nanosec",
		"image": opts.Image,
		"args":  args,
		"env":   agentEnv(username, opts.BusToken, opts.Timezone),
		"resources": map[string]any{
			"limits": map[string]any{
				"memory": fmt.Sprintf("%dMi", opts.Quota.MemoryMB),
				"cpu":    fmt.Sprintf("%d", opts.Quota.CPUs/1e9),
			},
		},
	}
	podSpec := map[string]any{
		"containers": []map[string]any{containerSpec},
		"hostname":   runner.AgentHostname,
	}
	if opts.Storage.Backend != "bind" {
		pvcName, err := b.ensurePVC(ctx, opts.Storage, req)
		if err != nil {
			return err
		}
		mountPath := opts.Storage.DataMountPath(username)
		containerSpec["volumeMounts"] = []map[string]any{
			{"name": "data", "mountPath": mountPath},
		}
		podSpec["volumes"] = []map[string]any{
			{
				"name":                  "data",
				"persistentVolumeClaim": map[string]any{"claimName": pvcName},
			},
		}
	}

	deployment := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":   name,
			"labels": labels,
		},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     podSpec,
			},
		},
	}

	// Replace the deployment if it already exists so a re-provision picks up
	// the latest volume configuration, then mark it ready only on creation.
	existingURL := b.baseURL + "/apis/apps/v1/namespaces/" + b.namespace + "/deployments/" + name
	if err := b.requestOK(ctx, http.MethodGet, existingURL); err == nil {
		// Exists: update it (request body marshaled below) instead of failing.
	} else {
		body, err := json.Marshal(deployment)
		if err != nil {
			return fmt.Errorf("marshal deployment: %w", err)
		}
		url := b.baseURL + "/apis/apps/v1/namespaces/" + b.namespace + "/deployments"
		if err := b.request(ctx, http.MethodPost, url, body, http.StatusCreated); err != nil {
			return err
		}
		slog.Info("deployment provisioned", "user_id", req.UserID, "binding_id", req.BindingID, "deployment", name)
		return nil
	}

	body, err := json.Marshal(deployment)
	if err != nil {
		return fmt.Errorf("marshal deployment: %w", err)
	}
	if err := b.request(ctx, http.MethodPut, existingURL, body, http.StatusOK); err != nil {
		return err
	}
	slog.Info("deployment updated", "user_id", req.UserID, "binding_id", req.BindingID, "deployment", name)
	return nil
}

// agentEnv is the container environment: the agent identity plus the shared bus
// token, without which the agent cannot reach the bus at all.
func agentEnv(username, busToken, timezone string) []map[string]any {
	env := []map[string]any{{"name": "AGENT_USERNAME", "value": username}}
	if busToken != "" {
		env = append(env, map[string]any{"name": "NANOSEC_BUS_INTERNAL_TOKEN", "value": busToken})
	}
	if timezone != "" {
		env = append(env, map[string]any{"name": "TZ", "value": timezone})
	}
	return env
}

// ensurePVC creates a per-user PersistentVolumeClaim (if none exists) so agent
// data survives pod/Deployment recreation. Deprovision keeps the PVC so a
// restarted agent reuses the same volume.
func (b *Backend) ensurePVC(ctx context.Context, st config.StorageConfig, req nanoprotocol.ContainerRequest) (string, error) {
	rawName := runner.DataVolumeName(req)
	pvcName := SanitizeName(rawName)

	url := b.baseURL + "/api/v1/namespaces/" + b.namespace + "/persistentvolumeclaims"
	labels := map[string]string{"app": "nanosec-agent"}
	claim := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": pvcName, "labels": labels},
		"spec": map[string]any{
			"accessModes": []string{"ReadWriteOnce"},
			"resources": map[string]any{
				"requests": map[string]any{"storage": "1Gi"},
			},
		},
	}
	if st.StorageClass != "" {
		claim["spec"].(map[string]any)["storageClassName"] = st.StorageClass
	}

	body, err := json.Marshal(claim)
	if err != nil {
		return "", fmt.Errorf("marshal pvc: %w", err)
	}

	resp, err := b.postIfAbsent(ctx, url, body)
	if err != nil {
		return "", fmt.Errorf("ensure pvc %s: %w", pvcName, err)
	}
	if resp.created {
		slog.Info("created pvc", "user_id", req.UserID, "pvc", pvcName)
	} else {
		slog.Info("pvc already exists", "user_id", req.UserID, "pvc", pvcName)
	}
	return pvcName, nil
}

// Deprovision deletes the agent Deployment for a request.
func (b *Backend) Deprovision(ctx context.Context, req nanoprotocol.ContainerRequest, _ runner.ProvisionOptions) error {
	name := DeploymentName(req)
	url := b.baseURL + "/apis/apps/v1/namespaces/" + b.namespace + "/deployments/" + name
	if err := b.request(ctx, http.MethodDelete, url, nil, http.StatusOK); err != nil {
		return err
	}
	slog.Info("deployment disposed", "user_id", req.UserID, "binding_id", req.BindingID, "deployment", name)
	return nil
}

// DeploymentName returns the deterministic Deployment name for a request.
func DeploymentName(req nanoprotocol.ContainerRequest) string {
	if req.BindingID > 0 {
		return SanitizeName(fmt.Sprintf("nanosec-agent-%s-%d", req.UserID, req.BindingID))
	}
	return SanitizeName(fmt.Sprintf("nanosec-agent-%s", req.UserID))
}

// SanitizeName lowercases and keeps only DNS-1123 subdomain-safe characters.
func SanitizeName(s string) string {
	s = strings.ToLower(s)
	s = invalidNameChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "nanosec-agent"
	}
	if len(s) > 63 {
		return strings.TrimRight(s[:63], "-")
	}
	return s
}

func (b *Backend) request(ctx context.Context, method, url string, body []byte, wantStatus int) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("k8s api %s %s: %w", method, filepath.Base(url), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("k8s api %s %s: status %d: %s", method, filepath.Base(url), resp.StatusCode, string(detail))
	}
	return nil
}

// requestOK returns nil when the k8s API request succeeds (any 2xx), used to
// probe for existing resources.
func (b *Backend) requestOK(ctx context.Context, method, url string) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// postIfAbsent POSTs a create request, tolerating a 409 Conflict for an
// already-existing resource. It reports whether the resource was created.
func (b *Backend) postIfAbsent(ctx context.Context, url string, body []byte) (struct{ created bool }, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return struct{ created bool }{}, err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return struct{ created bool }{}, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated:
		return struct{ created bool }{created: true}, nil
	case http.StatusConflict:
		return struct{ created bool }{created: false}, nil
	default:
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return struct{ created bool }{}, fmt.Errorf("status %d: %s", resp.StatusCode, string(detail))
	}
}
