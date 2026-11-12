package browser

import (
	"reflect"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestCheckURL(t *testing.T) {
	ok := []string{
		"https://platform.kubernetix.ai/settings?tab=tokens&new=cli",
		"http://localhost:5173/settings",
	}
	bad := []string{"", "-x", "file:///etc/passwd", "javascript:alert(1)", "https://", "ssh://host", "https://a b"}
	for _, u := range ok {
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) = %v", u, err)
		}
	}
	for _, u := range bad {
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q) accepted", u)
		}
	}
}

func TestCandidates(t *testing.T) {
	u := "https://x.test/a?b=1&c=2"
	tests := []struct {
		goos string
		env  map[string]string
		want [][]string
	}{
		{"darwin", nil, [][]string{{"open", u}}},
		{"windows", nil, [][]string{{"rundll32", "url.dll,FileProtocolHandler", u}}},
		{"linux", map[string]string{"DISPLAY": ":0"}, [][]string{{"xdg-open", u}}},
		{"linux", map[string]string{"WSL_DISTRO_NAME": "Ubuntu"}, [][]string{{"wslview", u}, {"rundll32.exe", "url.dll,FileProtocolHandler", u}, {"xdg-open", u}}},
		{"freebsd", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, [][]string{{"xdg-open", u}}},
	}
	for _, tt := range tests {
		if got := candidates(tt.goos, env(tt.env), u); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("candidates(%s, %v) = %v, want %v", tt.goos, tt.env, got, tt.want)
		}
	}
}

func TestCanOpen(t *testing.T) {
	tests := []struct {
		goos string
		env  map[string]string
		want bool
	}{
		{"darwin", nil, true},
		{"windows", nil, true},
		{"darwin", map[string]string{"CI": "true"}, false},
		{"darwin", map[string]string{"GITHUB_ACTIONS": "true"}, false},
		{"darwin", map[string]string{"SSH_CONNECTION": "1.2.3.4 1 5.6.7.8 22"}, false},
		{"linux", nil, false},
		{"linux", map[string]string{"DISPLAY": ":0"}, true},
		{"linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, true},
		{"linux", map[string]string{"WSL_DISTRO_NAME": "Ubuntu"}, true},
	}
	for _, tt := range tests {
		if got := canOpen(tt.goos, env(tt.env)); got != tt.want {
			t.Errorf("canOpen(%s, %v) = %v, want %v", tt.goos, tt.env, got, tt.want)
		}
	}
}
