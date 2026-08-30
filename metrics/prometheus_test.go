package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	"github.com/golang-jwt/jwt/v5"
)

func TestGetMetricsEndpointUsesConfiguredScheme(t *testing.T) {
	cfg := config.DefaultConfig()
	exporter := &PrometheusExporter{cfg: cfg}
	if got := exporter.GetMetricsEndpoint(); got != "http://127.0.0.1:1974/metrics" {
		t.Fatalf("HTTP metrics endpoint = %q", got)
	}

	cfg.PrometheusTLSEnabled = true
	if got := exporter.GetMetricsEndpoint(); got != "https://127.0.0.1:1974/metrics" {
		t.Fatalf("HTTPS metrics endpoint = %q", got)
	}
}

func TestPrometheusStartReportsBindFailureSynchronously(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve test port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusMetricsBindHost = "127.0.0.1"
	cfg.PrometheusMetricsBindPort = listener.Addr().(*net.TCPAddr).Port
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}

	if err := exporter.Start(context.Background()); err == nil {
		t.Fatal("Start() succeeded on an occupied port")
	}
	if exporter.IsRunning() {
		t.Fatal("exporter remained marked running after bind failure")
	}
}

func TestPrometheusStopWaitsForShutdownAndServeCompletion(t *testing.T) {
	exporter := newStartedTestPrometheusExporter(t)

	shutdownStarted := make(chan struct{})
	releaseShutdown := make(chan struct{})
	defer func() {
		select {
		case <-releaseShutdown:
		default:
			close(releaseShutdown)
		}
	}()
	exporter.shutdownHTTPServer = func(server *http.Server, ctx context.Context) error {
		close(shutdownStarted)
		<-releaseShutdown
		return server.Shutdown(ctx)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- exporter.Stop() }()
	<-shutdownStarted
	select {
	case err := <-stopDone:
		t.Fatalf("Stop() returned before HTTP shutdown completed: %v", err)
	default:
	}

	close(releaseShutdown)
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if exporter.IsRunning() {
		t.Fatal("exporter remained running after Stop() completed")
	}
}

func TestPrometheusStopPropagatesShutdownFailure(t *testing.T) {
	exporter := newStartedTestPrometheusExporter(t)
	wantErr := errors.New("injected shutdown failure")
	exporter.shutdownHTTPServer = func(*http.Server, context.Context) error { return wantErr }
	exporter.closeHTTPServer = func(server *http.Server) error { return server.Close() }

	if err := exporter.Stop(); !errors.Is(err, wantErr) {
		t.Fatalf("Stop() error = %v, want error wrapping %v", err, wantErr)
	}
	if exporter.IsRunning() {
		t.Fatal("exporter remained running after failed graceful shutdown and forced close")
	}
}

func newStartedTestPrometheusExporter(t *testing.T) *PrometheusExporter {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusAuthType = "none"
	cfg.PrometheusMetricsBindHost = "127.0.0.1"
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}
	// The constructor validates the shipped port, while port zero lets the
	// kernel choose an isolated listener for this lifecycle test.
	cfg.PrometheusMetricsBindPort = 0
	if err := exporter.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() { _ = exporter.Stop() })
	return exporter
}

func TestRecordLimitHookExecutionUsesBoundedLabels(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}

	exporter.RecordLimitHookExecution(LimitHookTypeScript, LimitHookOutcomeSuccess)
	exporter.RecordLimitHookExecution(LimitHookTypeScript, LimitHookOutcomeSuccess)
	exporter.RecordLimitHookExecution(LimitHookTypeHTTP, LimitHookOutcomeFailure)
	exporter.RecordLimitHookExecution(LimitHookType("unbounded"), LimitHookOutcome("unbounded"))

	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "resman_limit_hook_executions_total" {
			continue
		}
		if len(family.Metric) != 2 {
			t.Fatalf("limit-hook metric series = %d, want 2 bounded series", len(family.Metric))
		}
		got := make(map[string]float64, len(family.Metric))
		for _, metric := range family.Metric {
			labels := make(map[string]string, len(metric.Label))
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			got[labels["hook_type"]+"/"+labels["outcome"]] = metric.GetCounter().GetValue()
		}
		if got["script/success"] != 2 || got["http/failure"] != 1 {
			t.Fatalf("limit-hook metric values = %+v, want script/success=2 and http/failure=1", got)
		}
		return
	}
	t.Fatal("resman_limit_hook_executions_total metric not found")
}

func TestNewPrometheusExporterAppliesTLSAndClientCA(t *testing.T) {
	certFile, keyFile, caFile := writeTestTLSMaterial(t)

	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusTLSEnabled = true
	cfg.PrometheusTLSCertFile = certFile
	cfg.PrometheusTLSKeyFile = keyFile
	cfg.PrometheusTLSCAFile = caFile
	cfg.PrometheusTLSMinVersion = "1.3"

	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}
	if exporter.tlsConfig == nil {
		t.Fatal("TLS configuration was not created")
	}
	if exporter.tlsConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("TLS minimum version = %d, want TLS 1.3", exporter.tlsConfig.MinVersion)
	}
	if exporter.tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("TLS client authentication = %d, want RequireAndVerifyClientCert", exporter.tlsConfig.ClientAuth)
	}
	if exporter.tlsConfig.ClientCAs == nil {
		t.Fatal("TLS client CA pool was not configured")
	}
}

func TestNewPrometheusExporterRejectsInvalidClientCA(t *testing.T) {
	certFile, keyFile, _ := writeTestTLSMaterial(t)
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusTLSEnabled = true
	cfg.PrometheusTLSCertFile = certFile
	cfg.PrometheusTLSKeyFile = keyFile
	cfg.PrometheusTLSCAFile = writeCredentialFile(t, "ca.crt", "not a certificate")

	if exporter, err := NewPrometheusExporter(cfg); err == nil || exporter != nil {
		t.Fatalf("NewPrometheusExporter() = (%v, %v), want nil exporter and error", exporter, err)
	}
}

func TestNewPrometheusExporterRejectsInvalidTLSKeyPair(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusTLSEnabled = true
	cfg.PrometheusTLSCertFile = writeCredentialFile(t, "server.crt", "not a certificate")
	cfg.PrometheusTLSKeyFile = writeCredentialFile(t, "server.key", "not a key")

	if exporter, err := NewPrometheusExporter(cfg); err == nil || exporter != nil {
		t.Fatalf("NewPrometheusExporter() = (%v, %v), want nil exporter and error", exporter, err)
	}
}

func writeTestTLSMaterial(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	testServer := httptest.NewTLSServer(nil)
	t.Cleanup(testServer.Close)

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: testServer.Certificate().Raw,
	})
	keyDER, err := x509.MarshalPKCS8PrivateKey(testServer.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatalf("failed to encode test TLS key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	caFile = filepath.Join(dir, "ca.crt")
	for path, contents := range map[string][]byte{
		certFile: certPEM,
		keyFile:  keyPEM,
		caFile:   certPEM,
	} {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatalf("failed to write TLS material %s: %v", path, err)
		}
	}
	return certFile, keyFile, caFile
}

func TestNewPrometheusExporterRejectsInvalidCredentials(t *testing.T) {
	tests := []struct {
		name      string
		authType  string
		username  string
		password  string
		jwtSecret string
	}{
		{name: "missing basic password file", authType: "basic", username: "prometheus"},
		{name: "missing basic username", authType: "basic", password: "secret"},
		{name: "empty basic password", authType: "basic", username: "prometheus", password: " \n"},
		{name: "missing JWT secret file", authType: "jwt"},
		{name: "empty JWT secret", authType: "jwt", jwtSecret: " \n"},
		{name: "both requires basic credentials", authType: "both", jwtSecret: "jwt-secret"},
		{name: "both requires JWT credentials", authType: "both", username: "prometheus", password: "secret"},
		{name: "unsupported auth type", authType: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.EnablePrometheus = true
			cfg.PrometheusAuthType = tt.authType
			cfg.PrometheusAuthUsername = tt.username
			if tt.password != "" {
				cfg.PrometheusAuthPasswordFile = writeCredentialFile(t, "password", tt.password)
			}
			if tt.jwtSecret != "" {
				cfg.PrometheusJWTSecretFile = writeCredentialFile(t, "jwt", tt.jwtSecret)
			}

			if exporter, err := NewPrometheusExporter(cfg); err == nil || exporter != nil {
				t.Fatalf("NewPrometheusExporter() = (%v, %v), want nil exporter and error", exporter, err)
			}
		})
	}
}

func TestNewPrometheusExporterRejectsUnreadableCredentialFile(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusAuthType = "basic"
	cfg.PrometheusAuthUsername = "prometheus"
	cfg.PrometheusAuthPasswordFile = filepath.Join(t.TempDir(), "missing")

	if exporter, err := NewPrometheusExporter(cfg); err == nil || exporter != nil {
		t.Fatalf("NewPrometheusExporter() = (%v, %v), want nil exporter and error", exporter, err)
	}
}

func TestCheckJWTAuthRequiresExpiration(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PrometheusAuthType = "jwt"
	cfg.PrometheusJWTIssuer = "resman"
	cfg.PrometheusJWTAudience = "prometheus"
	secret := []byte("test-secret")
	exporter := &PrometheusExporter{
		cfg:       cfg,
		logger:    logging.GetLogger(),
		jwtSecret: secret,
	}

	tests := []struct {
		name   string
		claims jwt.MapClaims
		want   bool
	}{
		{
			name: "missing expiration",
			claims: jwt.MapClaims{
				"iss": "resman",
				"aud": "prometheus",
			},
			want: false,
		},
		{
			name: "future expiration",
			claims: jwt.MapClaims{
				"iss": "resman",
				"aud": "prometheus",
				"exp": time.Now().Add(time.Hour).Unix(),
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, tt.claims).SignedString(secret)
			if err != nil {
				t.Fatalf("failed to sign JWT: %v", err)
			}
			request := httptest.NewRequest("GET", "/metrics", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			if got := exporter.checkJWTAuth(request); got != tt.want {
				t.Fatalf("checkJWTAuth() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestAuthenticationChecksFailClosedWithoutLoadedSecrets(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PrometheusAuthType = "basic"
	request := httptest.NewRequest("GET", "/metrics", nil)
	request.SetBasicAuth("", "")
	exporter := &PrometheusExporter{
		cfg:    cfg,
		logger: logging.GetLogger(),
	}
	if exporter.checkBasicAuth(request) {
		t.Fatal("empty Basic credentials were accepted")
	}

	cfg.PrometheusAuthType = "jwt"
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte{})
	if err != nil {
		t.Fatalf("failed to sign empty-secret JWT: %v", err)
	}
	request = httptest.NewRequest("GET", "/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	if exporter.checkJWTAuth(request) {
		t.Fatal("JWT signed with an empty secret was accepted")
	}
}

func TestCleanupUserMetricsRemovesCPUAverageAndEMASeries(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusAuthType = "none"

	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}
	exporter.UpdateUserSnapshot(UserExporterMetrics{
		UID: 1000, Username: "testuser", CPUUsagePercent: 25,
		CPUUsageAverage: 20, CPUUsageEMA: 22, MemoryUsageBytes: 1024,
		ProcessCount: 2,
	})

	before, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() before cleanup error: %v", err)
	}
	beforeCounts := make(map[string]int, len(before))
	for _, family := range before {
		beforeCounts[family.GetName()] = len(family.Metric)
	}
	for _, name := range []string{
		"resman_user_cpu_usage_average_percent",
		"resman_user_cpu_usage_ema_percent",
	} {
		if beforeCounts[name] != 1 {
			t.Fatalf("%s series before cleanup = %d, want 1", name, beforeCounts[name])
		}
	}

	exporter.CleanupUserMetrics(map[int]bool{})
	after, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() after cleanup error: %v", err)
	}
	afterCounts := make(map[string]int, len(after))
	for _, family := range after {
		afterCounts[family.GetName()] = len(family.Metric)
	}
	for _, name := range []string{
		"resman_user_cpu_usage_average_percent",
		"resman_user_cpu_usage_ema_percent",
	} {
		if afterCounts[name] != 0 {
			t.Fatalf("%s series after cleanup = %d, want 0", name, afterCounts[name])
		}
	}
}

func TestUpdateUserSnapshotPublishesObservedCPULimitState(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}

	snapshot := UserExporterMetrics{UID: 1000, Username: "alice", CPUUsagePercent: 10, CPUUsageAverage: 10, CPUUsageEMA: 10, MemoryUsageBytes: 1024, ProcessCount: 1}
	exporter.UpdateUserSnapshot(snapshot)
	if got := gatheredMetricValue(t, exporter, "resman_user_cpu_limit_active"); got != 0 {
		t.Fatalf("inactive observed CPU limit gauge = %f, want 0", got)
	}
	snapshot.CPULimitActive = true
	exporter.UpdateUserSnapshot(snapshot)
	if got := gatheredMetricValue(t, exporter, "resman_user_cpu_limit_active"); got != 1 {
		t.Fatalf("active observed CPU limit gauge = %f, want 1", got)
	}
}

func TestUpdateUserSnapshotRemovesUnavailableAndStaleCgroupSeries(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}

	root := t.TempDir()
	pathA := filepath.Join(root, "user_1000")
	pathB := filepath.Join(root, "limited", "user_1000")
	writeMemoryCurrent := func(path, value string) {
		t.Helper()
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatalf("create cgroup fixture %s: %v", path, err)
		}
		if err := os.WriteFile(filepath.Join(path, "memory.current"), []byte(value), 0644); err != nil {
			t.Fatalf("write memory.current in %s: %v", path, err)
		}
	}
	writeMemoryCurrent(pathA, "111\n")

	snapshot := UserExporterMetrics{
		UID: 1000, Username: "alice", ProcessCount: 1,
		CgroupPath: pathA, CPUQuota: "50000 100000",
	}
	exporter.UpdateUserSnapshot(snapshot)
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_cpu_quota_microseconds", map[string]float64{pathA: 50000})
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_cpu_period_microseconds", map[string]float64{pathA: 100000})
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_memory_usage_bytes", map[string]float64{pathA: 111})
	if got := gatheredMetricHelp(t, exporter, "resman_cgroup_cpu_quota_microseconds"); !strings.Contains(got, "absent when cpu.max is unlimited or unavailable") {
		t.Fatalf("quota help does not describe availability: %q", got)
	}

	// Unlimited is a valid cpu.max record: retain its period but remove the
	// previously published finite quota.
	snapshot.CPUQuota = "max 100000"
	exporter.UpdateUserSnapshot(snapshot)
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_cpu_quota_microseconds", nil)
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_cpu_period_microseconds", map[string]float64{pathA: 100000})

	// A malformed pair is atomic: neither half may remain visible.
	snapshot.CPUQuota = "abc 100000"
	exporter.UpdateUserSnapshot(snapshot)
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_cpu_quota_microseconds", nil)
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_cpu_period_microseconds", nil)

	// An unavailable memory.current removes the old value rather than
	// publishing zero or retaining the last observation.
	if err := os.Remove(filepath.Join(pathA, "memory.current")); err != nil {
		t.Fatalf("remove memory.current fixture: %v", err)
	}
	snapshot.CPUQuota = "50000 100000"
	exporter.UpdateUserSnapshot(snapshot)
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_memory_usage_bytes", nil)

	// A placement transition removes every series for the previous path.
	writeMemoryCurrent(pathB, "222\n")
	snapshot.CgroupPath = pathB
	snapshot.CPUQuota = "25000 100000"
	exporter.UpdateUserSnapshot(snapshot)
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_cpu_quota_microseconds", map[string]float64{pathB: 25000})
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_cpu_period_microseconds", map[string]float64{pathB: 100000})
	assertCgroupGaugeSeries(t, exporter, "resman_cgroup_memory_usage_bytes", map[string]float64{pathB: 222})

	// Releasing the cgroup removes its current labels immediately.
	snapshot.CgroupPath = ""
	snapshot.CPUQuota = ""
	exporter.UpdateUserSnapshot(snapshot)
	for _, name := range []string{
		"resman_cgroup_cpu_quota_microseconds",
		"resman_cgroup_cpu_period_microseconds",
		"resman_cgroup_memory_usage_bytes",
	} {
		assertCgroupGaugeSeries(t, exporter, name, nil)
	}

	// Cleanup also owns cgroup-labelled series when the user disappears.
	snapshot.CgroupPath = pathB
	snapshot.CPUQuota = "25000 100000"
	exporter.UpdateUserSnapshot(snapshot)
	exporter.CleanupUserMetrics(map[int]bool{})
	for _, name := range []string{
		"resman_cgroup_cpu_quota_microseconds",
		"resman_cgroup_cpu_period_microseconds",
		"resman_cgroup_memory_usage_bytes",
	} {
		assertCgroupGaugeSeries(t, exporter, name, nil)
	}
}

func TestUpdateSystemSnapshotPublishesEveryTypedGaugeWithoutCountingItAsControlCycle(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}

	exporter.UpdateSystemSnapshot(SystemExporterMetrics{TotalCPUUsage: 10})
	exporter.UpdateSystemSnapshot(SystemExporterMetrics{
		TotalCPUUsage:                                25,
		TotalCores:                                   8,
		ActionCores:                                  6,
		ObservedUsersCPUUsage:                        40,
		ObservedUsersCount:                           5,
		ObservedUsersMemoryUsage:                     1024,
		CPUEligibleUsersCPUUsage:                     30,
		CPUEligibleUsersCount:                        3,
		CPUEligibleUsersMemoryUsage:                  512,
		RAMEligibleUsersCount:                        4,
		RAMEligibleUsersMemoryUsage:                  768,
		IOEligibleUsersCount:                         2,
		IOEligibleUsersReadBytesPerSecond:            100,
		IOEligibleUsersWriteBytesPerSecond:           200,
		IOEligibleUsersReadBlockOperationsPerSecond:  10,
		IOEligibleUsersWriteBlockOperationsPerSecond: 20,
		CPUActivelyLimitedUsersCount:                 2,
		ActivelyLimitedUsersCount:                    3,
		CPULimitsActive:                              true,
		ResourceLimitsActive:                         true,
		AnyLimitsActive:                              true,
		MemoryUsageMB:                                256,
		TotalMemoryMB:                                2048,
		CachedMemoryMB:                               128,
		SystemLoad:                                   1.5,
		ProcFSExecutableIdentityUnavailableProcesses: 2,
		ProcFSIOUnavailableProcesses:                 3,
	})

	wantMetrics := map[string]float64{
		"resman_cpu_total_usage_percent":                             25,
		"resman_cpu_total_cores":                                     8,
		"resman_cpu_action_cores":                                    6,
		"resman_all_users_cpu_usage_percent":                         40,
		"resman_all_users_count":                                     5,
		"resman_all_users_memory_usage_bytes":                        1024,
		"resman_cpu_eligible_users_cpu_usage_percent":                30,
		"resman_cpu_eligible_users_count":                            3,
		"resman_cpu_eligible_users_memory_usage_bytes":               512,
		"resman_ram_eligible_users_count":                            4,
		"resman_ram_eligible_users_memory_usage_bytes":               768,
		"resman_io_eligible_users_count":                             2,
		"resman_io_eligible_users_read_bytes_per_second":             100,
		"resman_io_eligible_users_write_bytes_per_second":            200,
		"resman_io_eligible_users_read_block_operations_per_second":  10,
		"resman_io_eligible_users_write_block_operations_per_second": 20,
		"resman_cpu_actively_limited_users_count":                    2,
		"resman_actively_limited_users_count":                        3,
		"resman_cpu_limits_active":                                   1,
		"resman_resource_limits_active":                              1,
		"resman_any_limits_active":                                   1,
		"resman_memory_usage_megabytes":                              256,
		"resman_memory_total_megabytes":                              2048,
		"resman_memory_cached_megabytes":                             128,
		"resman_system_load_average":                                 1.5,
	}
	for name, want := range wantMetrics {
		if got := gatheredMetricValue(t, exporter, name); got != want {
			t.Errorf("%s = %f, want %f", name, got, want)
		}
	}
	if got := gatheredMetricValue(t, exporter, "resman_control_cycles_total"); got != 0 {
		t.Fatalf("control cycles after metrics-only refresh = %f, want 0", got)
	}

	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	wantCoverage := map[string]float64{"executable_identity": 2, "io_decision": 3}
	foundCoverage := false
	oldAmbiguousNames := map[string]bool{
		"resman_limited_users_count_filtered":     true,
		"resman_limited_users_cpu_usage_percent":  true,
		"resman_limited_users_memory_usage_bytes": true,
		"resman_limited_users_count":              true,
		"resman_limits_active":                    true,
		"resman_user_cpu_limited":                 true,
		"resman_limits_activated_total":           true,
		"resman_limits_deactivated_total":         true,
	}
	for _, family := range families {
		if oldAmbiguousNames[family.GetName()] {
			t.Errorf("legacy ambiguous metric %s is still registered", family.GetName())
		}
		if family.GetName() != "resman_procfs_unavailable_processes" {
			continue
		}
		foundCoverage = true
		if len(family.Metric) != len(wantCoverage) {
			t.Fatalf("procfs coverage series = %d, want %d", len(family.Metric), len(wantCoverage))
		}
		for _, metric := range family.Metric {
			access := ""
			for _, label := range metric.Label {
				if label.GetName() == "access" {
					access = label.GetValue()
				}
			}
			want, ok := wantCoverage[access]
			if !ok {
				t.Fatalf("unexpected procfs access label %q", access)
			}
			if got := metric.GetGauge().GetValue(); got != want {
				t.Fatalf("procfs coverage %s = %f, want %f", access, got, want)
			}
		}
	}
	if !foundCoverage {
		t.Fatal("resman_procfs_unavailable_processes metric family not found")
	}

	exporter.RecordControlCycleTrigger("polling")
	exporter.RecordControlCycleTrigger("psi")
	if got := gatheredMetricValue(t, exporter, "resman_control_cycles_total"); got != 2 {
		t.Fatalf("control cycles after two triggers = %f, want 2", got)
	}
}

func TestRecordErrorPublishesOneBoundedSeries(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}

	exporter.RecordError("metrics_database", "write_failure")
	exporter.RecordError("metrics_database", "write_failure")

	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "resman_errors_total" {
			continue
		}
		if len(family.Metric) != 1 {
			t.Fatalf("resman_errors_total series = %d, want 1", len(family.Metric))
		}
		metric := family.Metric[0]
		if got := metric.Counter.GetValue(); got != 2 {
			t.Fatalf("resman_errors_total value = %f, want 2", got)
		}
		labels := make(map[string]string, len(metric.Label))
		for _, label := range metric.Label {
			labels[label.GetName()] = label.GetValue()
		}
		if labels["component"] != "metrics_database" || labels["error_type"] != "write_failure" {
			t.Fatalf("resman_errors_total labels = %+v, want metrics_database/write_failure", labels)
		}
		return
	}
	t.Fatal("resman_errors_total metric family not found")
}

func TestRecordCgroupIngressSkipsPublishesOnlyBoundedReasons(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}

	exporter.RecordCgroupIngressSkips(cgroup.ProcessMoveResult{
		PIDNamespaceMismatches:  2,
		PIDNamespaceUnavailable: 1,
	})
	exporter.RecordCgroupIngressSkips(cgroup.ProcessMoveResult{PIDNamespaceMismatches: 3})

	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "resman_cgroup_ingress_skipped_total" {
			continue
		}
		if len(family.Metric) != 2 {
			t.Fatalf("resman_cgroup_ingress_skipped_total series = %d, want 2", len(family.Metric))
		}
		values := make(map[string]float64, len(family.Metric))
		for _, metric := range family.Metric {
			if len(metric.Label) != 3 {
				t.Fatalf("metric labels = %+v, want reason plus two static labels", metric.Label)
			}
			for _, label := range metric.Label {
				if label.GetName() == "reason" {
					values[label.GetValue()] = metric.Counter.GetValue()
				}
			}
		}
		if values[string(cgroup.PIDNamespaceMismatch)] != 5 || values[string(cgroup.PIDNamespaceUnavailable)] != 1 {
			t.Fatalf("bounded ingress skip values = %v, want mismatch=5 unavailable=1", values)
		}
		return
	}
	t.Fatal("resman_cgroup_ingress_skipped_total metric family not found")
}

func TestOperationalMetricsPublishTruthfulBoundedSeries(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.ServerRole = "test-role"
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}

	exporter.IncrementCPULimitsActivated()
	exporter.IncrementCPULimitsActivated()
	exporter.IncrementCPULimitsDeactivated()
	exporter.RecordControlCycleDuration(2 * time.Second)
	exporter.RecordControlCycleDuration(3 * time.Second)
	exporter.RecordMetricsCollectionDuration(25 * time.Millisecond)
	exporter.RecordError("limit_transition", "activation_failure")
	exporter.RecordError("limit_transition", "activation_failure")

	tests := []struct {
		name               string
		wantCounter        float64
		wantHistogramCount uint64
		wantHelp           string
		wantLabels         map[string]string
	}{
		{
			name:        "resman_cpu_limits_activated_total",
			wantCounter: 2,
			wantHelp:    "Total confirmed transitions from inactive to active CPU limits",
		},
		{
			name:        "resman_cpu_limits_deactivated_total",
			wantCounter: 1,
			wantHelp:    "Total confirmed transitions from active to inactive CPU limits",
		},
		{
			name:               "resman_control_cycle_duration_seconds",
			wantHistogramCount: 2,
			wantHelp:           "Duration of control cycles, including failed and suspended cycles, in seconds",
		},
		{
			name:               "resman_metrics_collection_duration_seconds",
			wantHistogramCount: 1,
			wantHelp:           "Duration of system metrics collection for control cycles and metrics-only refreshes in seconds",
		},
		{
			name:        "resman_errors_total",
			wantCounter: 2,
			wantHelp:    "Total number of operational errors by component and bounded error type",
			wantLabels: map[string]string{
				"component":  "limit_transition",
				"error_type": "activation_failure",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gatheredOperationalMetric(t, exporter, tt.name)
			if got.series != 1 {
				t.Errorf("series = %d, want 1", got.series)
			}
			if got.help != tt.wantHelp {
				t.Errorf("help = %q, want %q", got.help, tt.wantHelp)
			}
			if tt.wantHistogramCount > 0 {
				if got.histogramCount != tt.wantHistogramCount {
					t.Errorf("histogram sample count = %d, want %d", got.histogramCount, tt.wantHistogramCount)
				}
			} else if got.counter != tt.wantCounter {
				t.Errorf("counter = %f, want %f", got.counter, tt.wantCounter)
			}
			for label, value := range map[string]string{
				"hostname":    exporter.hostname,
				"server_role": "test-role",
			} {
				if got.labels[label] != value {
					t.Errorf("label %s = %q, want %q", label, got.labels[label], value)
				}
			}
			for label, value := range tt.wantLabels {
				if got.labels[label] != value {
					t.Errorf("label %s = %q, want %q", label, got.labels[label], value)
				}
			}
		})
	}
}

func TestIOOperationMetricHelpDescribesSyscallCounters(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}

	exporter.UpdateUserSnapshot(UserExporterMetrics{
		UID: 1000, Username: "testuser", ProcessCount: 1,
		ObservedIOReadOps: 10, ObservedIOWriteOps: 20,
	})

	tests := []struct {
		name string
		help string
	}{
		{
			name: "resman_user_io_read_ops_total",
			help: "Total read-family syscalls reported by /proc/PID/io syscr per user",
		},
		{
			name: "resman_user_io_write_ops_total",
			help: "Total write-family syscalls reported by /proc/PID/io syscw per user",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gatheredMetricHelp(t, exporter, tt.name); got != tt.help {
				t.Fatalf("metric help = %q, want %q", got, tt.help)
			}
		})
	}
}

func TestPrometheusExporterUsesSharedUsernameResolver(t *testing.T) {
	exporter := &PrometheusExporter{}
	var resolvedUID int
	exporter.SetUsernameResolver(func(uid int) string {
		resolvedUID = uid
		return "directory-user"
	})

	if got := exporter.getUsernameFromUID("1007"); got != "directory-user" {
		t.Fatalf("resolved username = %q, want directory-user", got)
	}
	if resolvedUID != 1007 {
		t.Fatalf("resolver UID = %d, want 1007", resolvedUID)
	}
}

func gatheredMetricValue(t *testing.T, exporter *PrometheusExporter, name string) float64 {
	t.Helper()
	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name || len(family.Metric) == 0 {
			continue
		}
		metric := family.Metric[0]
		if metric.Gauge != nil {
			return metric.Gauge.GetValue()
		}
		if metric.Counter != nil {
			return metric.Counter.GetValue()
		}
		t.Fatalf("metric %s is neither gauge nor counter", name)
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func assertCgroupGaugeSeries(t *testing.T, exporter *PrometheusExporter, name string, expected map[string]float64) {
	t.Helper()
	actual := make(map[string]float64)
	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			path := ""
			for _, label := range metric.Label {
				if label.GetName() == "cgroup_path" {
					path = label.GetValue()
					break
				}
			}
			actual[path] = metric.GetGauge().GetValue()
		}
	}
	if len(actual) != len(expected) {
		t.Fatalf("%s series = %v, want %v", name, actual, expected)
	}
	for path, want := range expected {
		if got, ok := actual[path]; !ok || got != want {
			t.Fatalf("%s[%s] = %f, %t; want %f, true", name, path, got, ok, want)
		}
	}
}

type operationalMetricSnapshot struct {
	series         int
	counter        float64
	histogramCount uint64
	help           string
	labels         map[string]string
}

func gatheredOperationalMetric(t *testing.T, exporter *PrometheusExporter, name string) operationalMetricSnapshot {
	t.Helper()
	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name || len(family.Metric) == 0 {
			continue
		}
		metric := family.Metric[0]
		labels := make(map[string]string, len(metric.Label))
		for _, label := range metric.Label {
			labels[label.GetName()] = label.GetValue()
		}
		return operationalMetricSnapshot{
			series:         len(family.Metric),
			counter:        metric.GetCounter().GetValue(),
			histogramCount: metric.GetHistogram().GetSampleCount(),
			help:           family.GetHelp(),
			labels:         labels,
		}
	}
	t.Fatalf("metric %s not found", name)
	return operationalMetricSnapshot{}
}

func gatheredMetricHelp(t *testing.T, exporter *PrometheusExporter, name string) string {
	t.Helper()
	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family.GetHelp()
		}
	}
	t.Fatalf("metric %s not found", name)
	return ""
}

func writeCredentialFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("failed to write credential file: %v", err)
	}
	return path
}
