package blockiops

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestEmbeddedObjectMatchesSourceManifest(t *testing.T) {
	manifest, err := os.ReadFile("bpf/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(manifest)), "\n")
	if len(lines) != 2 {
		t.Fatal("source/object integrity manifest must contain exactly two files")
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatal("malformed object integrity manifest")
		}
		if fields[1] != "bpf/collector.c" && fields[1] != "bpf/collector_bpfel.o" {
			t.Fatal("unexpected object manifest input")
		}
		data, err := os.ReadFile(fields[1])
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != fields[0] {
			t.Fatalf("%s changed without regenerating the kernel object and manifest", fields[1])
		}
		if fields[1] == "bpf/collector_bpfel.o" && sha256.Sum256(collectorObject) != hash {
			t.Fatal("embedded object differs from source artifact")
		}
	}
}
