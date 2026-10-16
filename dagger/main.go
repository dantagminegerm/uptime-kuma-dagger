// Local monitoring platform for Uptime Kuma, driven entirely by Dagger.
//
// The module controls the host Docker daemon through the proxied
// /var/run/docker.sock and manages two independent docker-compose layers:
//
//   - app/        — Uptime Kuma + MariaDB           (deploy-app)
//   - monitoring/ — Prometheus, Grafana, exporters  (deploy-monitoring)
//
// Grafana data sources and dashboards are pushed through the Grafana HTTP API
// (configure-grafana). deploy-all runs everything end to end and validates it.
//
// Defaults for every constructor argument live in the repository's .env file,
// so `dagger call deploy-all` works without flags.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"dagger/uptime-platform/internal/dagger"
)

const (
	dockerCLIImage       = "docker:29.9-cli"
	observabilityNetwork = "platform-observability"

	appProject        = "uptime-app"
	appCompose        = "app/docker-compose.yml"
	monitoringProject = "uptime-monitoring"
	monitoringCompose = "monitoring/docker-compose.yml"
)

type UptimePlatform struct {
	// +private
	Sock *dagger.Socket
	// +private
	Source *dagger.Directory
	// +private
	Grafana *dagger.Service
	// +private
	Prometheus *dagger.Service
	// +private
	UptimeKuma *dagger.Service
	// +private
	GrafanaUser string
	// +private
	DBRootPassword *dagger.Secret
	// +private
	DBPassword *dagger.Secret
	// +private
	DBExporterPassword *dagger.Secret
	// +private
	GrafanaPassword *dagger.Secret
}

func New(
	// Host Docker daemon socket (e.g. /var/run/docker.sock).
	sock *dagger.Socket,
	// Repository files needed on the Docker side: compose stacks and Grafana assets.
	// +defaultPath="/"
	// +ignore=["*", "!app/**", "!monitoring/**", "!grafana/**"]
	source *dagger.Directory,
	// Grafana on the host, e.g. tcp://localhost:3000. Needed by configure-grafana and validate.
	// +optional
	grafana *dagger.Service,
	// Prometheus on the host, e.g. tcp://localhost:9090. Needed by validate.
	// +optional
	prometheus *dagger.Service,
	// Uptime Kuma on the host, e.g. tcp://localhost:3001. Needed by validate.
	// +optional
	uptimeKuma *dagger.Service,
	// Grafana admin user.
	// +default="admin"
	grafanaUser string,
	// MariaDB root password.
	// +optional
	dbRootPassword *dagger.Secret,
	// Password of the MariaDB user Uptime Kuma connects with.
	// +optional
	dbPassword *dagger.Secret,
	// Password of the read-only MariaDB account used by mysqld-exporter.
	// +optional
	dbExporterPassword *dagger.Secret,
	// Grafana admin password.
	// +optional
	grafanaPassword *dagger.Secret,
) *UptimePlatform {
	return &UptimePlatform{
		Sock:               sock,
		Source:             source,
		Grafana:            grafana,
		Prometheus:         prometheus,
		UptimeKuma:         uptimeKuma,
		GrafanaUser:        grafanaUser,
		DBRootPassword:     dbRootPassword,
		DBPassword:         dbPassword,
		DBExporterPassword: dbExporterPassword,
		GrafanaPassword:    grafanaPassword,
	}
}

// Deploy MariaDB and Uptime Kuma (layer 1) and wire them together.
// +cache="never"
func (m *UptimePlatform) DeployApp(ctx context.Context) (string, error) {
	if err := require(map[string]any{
		"db-root-password":     m.DBRootPassword,
		"db-password":          m.DBPassword,
		"db-exporter-password": m.DBExporterPassword,
	}); err != nil {
		return "", err
	}
	exporterPassword, err := sqlStringSecret(ctx, "mariadb-exporter-password-sql", m.DBExporterPassword)
	if err != nil {
		return "", err
	}

	return m.docker().
		WithSecretVariable("MARIADB_ROOT_PASSWORD", m.DBRootPassword).
		WithSecretVariable("MARIADB_PASSWORD", m.DBPassword).
		WithSecretVariable("EXPORTER_PASSWORD_SQL", exporterPassword).
		WithExec(sh(ensureNetworkScript)).
		WithExec(sh(composeUp(appCompose, false, 600))).
		// Idempotently (re)create the least-privilege account for mysqld-exporter,
		// so password rotation and pre-existing volumes are handled too.
		WithExec(sh(syncExporterUserScript)).
		WithExec(sh(composePs(appProject))).
		Stdout(ctx)
}

// Deploy Prometheus, Grafana and the exporters (layer 2).
// +cache="never"
func (m *UptimePlatform) DeployMonitoring(ctx context.Context) (string, error) {
	if err := require(map[string]any{
		"grafana-password":     m.GrafanaPassword,
		"db-exporter-password": m.DBExporterPassword,
	}); err != nil {
		return "", err
	}
	return m.docker().
		WithEnvVariable("GRAFANA_ADMIN_USER", m.GrafanaUser).
		WithSecretVariable("GRAFANA_ADMIN_PASSWORD", m.GrafanaPassword).
		WithSecretVariable("MARIADB_EXPORTER_PASSWORD", m.DBExporterPassword).
		WithExec(sh(ensureNetworkScript)).
		WithExec(sh(composeUp(monitoringCompose, true, 300))).
		WithExec(sh(composePs(monitoringProject))).
		Stdout(ctx)
}

// Push data sources and dashboards from grafana/ through the Grafana HTTP API.
// +cache="never"
func (m *UptimePlatform) ConfigureGrafana(ctx context.Context) (string, error) {
	g, err := m.grafanaClient(ctx)
	if err != nil {
		return "", err
	}
	return g.provision(ctx, m.Source.Directory("grafana"))
}

// Deploy the whole platform end to end, configure Grafana and validate every system.
// +cache="never"
func (m *UptimePlatform) DeployAll(ctx context.Context) (string, error) {
	var report strings.Builder
	steps := []struct {
		name string
		run  func(context.Context) (string, error)
	}{
		{"deploy-app", m.DeployApp},
		{"deploy-monitoring", m.DeployMonitoring},
		{"configure-grafana", m.ConfigureGrafana},
		{"validate", m.Validate},
	}
	for _, step := range steps {
		start := time.Now()
		out, err := step.run(ctx)
		fmt.Fprintf(&report, "==> %s (%s)\n%s\n", step.name, time.Since(start).Round(time.Second), strings.TrimRight(out, "\n"))
		if err != nil {
			return report.String(), fmt.Errorf("%s failed: %w", step.name, err)
		}
	}
	report.WriteString(endpointsSummary)
	return report.String(), nil
}

// Tear down both layers. Data volumes are kept unless --volumes is set.
// +cache="never"
func (m *UptimePlatform) Destroy(
	ctx context.Context,
	// Also delete the named volumes (MariaDB, Uptime Kuma, Prometheus, Grafana data).
	// +optional
	volumes bool,
) (string, error) {
	flags := "--remove-orphans"
	if volumes {
		flags += " --volumes"
	}
	script := fmt.Sprintf(`set -eu
docker compose -p %[1]s down %[3]s
docker compose -p %[2]s down %[3]s
docker network rm %[4]s 2>/dev/null || true
echo "platform removed"`, monitoringProject, appProject, flags, observabilityNetwork)
	return m.docker().WithExec(sh(script)).Stdout(ctx)
}

// docker returns a container with the Docker CLI (+compose) wired to the host daemon
// and the repository's compose stacks mounted in the working directory.
func (m *UptimePlatform) docker() *dagger.Container {
	return dag.Container().
		From(dockerCLIImage).
		WithUnixSocket("/var/run/docker.sock", m.Sock).
		WithMountedDirectory("/workspace", m.Source).
		WithWorkdir("/workspace").
		// Every call acts on live host state, so never reuse a cached exec.
		WithEnvVariable("CACHE_BUSTER", time.Now().Format(time.RFC3339Nano))
}

const ensureNetworkScript = `set -eu
if ! docker network inspect ` + observabilityNetwork + ` >/dev/null 2>&1; then
  docker network create --internal --label com.uptime-platform.managed=true ` + observabilityNetwork + `
fi`

const syncExporterUserScript = `set -eu
cat <<SQL | docker compose -f ` + appCompose + ` exec -T mariadb sh -c 'MYSQL_PWD="$(cat /run/secrets/mariadb_root_password)" exec mariadb -uroot'
CREATE USER IF NOT EXISTS 'exporter'@'%' IDENTIFIED BY '${EXPORTER_PASSWORD_SQL}' WITH MAX_USER_CONNECTIONS 3;
ALTER USER 'exporter'@'%' IDENTIFIED BY '${EXPORTER_PASSWORD_SQL}' WITH MAX_USER_CONNECTIONS 3;
GRANT PROCESS, REPLICATION CLIENT, SLAVE MONITOR ON *.* TO 'exporter'@'%';
GRANT SELECT ON performance_schema.* TO 'exporter'@'%';
SQL
echo "mariadb: exporter account synchronised"`

const endpointsSummary = `
Endpoints (bound to 127.0.0.1 only):
  Uptime Kuma  http://localhost:3001
  Grafana      http://localhost:3000   (dashboard: Platform / Platform Health)
  Prometheus   http://localhost:9090
Grafana admin password: .secrets/grafana_admin_password
`

func composeUp(file string, build bool, waitTimeout int) string {
	flags := ""
	if build {
		flags = " --build"
	}
	return fmt.Sprintf("set -eu\ndocker compose -f %s up -d%s --wait --wait-timeout %d --remove-orphans --quiet-pull",
		file, flags, waitTimeout)
}

func composePs(project string) string {
	return fmt.Sprintf(`docker ps -a --filter label=com.docker.compose.project=%s --format 'table {{.Label "com.docker.compose.service"}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}'`, project)
}

func sh(script string) []string {
	return []string{"sh", "-c", script}
}

// require fails with an actionable message when constructor arguments are missing.
func require(args map[string]any) error {
	var missing []string
	for name, v := range args {
		switch x := v.(type) {
		case *dagger.Secret:
			if x == nil {
				missing = append(missing, "--"+name)
			}
		case *dagger.Service:
			if x == nil {
				missing = append(missing, "--"+name)
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing module arguments %s (set them in .env or pass them before the function name)",
		strings.Join(missing, ", "))
}

// sqlStringSecret returns the secret escaped for use inside a single-quoted SQL literal.
func sqlStringSecret(ctx context.Context, name string, s *dagger.Secret) (*dagger.Secret, error) {
	plain, err := s.Plaintext(ctx)
	if err != nil {
		return nil, err
	}
	if plain == "" {
		return nil, errors.New(name + ": secret is empty")
	}
	escaped := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(plain)
	return dag.SetSecret(name, escaped), nil
}
