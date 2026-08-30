package docker

import (
	"os"
	"strings"
	"testing"
)

func TestShippedContainerPreservesRuntimePrivilegeContract(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read shipped Dockerfile: %v", err)
	}
	content := string(data)
	for _, required := range []string{
		"FROM docker.io/library/oraclelinux:9 AS builder",
		"RUN CGO_ENABLED=1 GOOS=linux go build",
		"dnf install -y ca-certificates sssd-client tzdata",
		"install -d -m 0700 /etc/resman",
		"/etc/resman/tls",
		"COPY config/resman.conf.example /etc/resman/resman.conf",
		"chmod 0600 /etc/resman/resman.conf",
		`CMD ["--config", "/etc/resman/resman.conf"]`,
		"USER 0",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("Dockerfile is missing required runtime contract %q", required)
		}
	}
	for _, forbidden := range []string{"CGO_ENABLED=0", "USER appuser", "apk add"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("Dockerfile retained unsupported runtime contract %q", forbidden)
		}
	}
}

func TestContainerDocumentationPinsRootfulPodmanNamespacesAndMounts(t *testing.T) {
	data, err := os.ReadFile("../../docs/CONTAINER.md")
	if err != nil {
		t.Fatalf("read container documentation: %v", err)
	}
	content := string(data)
	for _, required := range []string{
		"sudo podman run",
		"--privileged",
		"--pid=host",
		"--cgroupns=host",
		"/sys/fs/cgroup:/sys/fs/cgroup:rw",
		"/etc/resman:/etc/resman:rw",
		"/etc/passwd:/etc/passwd:ro",
		"/etc/nsswitch.conf:/etc/nsswitch.conf:ro",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("container documentation is missing %q", required)
		}
	}
}
