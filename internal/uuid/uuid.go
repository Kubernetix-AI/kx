// Package uuid makes the two kinds of UUID kx needs: random (version 4)
// idempotency keys and name-based (version 5, RFC 9562) keys that stay the
// same when a CI job is retried.
package uuid

import (
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // UUIDv5 is defined over SHA-1; it is an identifier, not a signature.
	"encoding/hex"
	"errors"
	"strings"
)

// Well-known namespaces from RFC 9562, and the kx namespace, which is
// V5(NamespaceURL, "https://github.com/Kubernetix-AI/kx").
const (
	NamespaceDNS = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	NamespaceURL = "6ba7b811-9dad-11d1-80b4-00c04fd430c8"
	NamespaceKx  = "9fb82d36-1872-5d09-99d5-6ff40ef899ff"
)

// V4 returns a random UUID.
func V4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return format(b), nil
}

// V5 returns the name-based UUID of name within namespace.
func V5(namespace, name string) (string, error) {
	ns, err := parse(namespace)
	if err != nil {
		return "", err
	}
	h := sha1.New() //nolint:gosec // see import comment
	h.Write(ns[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return format(b), nil
}

func parse(s string) ([16]byte, error) {
	var b [16]byte
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return b, errors.New("uuid: malformed namespace")
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(raw) != 16 {
		return b, errors.New("uuid: malformed namespace")
	}
	copy(b[:], raw)
	return b, nil
}

func format(b [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
