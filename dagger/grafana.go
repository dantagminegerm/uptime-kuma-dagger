package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"dagger/uptime-platform/internal/dagger"
)

const dashboardFolderUID = "platform"

type grafanaClient struct {
	base string
	user string
	pass string
	http *http.Client
}

func (m *UptimePlatform) grafanaClient(ctx context.Context) (*grafanaClient, error) {
	if err := require(map[string]any{"grafana": m.Grafana, "grafana-password": m.GrafanaPassword}); err != nil {
		return nil, err
	}
	base, err := serviceURL(ctx, m.Grafana)
	if err != nil {
		return nil, fmt.Errorf("grafana endpoint: %w", err)
	}
	pass, err := m.GrafanaPassword.Plaintext(ctx)
	if err != nil {
		return nil, err
	}
	return &grafanaClient{base: base, user: m.GrafanaUser, pass: pass, http: &http.Client{Timeout: 15 * time.Second}}, nil
}

// serviceURL starts the (host-tunnelled) service and returns its base URL.
func serviceURL(ctx context.Context, svc *dagger.Service) (string, error) {
	if _, err := svc.Start(ctx); err != nil {
		return "", err
	}
	return svc.Endpoint(ctx, dagger.ServiceEndpointOpts{Scheme: "http"})
}

// provision upserts every data source and dashboard found under dir.
func (g *grafanaClient) provision(ctx context.Context, dir *dagger.Directory) (string, error) {
	var report strings.Builder
	if err := g.waitHealthy(ctx, 2*time.Minute); err != nil {
		return "", err
	}

	datasources, err := readJSONFiles(ctx, dir, "datasources")
	if err != nil {
		return "", err
	}
	for name, raw := range datasources {
		uid, err := g.upsertDatasource(ctx, raw)
		if err != nil {
			return report.String(), fmt.Errorf("datasource %s: %w", name, err)
		}
		fmt.Fprintf(&report, "datasource %-12s uid=%s ... ok\n", name, uid)
	}

	if err := g.ensureFolder(ctx, dashboardFolderUID, "Platform"); err != nil {
		return report.String(), err
	}
	dashboards, err := readJSONFiles(ctx, dir, "dashboards")
	if err != nil {
		return report.String(), err
	}
	for name, raw := range dashboards {
		url, err := g.upsertDashboard(ctx, raw)
		if err != nil {
			return report.String(), fmt.Errorf("dashboard %s: %w", name, err)
		}
		fmt.Fprintf(&report, "dashboard  %-12s %s ... ok\n", name, url)
	}
	return report.String(), nil
}

func (g *grafanaClient) waitHealthy(ctx context.Context, timeout time.Duration) error {
	return retry(ctx, timeout, 3*time.Second, func() error {
		var health struct {
			Database string `json:"database"`
		}
		if _, err := g.do(ctx, http.MethodGet, "/api/health", nil, &health); err != nil {
			return err
		}
		if health.Database != "ok" {
			return fmt.Errorf("grafana database: %q", health.Database)
		}
		return nil
	})
}

func (g *grafanaClient) upsertDatasource(ctx context.Context, raw json.RawMessage) (string, error) {
	var ds struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(raw, &ds); err != nil || ds.UID == "" {
		return "", fmt.Errorf("data source definition must have a uid")
	}
	status, err := g.do(ctx, http.MethodGet, "/api/datasources/uid/"+ds.UID, nil, nil)
	switch {
	case status == http.StatusNotFound:
		_, err = g.do(ctx, http.MethodPost, "/api/datasources", raw, nil)
	case err == nil:
		_, err = g.do(ctx, http.MethodPut, "/api/datasources/uid/"+ds.UID, raw, nil)
	}
	if err != nil {
		return "", err
	}
	// Ask Grafana to actually query the backend, not just store the config.
	var health struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if _, err := g.do(ctx, http.MethodGet, "/api/datasources/uid/"+ds.UID+"/health", nil, &health); err != nil {
		return "", fmt.Errorf("health check: %w", err)
	}
	if health.Status != "OK" {
		return "", fmt.Errorf("health check: %s %s", health.Status, health.Message)
	}
	return ds.UID, nil
}

func (g *grafanaClient) ensureFolder(ctx context.Context, uid, title string) error {
	status, err := g.do(ctx, http.MethodGet, "/api/folders/"+uid, nil, nil)
	if status == http.StatusNotFound {
		body, _ := json.Marshal(map[string]string{"uid": uid, "title": title})
		_, err = g.do(ctx, http.MethodPost, "/api/folders", body, nil)
	}
	if err != nil {
		return fmt.Errorf("folder %s: %w", uid, err)
	}
	return nil
}

func (g *grafanaClient) upsertDashboard(ctx context.Context, raw json.RawMessage) (string, error) {
	var dashboard map[string]any
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		return "", err
	}
	if uid, _ := dashboard["uid"].(string); uid == "" {
		return "", fmt.Errorf("dashboard must have a uid")
	}
	delete(dashboard, "id") // let Grafana match by uid
	body, _ := json.Marshal(map[string]any{
		"dashboard": dashboard,
		"folderUid": dashboardFolderUID,
		"overwrite": true,
		"message":   "provisioned by dagger configure-grafana",
	})
	var resp struct {
		URL     string `json:"url"`
		Version int    `json:"version"`
	}
	if _, err := g.do(ctx, http.MethodPost, "/api/dashboards/db", body, &resp); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (v%d)", resp.URL, resp.Version), nil
}

// do performs an authenticated JSON request and returns the HTTP status.
// Any non-2xx status is returned as an error together with the status code.
func (g *grafanaClient) do(ctx context.Context, method, p string, body []byte, out any) (int, error) {
	return doJSON(ctx, g.http, method, g.base+p, body, out, func(r *http.Request) {
		r.SetBasicAuth(g.user, g.pass)
	})
}

func doJSON(ctx context.Context, c *http.Client, method, url string, body []byte, out any, mod func(*http.Request)) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if mod != nil {
		mod(req)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d: %s", method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode: %w", method, req.URL.Path, err)
		}
	}
	return resp.StatusCode, nil
}

// readJSONFiles returns the *.json files in dir/sub keyed by base name (without extension).
func readJSONFiles(ctx context.Context, dir *dagger.Directory, sub string) (map[string]json.RawMessage, error) {
	d := dir.Directory(sub)
	entries, err := d.Glob(ctx, "*.json")
	if err != nil {
		return nil, fmt.Errorf("list grafana/%s: %w", sub, err)
	}
	files := make(map[string]json.RawMessage, len(entries))
	for _, e := range entries {
		content, err := d.File(e).Contents(ctx)
		if err != nil {
			return nil, err
		}
		if !json.Valid([]byte(content)) {
			return nil, fmt.Errorf("grafana/%s/%s: invalid JSON", sub, e)
		}
		files[strings.TrimSuffix(path.Base(e), ".json")] = json.RawMessage(content)
	}
	return files, nil
}

// retry calls fn until it succeeds or timeout elapses, returning the last error.
func retry(ctx context.Context, timeout, interval time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
