package billing

import (
	"crypto/sha256"
	"encoding/json"
)

// ReserveDigest identifies the content of a reserve request, apart from its
// (call_id, attempt_id) key. Two requests with the same key are the same call only
// when their digests are equal.
func ReserveDigest(tenantID, priceVersionID string, b Bounds) []byte {
	return digest(tenantID, priceVersionID, b.MaxInput, b.MaxOutput)
}

// SettleDigest identifies the content of a settle request. An unknown count and a
// count of 0 give different digests.
func SettleDigest(o Outcome, u Usage) []byte {
	return digest(string(o), u.Input, u.Output)
}

// digest hashes the JSON array of the fields, so it depends on their values and
// order only, never on how the request was written.
func digest(fields ...any) []byte {
	encoded, err := json.Marshal(fields)
	if err != nil {
		panic("billing: digest fields must encode as JSON: " + err.Error())
	}
	sum := sha256.Sum256(encoded)
	return sum[:]
}
