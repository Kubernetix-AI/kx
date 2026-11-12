package api

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// Shared test vectors: the platform generates tokens with the same checksum.
// They are public test vectors, well formed but never issued, so they unlock
// nothing.
var tokenVectors = []string{
	"kx_0123456789abcdefghijABCDEFGHIJklmnopqrst1zpKRU",
	"kx_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz2x81PZ",
	"kx_00000000000000000000000000000000000000002kaqcA",
	// CRC-32s small enough that the base62 checksum is left-padded with 0.
	"kx_33333333333333333333333333333333333333330oWRgv",
	"kx_000000000000000000000000000000000000035600U3y3",
}

func TestCheckToken_Vectors(t *testing.T) {
	for _, tok := range tokenVectors {
		if err := CheckToken(tok); err != nil {
			t.Errorf("CheckToken(%s) = %v, want nil", TokenPrefix(tok), err)
		}
	}
}

func TestChecksum_Vectors(t *testing.T) {
	want := map[string]string{
		"0123456789abcdefghijABCDEFGHIJklmnopqrst": "1zpKRU",
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz": "2x81PZ",
		"0000000000000000000000000000000000000000": "2kaqcA",
		"3333333333333333333333333333333333333333": "0oWRgv",
		"0000000000000000000000000000000000000356": "00U3y3",
	}
	for body, sum := range want {
		if got := tokenChecksum(body); got != sum {
			t.Errorf("tokenChecksum(%s) = %s, want %s", body, got, sum)
		}
	}
}

func TestCheckToken_Rejects(t *testing.T) {
	good := tokenVectors[0]
	tests := []struct {
		name   string
		tok    string
		legacy bool
	}{
		{"empty", "", false},
		{"no prefix", strings.TrimPrefix(good, "kx_"), false},
		{"wrong prefix", "kz_" + good[3:], false},
		{"short", good[:len(good)-1], false},
		{"long", good + "A", false},
		{"bad checksum", good[:len(good)-1] + "V", false},
		{"typo in body", "kx_1123456789abcdefghijABCDEFGHIJklmnopqrst1zpKRU", false},
		{"symbol", "kx_0123456789abcdefghijABCDEFGHIJklmnopqrs-1zpKRU", false},
		{"surrounding space", " " + good, false},
		{"legacy ct_", "ct_0123456789abcdef0123456789abcdef", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckToken(tt.tok)
			var inv *kxerr.InvalidToken
			if !errors.As(err, &inv) {
				t.Fatalf("CheckToken() = %v, want *kxerr.InvalidToken", err)
			}
			if inv.Legacy != tt.legacy {
				t.Fatalf("Legacy = %v, want %v", inv.Legacy, tt.legacy)
			}
			if strings.Contains(err.Error(), tt.tok) && tt.tok != "" {
				t.Fatal("error message contains the token")
			}
		})
	}
}

func TestTokenPrefix(t *testing.T) {
	if got := TokenPrefix(tokenVectors[0]); got != "kx_01234567" {
		t.Fatalf("TokenPrefix = %q", got)
	}
	// Too short to be a kx_ token, or not one at all: show nothing, so a
	// short legacy key is never printed in full.
	for _, s := range []string{"kx_abc", "ct_12345678", "ct_0123456789abcdef", "hunter2hunter2hunter2"} {
		if got := TokenPrefix(s); got != "" {
			t.Fatalf("TokenPrefix(%d chars) = %q, want empty", len(s), got)
		}
	}
	if got := TokenPrefix(""); got != "" {
		t.Fatalf("TokenPrefix(empty) = %q", got)
	}
}

var tokenRe = regexp.MustCompile(`^kx_[0-9A-Za-z]{46}$`)

func FuzzCheckToken(f *testing.F) {
	for _, tok := range tokenVectors {
		f.Add(tok)
	}
	f.Add("ct_abc")
	f.Add("")
	f.Add("kx_")
	f.Add("kx_\x00\xff")
	f.Fuzz(func(t *testing.T, s string) {
		err := CheckToken(s)
		if err == nil {
			if !tokenRe.MatchString(s) {
				t.Fatalf("accepted a token that does not match the format")
			}
			if tokenChecksum(s[3:43]) != s[43:] {
				t.Fatalf("accepted a token with a bad checksum")
			}
		}
		if p := TokenPrefix(s); len(p) > 11 || (p != "" && !strings.HasPrefix(s, "kx_")) {
			t.Fatalf("TokenPrefix returned %d characters of a non-token", len(p))
		}
	})
}
