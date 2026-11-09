package httpsecurity

import "testing"

func TestEqualCredential(t *testing.T) {
	for _, tt := range []struct {
		name, candidate, expected string
		want                      bool
	}{
		{"equal", "secret", "secret", true},
		{"same length wrong", "secrex", "secret", false},
		{"short", "sec", "secret", false},
		{"long", "secret-extra", "secret", false},
		{"empty candidate", "", "secret", false},
		{"empty expected", "secret", "", false},
		{"both empty", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := EqualCredential(tt.candidate, tt.expected); got != tt.want {
				t.Fatalf("match = %v, want %v", got, tt.want)
			}
		})
	}
}
