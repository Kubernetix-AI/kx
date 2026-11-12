package api

import (
	"hash/crc32"
	"strings"

	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// A Kubernetix API token is "kx_" + 40 base62 characters + a 6-character
// base62 CRC-32 (IEEE) of those 40, most significant digit first.
const (
	tokenPrefix    = "kx_"
	tokenBodyLen   = 40
	tokenSumLen    = 6
	tokenLen       = len(tokenPrefix) + tokenBodyLen + tokenSumLen
	tokenShowLen   = 11 // "kx_" + 8, the part kx may print
	base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// CheckToken validates a token's format and checksum without any network
// call. It returns a *kxerr.InvalidToken (Legacy set for ct_ keys), whose
// message never contains the token.
func CheckToken(tok string) error {
	if strings.HasPrefix(tok, "ct_") {
		return &kxerr.InvalidToken{Legacy: true, Reason: "legacy key"}
	}
	if len(tok) != tokenLen || !strings.HasPrefix(tok, tokenPrefix) {
		return &kxerr.InvalidToken{Reason: "wrong length or prefix"}
	}
	for i := len(tokenPrefix); i < len(tok); i++ {
		if strings.IndexByte(base62Alphabet, tok[i]) < 0 {
			return &kxerr.InvalidToken{Reason: "unexpected character"}
		}
	}
	body := tok[len(tokenPrefix) : len(tokenPrefix)+tokenBodyLen]
	if tokenChecksum(body) != tok[len(tokenPrefix)+tokenBodyLen:] {
		return &kxerr.InvalidToken{Reason: "checksum mismatch"}
	}
	return nil
}

// TokenPrefix returns the part of a token kx is allowed to show: "kx_" and
// the next 8 characters. For anything that is not plausibly a kx_ token
// (including short legacy keys) it returns "", so nothing is shown.
func TokenPrefix(tok string) string {
	if !strings.HasPrefix(tok, tokenPrefix) || len(tok) < 2*tokenShowLen {
		return ""
	}
	return tok[:tokenShowLen]
}

func tokenChecksum(body string) string {
	n := crc32.ChecksumIEEE([]byte(body))
	var out [tokenSumLen]byte
	for i := tokenSumLen - 1; i >= 0; i-- {
		out[i] = base62Alphabet[n%62]
		n /= 62
	}
	return string(out[:])
}
