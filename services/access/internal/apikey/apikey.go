// Package apikey holds how gateway keys are stored: never raw, only as an HMAC keyed
// with ACCESS_KEY_PEPPER, so a copy of the database does not reveal a usable key.
package apikey

import (
	"crypto/hmac"
	"crypto/sha256"
)

// HMAC returns HMAC-SHA256(pepper, key), the value kept in api_keys.key_hmac.
func HMAC(pepper []byte, key string) []byte {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(key))
	return mac.Sum(nil)
}
