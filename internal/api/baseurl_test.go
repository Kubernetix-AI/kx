package api

import (
	"errors"
	"testing"

	"github.com/kubernetix-ai/kx/internal/kxerr"
)

func TestCheckBaseURL(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
		reason   string
	}{
		{"https://platform.kubernetix.ai/api/v1", "https://platform.kubernetix.ai/api/v1", true, ""},
		{"https://platform.kubernetix.ai/api/v1/", "https://platform.kubernetix.ai/api/v1", true, ""},
		{"http://localhost:8080/api/v1", "http://localhost:8080/api/v1", true, ""},
		{"http://127.0.0.1:9999/api/v1", "http://127.0.0.1:9999/api/v1", true, ""},
		{"http://[::1]:9999/api/v1", "http://[::1]:9999/api/v1", true, ""},
		{"http://platform.kubernetix.ai/api/v1", "", false, kxerr.URLNotHTTPS},
		{"http://localhost.evil.example/api/v1", "", false, kxerr.URLNotHTTPS},
		{"http://127.0.0.1.nip.io/api/v1", "", false, kxerr.URLNotHTTPS},
		{"ftp://platform.kubernetix.ai/api/v1", "", false, kxerr.URLNotHTTPS},
		{"https://user:pass@platform.kubernetix.ai/api/v1", "", false, kxerr.URLCredentials},
		{"https://platform.kubernetix.ai/api/v1?x=1", "", false, kxerr.URLQueryOrFragment},
		{"https://platform.kubernetix.ai/api/v1#frag", "", false, kxerr.URLQueryOrFragment},
		{"https:///api/v1", "", false, kxerr.URLInvalid},
		{"platform.kubernetix.ai", "", false, kxerr.URLInvalid},
		{"", "", false, kxerr.URLInvalid},
		{"https://plat form.kubernetix.ai", "", false, kxerr.URLInvalid},
	}
	for _, tt := range tests {
		got, err := CheckBaseURL(tt.in, "KX_API_URL")
		if tt.ok {
			if err != nil || got != tt.want {
				t.Errorf("CheckBaseURL(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
			continue
		}
		var ins *kxerr.InsecureURL
		if !errors.As(err, &ins) {
			t.Errorf("CheckBaseURL(%q) = %q, %v; want *kxerr.InsecureURL", tt.in, got, err)
			continue
		}
		if ins.Source != "KX_API_URL" || ins.Reason != tt.reason {
			t.Errorf("CheckBaseURL(%q): Source = %q, Reason = %q; want KX_API_URL, %q", tt.in, ins.Source, ins.Reason, tt.reason)
		}
	}
}
