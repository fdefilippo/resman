// Package preflight implements inspection without constructing daemon components.
package preflight

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/tlsconfig"
)

// Status distinguishes observed facts, detected failures and unproved capabilities.
type Status string

const (
	Verified   Status = "verified"
	Invalid    Status = "invalid"
	Unverified Status = "unverified"
)

// Finding contains an inspection result without configuration values or secrets.
type Finding struct {
	Name   string
	Status Status
	Detail string
}

// Report records what the read-only check could and could not establish.
type Report struct {
	Findings        []Finding
	RestartRequired []string
}

// Valid reports whether any performed check detected an invalid prerequisite.
// Unverified operational capabilities do not masquerade as tested successes.
func (r Report) Valid() bool {
	for _, f := range r.Findings {
		if f.Status == Invalid {
			return false
		}
	}
	return true
}

// Print renders bounded diagnostics and key names, never a configuration dump.
func (r Report) Print(w io.Writer) error {
	for _, f := range r.Findings {
		if _, err := fmt.Fprintf(w, "%s %s: %s\n", f.Status, f.Name, f.Detail); err != nil {
			return fmt.Errorf("write check finding: %w", err)
		}
	}
	if len(r.RestartRequired) > 0 {
		if _, err := fmt.Fprintf(w, "Restart-required fields (classification only; no running-instance comparison): %s\n", strings.Join(r.RestartRequired, ", ")); err != nil {
			return fmt.Errorf("write lifecycle inventory: %w", err)
		}
	}
	return nil
}

// Check loads the required source and inspects prerequisites as the calling UID.
// It never initializes logging, leases, databases, PSI/BPF, or an HTTP listener.
func Check(ctx context.Context, path string) Report {
	r := Report{}
	add := func(name string, err error, success string) {
		f := Finding{Name: name, Status: Verified, Detail: success}
		if err != nil {
			f.Status = Invalid
			f.Detail = err.Error()
		}
		r.Findings = append(r.Findings, f)
	}
	unknown := func(name, detail string) {
		r.Findings = append(r.Findings, Finding{Name: name, Status: Unverified, Detail: detail})
	}
	cfg, err := config.LoadAndValidateFile(path)
	add("configuration", err, "file, defaults and environment overrides passed startup validation")
	if err != nil {
		return r
	}
	r.RestartRequired = config.RestartRequiredFields()
	policy, err := config.LoadCPUPointsPolicy(cfg)
	add("cpu_points_policy", err, "map syntax, file safety, exact NSS identities and capacity invariant validated")
	if err == nil {
		_, err = cpupoints.NewLiveCapacityProvider(cpupoints.NewSysfsOnlineCPUSource(), policy.Pool())
		add("online_cpu_capacity", err, "authoritative sysfs online CPU denominator is readable and valid")
	}
	_, err = config.LoadIOWeightPolicy(cfg)
	add("io_weight_policy", err, "enabled map and weight policy validated")
	_, err = os.ReadFile(filepath.Join(cfg.CgroupRoot, "cgroup.controllers"))
	add("cgroup_v2", err, "cgroup.controllers is readable; controller activation is not tested")
	host := cgroup.DetectEnforcementStatus("/run/systemd/system")
	if cfg.GetEnforcementMode() == config.EnforcementPolicySystemdNative || (cfg.GetEnforcementMode() != config.EnforcementPolicyObservationOnly && host.Reason == cgroup.EnforcementReasonSystemdOwnsHostWorkloads) {
		dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, dialErr := (&net.Dialer{}).DialContext(dialCtx, "unix", "/run/systemd/private")
		cancel()
		if dialErr == nil {
			dialErr = conn.Close()
		}
		add("systemd_socket", dialErr, "PID 1 private socket accepts connections; D-Bus authorization is unverified")
	} else {
		unknown("systemd_socket", "native authority is not established in this namespace; startup resolves the configured enforcement mode")
	}
	unknown("systemd_enforcement", "no property mutation or lease recovery attempted; controller admission, authorization and complete readback require startup")
	if cfg.MetricsDBEnabled {
		exists, err := database.InspectSQLiteStorage(cfg.MetricsDBPath)
		if err != nil {
			add("database_metadata", err, "")
		} else if exists {
			add("database_metadata", nil, "existing directory, database and sidecars satisfy startup metadata rules")
		} else {
			unknown("database_metadata", "storage is absent or in memory; creation and schema initialization were not attempted")
		}
		unknown("database_runtime", "SQLite is not opened; schema compatibility, locking and write access remain unverified")
	}
	if cfg.EnablePrometheus {
		if cfg.PrometheusTLSEnabled {
			_, err = tlsconfig.BuildServer(tlsconfig.ServerOptions{CertFile: cfg.PrometheusTLSCertFile, KeyFile: cfg.PrometheusTLSKeyFile, CAFile: cfg.PrometheusTLSCAFile, MinVersion: cfg.PrometheusTLSMinVersion})
			add("prometheus_tls", err, "TLS credentials parse and match")
		}
		if cfg.PrometheusAuthType == "basic" || cfg.PrometheusAuthType == "both" {
			add("prometheus_password", checkSecretFile(cfg.PrometheusAuthPasswordFile), "nonempty credential file is readable")
		}
		if cfg.PrometheusAuthType == "jwt" || cfg.PrometheusAuthType == "both" {
			add("prometheus_jwt", checkSecretFile(cfg.PrometheusJWTSecretFile), "nonempty credential file is readable")
		}
	}
	if cfg.MCPEnabled && cfg.MCPTransport == "http" {
		_, err = tlsconfig.BuildServer(tlsconfig.ServerOptions{CertFile: cfg.MCPTLSCertFile, KeyFile: cfg.MCPTLSKeyFile, CAFile: cfg.MCPTLSCAFile, MinVersion: cfg.MCPTLSMinVersion})
		add("mcp_tls", err, "TLS credentials parse and match")
	}
	unknown("runtime_permissions", "no files created, no ports bound, no PSI registrations or BPF programs loaded; effective write access, service sandbox and SELinux policy require operational validation")
	return r
}

func checkSecretFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read credential file: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return fmt.Errorf("credential file is empty")
	}
	return nil
}
