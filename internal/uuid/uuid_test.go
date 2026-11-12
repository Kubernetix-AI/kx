package uuid

import (
	"regexp"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestV5_KnownVectors(t *testing.T) {
	tests := []struct {
		name string
		ns   string
		in   string
		want string
	}{
		{"rfc dns namespace", NamespaceDNS, "python.org", "886313e1-3b8a-5372-9b90-0c9aee199e5d"},
		{"kx namespace derivation", NamespaceURL, "https://github.com/Kubernetix-AI/kx", "9fb82d36-1872-5d09-99d5-6ff40ef899ff"},
		{"kx namespace use", NamespaceKx, "acme/shop-api\x0012345\x002\x00shop-api", "f1498b64-ba27-5f9a-8704-8b6910db9ac2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := V5(tt.ns, tt.in)
			if err != nil {
				t.Fatalf("V5: %v", err)
			}
			if got != tt.want {
				t.Fatalf("V5(%s, %q) = %s, want %s", tt.ns, tt.in, got, tt.want)
			}
		})
	}
}

func TestV5_RejectsBadNamespace(t *testing.T) {
	for _, ns := range []string{"", "not-a-uuid", "886313e1-3b8a-5372-9b90-0c9aee199e5", "886313e13b8a53729b900c9aee199e5d00"} {
		if _, err := V5(ns, "x"); err == nil {
			t.Errorf("V5(%q) returned no error", ns)
		}
	}
}

func TestV4_FormatAndVersion(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u, err := V4()
		if err != nil {
			t.Fatalf("V4: %v", err)
		}
		if !uuidRe.MatchString(u) {
			t.Fatalf("V4() = %q, not a UUID", u)
		}
		if u[14] != '4' {
			t.Fatalf("V4() = %q, version nibble %c", u, u[14])
		}
		if v := u[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Fatalf("V4() = %q, variant nibble %c", u, v)
		}
		if seen[u] {
			t.Fatalf("V4() repeated %q", u)
		}
		seen[u] = true
	}
}
