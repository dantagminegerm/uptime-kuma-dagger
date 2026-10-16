package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type check struct {
	name   string
	ok     bool
	detail string
}

// Validate the running platform: container health, Prometheus targets and key
// metrics, Uptime Kuma HTTP, Grafana data source and dashboard. Fails if any check fails.
// +cache="never"
func (m *UptimePlatform) Validate(ctx context.Context) (string, error) {
	if err := require(map[string]any{"prometheus": m.Prometheus, "uptime-kuma": m.UptimeKuma}); err != nil {
		return "", err
	}
	var checks []check
	add := func(name string, err error, okDetail string) {
		if err != nil {
			checks = append(checks, check{name, false, err.Error()})
		} else {
			checks = append(checks, check{name, true, okDetail})
		}
	}

	// 1. Containers of both layers are running and healthy.
	for _, layer := range []struct{ project, file string }{{appProject, appCompose}, {monitoringProject, monitoringCompose}} {
		services, err := m.containerStates(ctx, layer.project, layer.file)
		if err != nil {
			add("containers/"+layer.project, err, "")
			continue
		}
		for _, s := range services {
			add("container/"+s.name, s.err(), s.status)
		}
	}

	// 2. Prometheus: every scrape target up, and the key health signals are green.
	promURL, err := serviceURL(ctx, m.Prometheus)
	if err != nil {
		return "", fmt.Errorf("prometheus endpoint: %w", err)
	}
	prom := &prometheusClient{base: promURL, http: &http.Client{Timeout: 10 * time.Second}}
	var targetsDetail string
	err = retry(ctx, 3*time.Minute, 5*time.Second, func() (err error) {
		targetsDetail, err = prom.allTargetsUp(ctx)
		return err
	})
	add("prometheus/targets", err, targetsDetail)
	for _, q := range []struct{ name, expr string }{
		{"uptime-kuma/http-probe", `probe_success{service="uptime-kuma"}`},
		{"mariadb/mysql_up", `mysql_up`},
		{"mariadb/tcp-probe", `probe_success{service="mariadb"}`},
		{"grafana/http-probe", `probe_success{service="grafana"}`},
	} {
		var detail string
		err := retry(ctx, time.Minute, 5*time.Second, func() (err error) {
			detail, err = prom.expectOne(ctx, q.expr)
			return err
		})
		add(q.name, err, detail)
	}
	latency, err := prom.scalar(ctx, `probe_duration_seconds{service="uptime-kuma"}`)
	add("uptime-kuma/latency", err, fmt.Sprintf("%.0f ms", latency*1000))
	containers, err := prom.scalar(ctx, `count(container_last_seen{container_label_com_docker_compose_project=~"uptime-.*"})`)
	add("cadvisor/containers", err, fmt.Sprintf("%.0f platform containers reported", containers))

	// 3. Uptime Kuma answers directly through its published port.
	kumaURL, err := serviceURL(ctx, m.UptimeKuma)
	if err == nil {
		err = httpOK(ctx, kumaURL+"/")
	}
	add("uptime-kuma/http", err, "web UI reachable on :3001")

	// 4. Grafana: data source can query Prometheus, dashboard is provisioned.
	if g, err := m.grafanaClient(ctx); err != nil {
		add("grafana", err, "")
	} else {
		var health struct{ Status, Message string }
		_, err := g.do(ctx, http.MethodGet, "/api/datasources/uid/prometheus/health", nil, &health)
		if err == nil && health.Status != "OK" {
			err = fmt.Errorf("%s: %s", health.Status, health.Message)
		}
		add("grafana/datasource", err, "Prometheus data source healthy")
		var dash struct {
			Meta struct{ URL string } `json:"meta"`
		}
		_, err = g.do(ctx, http.MethodGet, "/api/dashboards/uid/platform-health", nil, &dash)
		add("grafana/dashboard", err, dash.Meta.URL)
	}

	return renderChecks(checks)
}

func renderChecks(checks []check) (string, error) {
	var b strings.Builder
	failed := 0
	for _, c := range checks {
		mark := "OK  "
		if !c.ok {
			mark = "FAIL"
			failed++
		}
		fmt.Fprintf(&b, "[%s] %-32s %s\n", mark, c.name, c.detail)
	}
	if failed > 0 {
		fmt.Fprintf(&b, "%d of %d checks failed\n", failed, len(checks))
		return b.String(), fmt.Errorf("%d of %d checks failed:\n%s", failed, len(checks), b.String())
	}
	fmt.Fprintf(&b, "all %d checks passed\n", len(checks))
	return b.String(), nil
}

type serviceState struct {
	name, state, status string
	present             bool
}

func (s serviceState) err() error {
	switch {
	case !s.present:
		return fmt.Errorf("no container (layer not deployed?)")
	case s.state != "running":
		return fmt.Errorf("state %s (%s)", s.state, s.status)
	case strings.Contains(s.status, "(unhealthy)"), strings.Contains(s.status, "(health: starting)"):
		return fmt.Errorf("%s", s.status)
	}
	return nil
}

// containerStates lists the services declared in a compose file together with
// the state of their containers. Interpolation is disabled, so no secrets are needed.
func (m *UptimePlatform) containerStates(ctx context.Context, project, file string) ([]serviceState, error) {
	out, err := m.docker().WithExec(sh(fmt.Sprintf(`set -eu
docker compose -f %s config --services --no-interpolate | sed 's/^/service\t/'
docker ps -a --filter label=com.docker.compose.project=%s --format '{{.Label "com.docker.compose.service"}}\t{{.State}}\t{{.Status}}'`,
		file, project))).Stdout(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*serviceState{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		switch {
		case len(f) == 2 && f[0] == "service":
			byName[f[1]] = &serviceState{name: f[1]}
		case len(f) == 3:
			s, ok := byName[f[0]]
			if !ok {
				s = &serviceState{name: f[0]}
				byName[f[0]] = s
			}
			s.present, s.state, s.status = true, f[1], f[2]
		}
	}
	states := make([]serviceState, 0, len(byName))
	for _, s := range byName {
		states = append(states, *s)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].name < states[j].name })
	return states, nil
}

type prometheusClient struct {
	base string
	http *http.Client
}

func (p *prometheusClient) allTargetsUp(ctx context.Context) (string, error) {
	var resp struct {
		Data struct {
			ActiveTargets []struct {
				ScrapePool string `json:"scrapePool"`
				ScrapeURL  string `json:"scrapeUrl"`
				Health     string `json:"health"`
				LastError  string `json:"lastError"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if _, err := doJSON(ctx, p.http, http.MethodGet, p.base+"/api/v1/targets?state=active", nil, &resp, nil); err != nil {
		return "", err
	}
	var down []string
	for _, t := range resp.Data.ActiveTargets {
		if t.Health != "up" {
			down = append(down, fmt.Sprintf("%s %s: %s %s", t.ScrapePool, t.ScrapeURL, t.Health, t.LastError))
		}
	}
	if len(resp.Data.ActiveTargets) == 0 {
		return "", fmt.Errorf("no active targets")
	}
	if len(down) > 0 {
		return "", fmt.Errorf("%d targets not up: %s", len(down), strings.Join(down, "; "))
	}
	return fmt.Sprintf("%d/%d targets up", len(resp.Data.ActiveTargets), len(resp.Data.ActiveTargets)), nil
}

func (p *prometheusClient) query(ctx context.Context, expr string) ([]float64, error) {
	var resp struct {
		Data struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	u := p.base + "/api/v1/query?query=" + url.QueryEscape(expr)
	if _, err := doJSON(ctx, p.http, http.MethodGet, u, nil, &resp, nil); err != nil {
		return nil, err
	}
	values := make([]float64, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		var v float64
		s, _ := r.Value[1].(string)
		if _, err := fmt.Sscan(s, &v); err != nil {
			return nil, fmt.Errorf("%s: bad sample %q", expr, s)
		}
		values = append(values, v)
	}
	return values, nil
}

func (p *prometheusClient) scalar(ctx context.Context, expr string) (float64, error) {
	values, err := p.query(ctx, expr)
	if err != nil {
		return 0, err
	}
	if len(values) == 0 {
		return 0, fmt.Errorf("%s: no data", expr)
	}
	return values[0], nil
}

// expectOne checks that every series returned by expr has the value 1.
func (p *prometheusClient) expectOne(ctx context.Context, expr string) (string, error) {
	values, err := p.query(ctx, expr)
	if err != nil {
		return "", err
	}
	if len(values) == 0 {
		return "", fmt.Errorf("%s: no data", expr)
	}
	for _, v := range values {
		if v != 1 {
			return "", fmt.Errorf("%s = %v", expr, v)
		}
	}
	return expr + " = 1", nil
}

func httpOK(ctx context.Context, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return nil
}
