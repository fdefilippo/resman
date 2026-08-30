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
	"fmt"
	"os"
	"strings"
)

// ServerOptions defines the shared TLS contract for ResMan HTTP listeners.
type ServerOptions struct {
	CertFile   string
	KeyFile    string
	CAFile     string
	MinVersion string
}

// BuildServer loads server credentials and returns a hardened TLS configuration.
func BuildServer(options ServerOptions) (*tls.Config, error) {
	if strings.TrimSpace(options.CertFile) == "" || strings.TrimSpace(options.KeyFile) == "" {
		return nil, fmt.Errorf("TLS certificate and key files must both be configured")
	}

	certificate, err := tls.LoadX509KeyPair(options.CertFile, options.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading TLS certificate and key: %w", err)
	}

	version, err := parseVersion(options.MinVersion)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		MinVersion:   version,
		Certificates: []tls.Certificate{certificate},
	}
	if strings.TrimSpace(options.CAFile) == "" {
		return tlsConfig, nil
	}

	caPEM, err := os.ReadFile(options.CAFile)
	if err != nil {
		return nil, fmt.Errorf("reading TLS CA file %s: %w", options.CAFile, err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("TLS CA file %s does not contain a valid PEM certificate", options.CAFile)
	}
	tlsConfig.ClientCAs = clientCAs
	tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	return tlsConfig, nil
}

func parseVersion(version string) (uint16, error) {
	switch strings.TrimSpace(version) {
	case "1.0":
		return tls.VersionTLS10, nil
	case "1.1":
		return tls.VersionTLS11, nil
	case "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("invalid TLS minimum version %q: expected 1.0, 1.1, 1.2, or 1.3", version)
	}
}
