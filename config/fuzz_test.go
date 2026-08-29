package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzLoadAndValidate proves that configuration loading rejects malformed input
// with an error instead of panicking. resman-4pw.12 made loading strict, so any
// input that is not a valid contract must fail loudly and predictably.
func FuzzLoadAndValidate(f *testing.F) {
	f.Add("USER_INCLUDE_LIST=.*\n")
	f.Add("CPU_THRESHOLD=80\nUSER_INCLUDE_LIST=^app$\n")
	f.Add("UNKNOWN_KEY=1\n")
	f.Add("CPU_THRESHOLD=\n")
	f.Add("USER_INCLUDE_LIST=(\n")
	f.Add("METRICS_DB_PATH=:memory:\nMETRICS_DB_ENABLED=true\n")
	f.Add("BLACKOUT=* * * * *\n")
	f.Add("IO_READ_BPS=1M\nIO_LIMIT_ENABLED=true\n")
	f.Add("\x00\n")
	f.Add(strings.Repeat("A=1\n", 512))

	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, content string) {
		path := filepath.Join(dir, "fuzz.conf")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Skip()
		}
		cfg, err := loadAndValidateWithLayout(path, diskLayout{
			defaultConfigPath: filepath.Join(dir, "unused", "resman.conf"),
			legacyConfigPath:  filepath.Join(dir, "unused", "legacy.conf"),
			legacySavedPath:   filepath.Join(dir, "unused", "legacy.rpmsave"),
			legacyBackupPath:  filepath.Join(dir, "unused", "legacy.backup"),
			legacyTempPath:    filepath.Join(dir, "unused", "legacy.tmp"),
			defaultDBPath:     filepath.Join(dir, "unused", "metrics.db"),
			legacyDBPath:      filepath.Join(dir, "unused", "legacy.db"),
		})
		if err == nil && cfg == nil {
			t.Fatal("loadAndValidateWithLayout returned no configuration and no error")
		}
		if err != nil && cfg != nil {
			t.Fatalf("loadAndValidateWithLayout returned both a configuration and error %v", err)
		}
	})
}

// FuzzParseByteQuota exercises the byte-suffix parser that resman-4pw.16.2 found
// silently unenforced. A rejected value must produce an error, and an accepted
// value must never yield a zero byte count from a non-zero request.
func FuzzParseByteQuota(f *testing.F) {
	for _, seed := range []string{"1M", "0", "512K", "2G", "1T", "", "M", "-1", "1m", "9999999999999999999999", "1 M", "0x10"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		bytes, err := ParseByteQuota(value)
		if err != nil {
			return
		}
		trimmed := strings.TrimSpace(value)
		if bytes == 0 && trimmed != "" && trimmed != "0" && !strings.HasPrefix(trimmed, "0") {
			t.Fatalf("ParseByteQuota(%q) accepted a non-zero request as zero bytes", value)
		}
	})
}
