// Package browser opens web pages in the person's default browser, when
// there is one to open.
package browser

import (
	"errors"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/procenv"
)

// CanOpen reports whether opening a browser makes sense here: never in CI
// or over SSH, and on Linux only with a display (or under WSL).
func CanOpen(getenv func(string) string) bool { return canOpen(runtime.GOOS, getenv) }

// Open opens an http(s) URL in the default browser without waiting for it.
func Open(getenv func(string) string, rawURL string) error {
	if err := checkURL(rawURL); err != nil {
		return err
	}
	for _, c := range candidates(runtime.GOOS, getenv, rawURL) {
		path, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(path, c[1:]...)
		// The browser, and whatever it starts, doesn't need kx's tokens.
		cmd.Env = procenv.Environ()
		if err := cmd.Start(); err != nil {
			continue
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	return errors.New("no browser opener found")
}

func canOpen(goos string, getenv func(string) string) bool {
	if ci.Detect(getenv).IsCI || getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return false
	}
	switch goos {
	case "darwin", "windows":
		return true
	}
	return getenv("WSL_DISTRO_NAME") != "" || getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != ""
}

func candidates(goos string, getenv func(string) string, u string) [][]string {
	switch goos {
	case "darwin":
		return [][]string{{"open", u}}
	case "windows":
		return [][]string{{"rundll32", "url.dll,FileProtocolHandler", u}}
	}
	if getenv("WSL_DISTRO_NAME") != "" {
		return [][]string{{"wslview", u}, {"rundll32.exe", "url.dll,FileProtocolHandler", u}, {"xdg-open", u}}
	}
	return [][]string{{"xdg-open", u}}
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.HasPrefix(raw, "-") {
		return errors.New("browser: refusing to open a non-web URL")
	}
	return nil
}
