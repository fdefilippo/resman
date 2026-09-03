package cpupoints

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

type exactResolverFunc func(string) ([]ResolvedUserIdentity, error)

func (f exactResolverFunc) ResolveExactUsername(username string) ([]ResolvedUserIdentity, error) {
	return f(username)
}

func TestPolicyLoaderRejectsUninitializedLoader(t *testing.T) {
	resolver := exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
		return []ResolvedUserIdentity{{Username: username, UID: 1000}}, nil
	})

	for _, loader := range []*PolicyLoader{nil, {}} {
		if _, err := loader.Load(PolicyInputs{}, resolver); err == nil {
			t.Fatal("Load() accepted an uninitialized policy loader")
		}
	}
}

func TestPolicyMapGrammar(t *testing.T) {
	validSpecialNames := "john.smith=100\nDOMAIN\\user=200\nuser@example=300\nhyphen-name=400"
	tests := []struct {
		name    string
		content string
		valid   bool
		entries int
	}{
		{name: "empty map without final newline", content: PolicyMapMarker, valid: true},
		{name: "blank lines comments and final newline", content: PolicyMapMarker + "\n\n# comment with spaces\n", valid: true},
		{name: "CRLF", content: PolicyMapMarker + "\r\nalice=1\r\n", valid: true, entries: 1},
		{name: "complete exact identities", content: PolicyMapMarker + "\n" + validSpecialNames, valid: true, entries: 4},
		{name: "missing marker", content: "alice=1\n", valid: false},
		{name: "blank before marker", content: "\n" + PolicyMapMarker, valid: false},
		{name: "comment before marker", content: "# comment\n" + PolicyMapMarker, valid: false},
		{name: "duplicate marker", content: PolicyMapMarker + "\n" + PolicyMapMarker, valid: false},
		{name: "BOM", content: "\ufeff" + PolicyMapMarker, valid: false},
		{name: "embedded BOM", content: PolicyMapMarker + "\n# \ufeff", valid: false},
		{name: "invalid UTF-8", content: PolicyMapMarker + "\nali\xffce=1", valid: false},
		{name: "lone carriage return", content: PolicyMapMarker + "\ralice=1", valid: false},
		{name: "whitespace-only line", content: PolicyMapMarker + "\n \n", valid: false},
		{name: "indented comment", content: PolicyMapMarker + "\n # comment", valid: false},
		{name: "padded username", content: PolicyMapMarker + "\n alice=1", valid: false},
		{name: "padded points", content: PolicyMapMarker + "\nalice=1 ", valid: false},
		{name: "control-bearing username", content: PolicyMapMarker + "\nal\tice=1", valid: false},
		{name: "empty username", content: PolicyMapMarker + "\n=1", valid: false},
		{name: "missing equals", content: PolicyMapMarker + "\nalice", valid: false},
		{name: "extra equals", content: PolicyMapMarker + "\nalice=1=2", valid: false},
		{name: "empty points", content: PolicyMapMarker + "\nalice=", valid: false},
		{name: "positive sign", content: PolicyMapMarker + "\nalice=+1", valid: false},
		{name: "negative sign", content: PolicyMapMarker + "\nalice=-1", valid: false},
		{name: "leading zero", content: PolicyMapMarker + "\nalice=01", valid: false},
		{name: "zero", content: PolicyMapMarker + "\nalice=0", valid: false},
		{name: "above range", content: PolicyMapMarker + "\nalice=1001", valid: false},
		{name: "duplicate exact name", content: PolicyMapMarker + "\nalice=1\nalice=2", valid: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := parsePolicyMap([]byte(tt.content))
			if tt.valid {
				if err != nil {
					t.Fatalf("parsePolicyMap(): %v", err)
				}
				if len(entries) != tt.entries {
					t.Errorf("entries = %d, want %d", len(entries), tt.entries)
				}
				return
			}
			if err == nil {
				t.Fatalf("parsePolicyMap() accepted %q", tt.content)
			}
		})
	}
}

func TestPolicyMapFirstEqualsPreservesCompleteUsername(t *testing.T) {
	entries, err := parsePolicyMap([]byte(PolicyMapMarker + "\njohn.smith=500"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].username != "john.smith" || entries[0].points.Value() != 500 {
		t.Fatalf("entry = %#v", entries)
	}
}

func TestNSSIdentityResolverUsesTheExactCGOAwareBoundary(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current(): %v", err)
	}
	identities, err := (NSSIdentityResolver{}).ResolveExactUsername(current.Username)
	if err != nil {
		t.Fatalf("ResolveExactUsername(%q): %v", current.Username, err)
	}
	if len(identities) != 1 || identities[0].Username != current.Username {
		t.Fatalf("identities = %#v, want exact username %q", identities, current.Username)
	}
	wantUID, err := strconv.Atoi(current.Uid)
	if err != nil {
		t.Fatalf("current UID %q: %v", current.Uid, err)
	}
	if identities[0].UID != wantUID {
		t.Errorf("UID = %d, want %d", identities[0].UID, wantUID)
	}
}

func TestPolicyLoaderBuildsOneResolvedImmutableSnapshot(t *testing.T) {
	path := writePolicyMap(t, PolicyMapMarker+"\njohn.smith=300\nDOMAIN\\user=200\nuser@example=100\nhyphen-name=50\n")
	inputs := policyInputs(t, path, 100, 100)
	wantUID := map[string]int{
		"john.smith":   1001,
		"DOMAIN\\user": 1002,
		"user@example": 1003,
		"hyphen-name":  1004,
	}
	var resolved []string
	resolver := exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
		resolved = append(resolved, username)
		return []ResolvedUserIdentity{{Username: username, UID: wantUID[username]}}, nil
	})

	snapshot, err := newTestPolicyLoader().Load(inputs, resolver)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := strings.Join(resolved, ","); got != "john.smith,DOMAIN\\user,user@example,hyphen-name" {
		t.Errorf("NSS inputs = %q", got)
	}
	if snapshot.Reserve().Value() != 100 || snapshot.Pool().Value() != 900 || snapshot.BestEffort().Value() != 100 {
		t.Errorf("global points = reserve %d pool %d best-effort %d", snapshot.Reserve().Value(), snapshot.Pool().Value(), snapshot.BestEffort().Value())
	}
	if got := snapshot.ConfiguredGuaranteeTotal().Value(); got != 650 {
		t.Errorf("configured total = %d, want 650", got)
	}
	guarantee, ok := snapshot.GuaranteeForUID(1001)
	if !ok || guarantee.Username() != "john.smith" || guarantee.Points().Value() != 300 || guarantee.SourceLine() != 2 {
		t.Errorf("john.smith guarantee = %#v, found=%t", guarantee, ok)
	}
	if got := snapshot.ClassForUID(1001); got != AllocationClassGuaranteed {
		t.Errorf("mapped class = %q", got)
	}
	if got := snapshot.ClassForUID(9999); got != AllocationClassBestEffort {
		t.Errorf("omitted class = %q", got)
	}
	entries := snapshot.Entries()
	entries[0] = UserGuarantee{}
	if preserved, _ := snapshot.GuaranteeForUID(1001); preserved.Username() != "john.smith" {
		t.Fatal("mutating Entries() changed the immutable snapshot")
	}
	if snapshot.Source().Path().String() != path || snapshot.Source().Size() <= 0 || snapshot.Source().Inode() == 0 {
		t.Errorf("source provenance = path %q size %d inode %d", snapshot.Source().Path().String(), snapshot.Source().Size(), snapshot.Source().Inode())
	}
}

func TestPolicyLoaderConfirmsExactSourceIdentityAndContent(t *testing.T) {
	path := writePolicyMap(t, PolicyMapMarker+"\nalice=300\n")
	loader := newTestPolicyLoader()
	policy, err := loader.Load(policyInputs(t, path, 100, 100), exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
		return []ResolvedUserIdentity{{Username: username, UID: 1000}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := loader.ConfirmSource(policy.Source()); err != nil {
		t.Fatalf("ConfirmSource() baseline: %v", err)
	}
	if err := os.WriteFile(path, []byte(PolicyMapMarker+"\nalice=400\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loader.ConfirmSource(policy.Source()); err == nil || !strings.Contains(err.Error(), "changed after candidate validation") {
		t.Fatalf("ConfirmSource() error = %v, want changed-source rejection", err)
	}
}

func TestPolicyLoaderRejectsIdentityAmbiguityAtomically(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		resolver ExactIdentityResolver
	}{
		{
			name:    "unresolved",
			content: PolicyMapMarker + "\nalice=1",
			resolver: exactResolverFunc(func(string) ([]ResolvedUserIdentity, error) {
				return nil, nil
			}),
		},
		{
			name:    "resolver error",
			content: PolicyMapMarker + "\nalice=1",
			resolver: exactResolverFunc(func(string) ([]ResolvedUserIdentity, error) {
				return nil, errors.New("NSS unavailable")
			}),
		},
		{
			name:    "multiply resolved",
			content: PolicyMapMarker + "\nalice=1",
			resolver: exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
				return []ResolvedUserIdentity{{Username: username, UID: 1001}, {Username: username, UID: 1002}}, nil
			}),
		},
		{
			name:    "normalized instead of exact",
			content: PolicyMapMarker + "\nDOMAIN\\Alice=1",
			resolver: exactResolverFunc(func(string) ([]ResolvedUserIdentity, error) {
				return []ResolvedUserIdentity{{Username: "domain\\alice", UID: 1001}}, nil
			}),
		},
		{
			name:    "negative UID",
			content: PolicyMapMarker + "\nalice=1",
			resolver: exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
				return []ResolvedUserIdentity{{Username: username, UID: -1}}, nil
			}),
		},
		{
			name:    "duplicate UID",
			content: PolicyMapMarker + "\nalice=1\nalice.alias=1",
			resolver: exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
				return []ResolvedUserIdentity{{Username: username, UID: 1001}}, nil
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writePolicyMap(t, tt.content)
			inputs := policyInputs(t, path, 0, 1)
			if _, err := newTestPolicyLoader().Load(inputs, tt.resolver); err == nil {
				t.Fatal("Load() accepted ambiguous identity state")
			}
		})
	}
}

func TestPolicyLoaderReturnsTypedEditorSafeRejectionCauses(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		resolver ExactIdentityResolver
		assert   func(*testing.T, error)
	}{
		{
			name: "malformed map", content: "missing-marker\nalice=1", resolver: resolverByNumericSuffix,
			assert: func(t *testing.T, err error) {
				var typed *PolicySyntaxError
				if !errors.As(err, &typed) {
					t.Fatalf("error = %T %v, want PolicySyntaxError", err, err)
				}
			},
		},
		{
			name: "unresolved username", content: PolicyMapMarker + "\nalice=1",
			resolver: exactResolverFunc(func(string) ([]ResolvedUserIdentity, error) { return nil, nil }),
			assert: func(t *testing.T, err error) {
				var typed *PolicyIdentityError
				if !errors.As(err, &typed) || typed.Username != "alice" {
					t.Fatalf("error = %T %v, username = %q, want PolicyIdentityError for alice", err, err, typedUsername(typed))
				}
			},
		},
		{
			name: "overcommitted guarantees", content: PolicyMapMarker + "\nalice=900",
			resolver: exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
				return []ResolvedUserIdentity{{Username: username, UID: 1001}}, nil
			}),
			assert: func(t *testing.T, err error) {
				var typed *PolicyOvercommitError
				if !errors.As(err, &typed) || typed.Pool != 900 || typed.Guarantees != 900 || typed.BestEffort != 100 {
					t.Fatalf("error = %T %+v, want typed 900+100 overcommit of pool 900", err, typed)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writePolicyMap(t, tt.content)
			_, err := newTestPolicyLoader().Load(policyInputs(t, path, 100, 100), tt.resolver)
			if err == nil {
				t.Fatal("Load() accepted rejected policy")
			}
			tt.assert(t, err)
		})
	}
}

func typedUsername(err *PolicyIdentityError) string {
	if err == nil {
		return ""
	}
	return err.Username
}

func TestPolicyLoaderValidatesTheCompleteCapacityInvariant(t *testing.T) {
	tests := []struct {
		name       string
		reserve    uint64
		bestEffort uint64
		content    string
		valid      bool
		wantTotal  uint64
	}{
		{name: "equality", reserve: 100, bestEffort: 100, content: PolicyMapMarker + "\nalice=400\nbob=400", valid: true, wantTotal: 800},
		{name: "one point excess", reserve: 100, bestEffort: 100, content: PolicyMapMarker + "\nalice=401\nbob=400", valid: false},
		{name: "empty map", reserve: 100, bestEffort: 100, content: PolicyMapMarker + "\n", valid: true, wantTotal: 0},
	}
	resolver := exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
		uid := 1001
		if username == "bob" {
			uid = 1002
		}
		return []ResolvedUserIdentity{{Username: username, UID: uid}}, nil
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writePolicyMap(t, tt.content)
			snapshot, err := newTestPolicyLoader().Load(policyInputs(t, path, tt.reserve, tt.bestEffort), resolver)
			if tt.valid {
				if err != nil {
					t.Fatalf("Load(): %v", err)
				}
				if got := snapshot.ConfiguredGuaranteeTotal().Value(); got != tt.wantTotal {
					t.Errorf("configured total = %d, want %d", got, tt.wantTotal)
				}
				return
			}
			if err == nil {
				t.Fatal("Load() accepted overcommit")
			}
		})
	}
}

func TestPolicyLoaderEnforcesExactByteAndEntryBounds(t *testing.T) {
	t.Run("exact byte maximum", func(t *testing.T) {
		prefix := PolicyMapMarker + "\n#"
		content := prefix + strings.Repeat("x", MaximumPolicyMapBytes-len(prefix))
		path := writePolicyMap(t, content)
		snapshot, err := newTestPolicyLoader().Load(policyInputs(t, path, 0, 1), resolverByNumericSuffix)
		if err != nil {
			t.Fatalf("Load() exact byte maximum: %v", err)
		}
		if snapshot.Source().Size() != MaximumPolicyMapBytes {
			t.Errorf("source size = %d, want %d", snapshot.Source().Size(), MaximumPolicyMapBytes)
		}
	})

	t.Run("one byte excess", func(t *testing.T) {
		prefix := PolicyMapMarker + "\n#"
		content := prefix + strings.Repeat("x", MaximumPolicyMapBytes+1-len(prefix))
		path := writePolicyMap(t, content)
		if _, err := newTestPolicyLoader().Load(policyInputs(t, path, 0, 1), resolverByNumericSuffix); err == nil {
			t.Fatal("Load() accepted one byte over maximum")
		}
	})

	t.Run("exact entry maximum", func(t *testing.T) {
		path := writePolicyMap(t, numberedPolicyMap(MaximumPolicyMapEntries))
		snapshot, err := newTestPolicyLoader().Load(policyInputs(t, path, 0, 1), resolverByNumericSuffix)
		if err != nil {
			t.Fatalf("Load() exact entry maximum: %v", err)
		}
		if got := len(snapshot.Entries()); got != MaximumPolicyMapEntries {
			t.Errorf("entries = %d, want %d", got, MaximumPolicyMapEntries)
		}
	})

	t.Run("one entry excess", func(t *testing.T) {
		path := writePolicyMap(t, numberedPolicyMap(MaximumPolicyMapEntries+1))
		if _, err := newTestPolicyLoader().Load(policyInputs(t, path, 0, 1), resolverByNumericSuffix); err == nil {
			t.Fatal("Load() accepted one entry over maximum")
		}
	})
}

func TestPolicyLoaderRejectsUnsafePathsAndOpenedObjects(t *testing.T) {
	safeContent := PolicyMapMarker + "\n"
	resolver := exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
		return []ResolvedUserIdentity{{Username: username, UID: 1001}}, nil
	})

	t.Run("relative path", func(t *testing.T) {
		if _, err := NewPolicyMapPath("relative.map"); err == nil {
			t.Fatal("relative path accepted")
		}
	})

	t.Run("unclean path", func(t *testing.T) {
		if _, err := NewPolicyMapPath("/etc/resman/../points.map"); err == nil {
			t.Fatal("unclean path accepted")
		}
	})

	t.Run("padded path", func(t *testing.T) {
		if _, err := NewPolicyMapPath(" /etc/resman/cpu-points.map"); err == nil {
			t.Fatal("padded path accepted")
		}
	})

	t.Run("control-bearing path", func(t *testing.T) {
		if _, err := NewPolicyMapPath("/etc/resman/cpu-points.map\n"); err == nil {
			t.Fatal("control-bearing path accepted")
		}
	})

	t.Run("final symlink", func(t *testing.T) {
		dir := privateTestDir(t)
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte(safeContent), 0600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "points.map")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := newTestPolicyLoader().Load(policyInputs(t, link, 0, 1), resolver); err == nil {
			t.Fatal("final symlink accepted")
		}
	})

	t.Run("opened path uses no-follow defense", func(t *testing.T) {
		path := writePolicyMap(t, safeContent)
		loader := newTestPolicyLoader()
		productionOpen := loader.openFile
		loader.openFile = func(name string, flags int, mode os.FileMode) (*os.File, error) {
			if flags&syscall.O_NOFOLLOW == 0 {
				t.Fatal("policy file opened without O_NOFOLLOW")
			}
			return productionOpen(name, flags, mode)
		}
		if _, err := loader.Load(policyInputs(t, path, 0, 1), resolver); err != nil {
			t.Fatalf("Load(): %v", err)
		}
	})

	t.Run("symlink ancestor", func(t *testing.T) {
		root := privateTestDir(t)
		realDir := filepath.Join(root, "real")
		if err := os.Mkdir(realDir, 0700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(realDir, "points.map")
		if err := os.WriteFile(target, []byte(safeContent), 0600); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(root, "linked")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(linkDir, "points.map")
		if _, err := newTestPolicyLoader().Load(policyInputs(t, path, 0, 1), resolver); err == nil {
			t.Fatal("symlink ancestor accepted")
		}
	})

	t.Run("unsafe mode", func(t *testing.T) {
		path := writePolicyMap(t, safeContent)
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := newTestPolicyLoader().Load(policyInputs(t, path, 0, 1), resolver); err == nil {
			t.Fatal("mode 0644 accepted")
		}
	})

	t.Run("non-regular object", func(t *testing.T) {
		path := filepath.Join(privateTestDir(t), "points.map")
		if err := os.Mkdir(path, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newTestPolicyLoader().Load(policyInputs(t, path, 0, 1), resolver); err == nil {
			t.Fatal("directory accepted as policy file")
		}
	})

	t.Run("writable ancestor", func(t *testing.T) {
		root := privateTestDir(t)
		unsafeDir := filepath.Join(root, "unsafe")
		if err := os.Mkdir(unsafeDir, 0777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(unsafeDir, 0777); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(unsafeDir, "points.map")
		if err := os.WriteFile(path, []byte(safeContent), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newTestPolicyLoader().Load(policyInputs(t, path, 0, 1), resolver); err == nil {
			t.Fatal("group/other-writable ancestor accepted")
		}
	})

	t.Run("untrusted file owner", func(t *testing.T) {
		path := writePolicyMap(t, safeContent)
		loader := newTestPolicyLoader()
		originalOwner := loader.ownerUID
		loader.ownerUID = func(info os.FileInfo) (int, error) {
			if info.Name() == filepath.Base(path) {
				return loader.effectiveUID() + 1, nil
			}
			return originalOwner(info)
		}
		if _, err := loader.Load(policyInputs(t, path, 0, 1), resolver); err == nil {
			t.Fatal("untrusted file owner accepted")
		}
	})

	t.Run("file modified between opened-object inspections", func(t *testing.T) {
		path := writePolicyMap(t, safeContent)
		loader := newTestPolicyLoader()
		productionOwner := loader.ownerUID
		modified := false
		loader.ownerUID = func(info os.FileInfo) (int, error) {
			owner, err := productionOwner(info)
			if err != nil || modified || info.Name() != filepath.Base(path) {
				return owner, err
			}
			modified = true
			if err := os.WriteFile(path, []byte(PolicyMapMarker+"\nalice=1\n"), 0600); err != nil {
				return 0, fmt.Errorf("mutate policy fixture: %w", err)
			}
			return owner, nil
		}

		_, err := loader.Load(policyInputs(t, path, 0, 1), resolver)
		if !modified {
			t.Fatal("test did not modify the policy file during loading")
		}
		if err == nil || !strings.Contains(err.Error(), "changed while it was being read") {
			t.Fatalf("Load() error = %v, want concurrent-modification rejection", err)
		}
	})
}

func TestFailedPolicyLoadCannotMutateAPreviousSnapshot(t *testing.T) {
	path := writePolicyMap(t, PolicyMapMarker+"\nalice=300")
	resolver := exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
		return []ResolvedUserIdentity{{Username: username, UID: 1001}}, nil
	})
	inputs := policyInputs(t, path, 100, 100)
	previous, err := newTestPolicyLoader().Load(inputs, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(PolicyMapMarker+"\nalice=801"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newTestPolicyLoader().Load(inputs, resolver); err == nil {
		t.Fatal("overcommitted replacement accepted")
	}
	guarantee, ok := previous.GuaranteeForUID(1001)
	if !ok || guarantee.Points().Value() != 300 || previous.ConfiguredGuaranteeTotal().Value() != 300 {
		t.Fatalf("previous snapshot changed after failed load: %#v", previous)
	}
}

func TestPolicySnapshotSupportsConcurrentReadOnlyConsumers(t *testing.T) {
	path := writePolicyMap(t, PolicyMapMarker+"\nalice=300\nbob=200")
	resolver := exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
		uid := 1001
		if username == "bob" {
			uid = 1002
		}
		return []ResolvedUserIdentity{{Username: username, UID: uid}}, nil
	})
	snapshot, err := newTestPolicyLoader().Load(policyInputs(t, path, 100, 100), resolver)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 100; iteration++ {
				_ = snapshot.Entries()
				_, _ = snapshot.GuaranteeForUID(1001)
				_ = snapshot.ClassForUID(9999)
			}
		}()
	}
	wg.Wait()
}

func FuzzParsePolicyMap(f *testing.F) {
	for _, seed := range []string{
		PolicyMapMarker,
		PolicyMapMarker + "\nalice=1",
		PolicyMapMarker + "\r\njohn.smith=500\r\n",
		"\ufeff" + PolicyMapMarker,
		PolicyMapMarker + "\nalice=01",
		PolicyMapMarker + "\nalice=1=2",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := parsePolicyMap(data)
		if err != nil {
			return
		}
		if len(entries) > MaximumPolicyMapEntries {
			t.Fatalf("successful parse returned %d entries", len(entries))
		}
		seen := make(map[string]bool, len(entries))
		for _, entry := range entries {
			if entry.username == "" || seen[entry.username] {
				t.Fatalf("successful parse returned invalid username %q", entry.username)
			}
			seen[entry.username] = true
			if entry.points.Value() < 1 || entry.points.Value() > 1000 {
				t.Fatalf("successful parse returned points %d", entry.points.Value())
			}
		}
	})
}

func writePolicyMap(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(privateTestDir(t), "cpu-points.map")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write policy map: %v", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("chmod policy map: %v", err)
	}
	return path
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, path := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatalf("secure test directory %s: %v", path, err)
		}
	}
	return dir
}

func newTestPolicyLoader() *PolicyLoader {
	loader := NewPolicyLoader()
	productionOwner := loader.ownerUID
	loader.ownerUID = func(info os.FileInfo) (int, error) {
		// The managed test sandbox exposes /tmp as a sticky directory owned by
		// nobody. Production Linux hosts use root ownership; simulate only that
		// environmental boundary and leave every descendant check unchanged.
		if info.Name() == "tmp" && info.Mode()&os.ModeSticky != 0 {
			return 0, nil
		}
		return productionOwner(info)
	}
	return loader
}

func policyInputs(t *testing.T, path string, reserveValue, bestEffortValue uint64) PolicyInputs {
	t.Helper()
	reserve, err := NewReservePoints(reserveValue)
	if err != nil {
		t.Fatal(err)
	}
	bestEffort, err := NewBestEffortPoints(bestEffortValue)
	if err != nil {
		t.Fatal(err)
	}
	mapPath, err := NewPolicyMapPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return PolicyInputs{Reserve: reserve, BestEffort: bestEffort, MapPath: mapPath}
}

func numberedPolicyMap(entries int) string {
	var builder strings.Builder
	builder.WriteString(PolicyMapMarker)
	for index := 0; index < entries; index++ {
		fmt.Fprintf(&builder, "\nuser%d=1", index)
	}
	return builder.String()
}

var resolverByNumericSuffix = exactResolverFunc(func(username string) ([]ResolvedUserIdentity, error) {
	value := strings.TrimPrefix(username, "user")
	if value == username {
		return nil, fmt.Errorf("unexpected username %q", username)
	}
	uid, err := strconv.Atoi(value)
	if err != nil {
		return nil, err
	}
	return []ResolvedUserIdentity{{Username: username, UID: 10000 + uid}}, nil
})
