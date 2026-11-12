package api

import (
	"net"
	"net/url"
	"strings"

	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// CheckBaseURL validates an API base URL and returns it without a trailing
// slash. Only https is accepted, except plain http to a loopback address
// for local development. source names where the URL came from, for the
// error message.
func CheckBaseURL(raw, source string) (string, error) {
	bad := func(reason string) error { return &kxerr.InsecureURL{URL: raw, Source: source, Reason: reason} }
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.Opaque != "":
		return "", bad(kxerr.URLInvalid)
	case u.User != nil:
		return "", bad(kxerr.URLCredentials)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", bad(kxerr.URLQueryOrFragment)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", bad(kxerr.URLNotHTTPS)
		}
	default:
		return "", bad(kxerr.URLNotHTTPS)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
