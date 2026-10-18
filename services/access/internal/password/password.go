// Package password hashes and checks console user passwords with salted
// PBKDF2-SHA256 from the standard library. A hash is stored as
// "pbkdf2-sha256$<iterations>$<salt>$<key>", with base64url parts.
package password

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Iterations is the PBKDF2 work factor of new hashes.
const Iterations = 210_000

const (
	scheme  = "pbkdf2-sha256"
	saltLen = 16
	keyLen  = 32
)

// ErrMalformed means a stored hash is not in the expected format.
var ErrMalformed = errors.New("password: malformed hash")

// Hash returns the stored form of password, with a new random salt.
func Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, Iterations, keyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return fmt.Sprintf("%s$%d$%s$%s", scheme, Iterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// Check reports whether password matches stored. A malformed stored hash is an
// error, never a match.
func Check(stored, password string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != scheme {
		return false, ErrMalformed
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 || iterations > 10_000_000 {
		return false, ErrMalformed
	}
	enc := base64.RawURLEncoding
	salt, err := enc.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false, ErrMalformed
	}
	want, err := enc.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false, ErrMalformed
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
