// Package httpsecurity supplies shared HTTP credential comparison.
package httpsecurity

import (
	"crypto/sha256"
	"crypto/subtle"
)

// EqualCredential compares fixed-length digests without exposing the stored
// credential length through ConstantTimeCompare's unequal-length fast path.
func EqualCredential(candidate, expected string) bool {
	candidateDigest := sha256.Sum256([]byte(candidate))
	expectedDigest := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(candidateDigest[:], expectedDigest[:]) == 1
}
