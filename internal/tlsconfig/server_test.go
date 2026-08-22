/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		value string
		want  uint16
	}{
		{value: "1.0", want: tls.VersionTLS10},
		{value: "1.1", want: tls.VersionTLS11},
		{value: "1.2", want: tls.VersionTLS12},
		{value: "1.3", want: tls.VersionTLS13},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseVersion(tt.value)
			if err != nil {
				t.Fatalf("parseVersion(%q) error: %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("parseVersion(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}

	if _, err := parseVersion("SSLv3"); err == nil {
		t.Fatal("parseVersion() accepted an unsupported version")
	}
}

func TestBuildServerLoadsCertificateAndOptionalClientCA(t *testing.T) {
	certFile, keyFile, caFile := writeTestTLSMaterial(t)
	tests := []struct {
		name           string
		caFile         string
		wantClientAuth tls.ClientAuthType
	}{
		{name: "server TLS", wantClientAuth: tls.NoClientCert},
		{name: "mutual TLS", caFile: caFile, wantClientAuth: tls.RequireAndVerifyClientCert},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := BuildServer(ServerOptions{
				CertFile:   certFile,
				KeyFile:    keyFile,
				CAFile:     tt.caFile,
				MinVersion: "1.3",
			})
			if err != nil {
				t.Fatalf("BuildServer() error: %v", err)
			}
			if cfg.MinVersion != tls.VersionTLS13 {
				t.Fatalf("MinVersion = %d, want TLS 1.3", cfg.MinVersion)
			}
			if len(cfg.Certificates) != 1 {
				t.Fatalf("Certificates = %d, want 1", len(cfg.Certificates))
			}
			if cfg.ClientAuth != tt.wantClientAuth {
				t.Fatalf("ClientAuth = %d, want %d", cfg.ClientAuth, tt.wantClientAuth)
			}
		})
	}
}

func TestBuildServerRejectsMissingOrInvalidMaterial(t *testing.T) {
	tests := []struct {
		name    string
		options ServerOptions
	}{
		{name: "missing key", options: ServerOptions{CertFile: "cert", MinVersion: "1.3"}},
		{name: "invalid keypair", options: ServerOptions{CertFile: "missing-cert", KeyFile: "missing-key", MinVersion: "1.3"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if cfg, err := BuildServer(tt.options); err == nil || cfg != nil {
				t.Fatalf("BuildServer() = (%v, %v), want nil config and error", cfg, err)
			}
		})
	}
}

func writeTestTLSMaterial(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	testServer := httptest.NewTLSServer(nil)
	t.Cleanup(testServer.Close)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: testServer.Certificate().Raw})
	keyDER, err := x509.MarshalPKCS8PrivateKey(testServer.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatalf("encode TLS key: %v", err)
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
			t.Fatalf("write TLS material %s: %v", path, err)
		}
	}
	return certFile, keyFile, caFile
}
