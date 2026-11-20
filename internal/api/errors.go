package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/safetext"
)

// ErrContext is what kx knew when a request failed. Explain uses it to
// name the org and app and to pick the right fix for the token's source.
type ErrContext struct {
	Org         string
	App         string
	TokenSource kxerr.TokenSource
}

// Error is an error response from the Kubernetix API.
//
// The API's error body is {"error": "<code>", "message": "<text>", ...};
// token failures add "token_prefix", "token_name" and sometimes
// "expires_at", plan limits add "limit_name", and billing blocks may add
// "reason". The ceiling on deploys started a day adds "reason":
// "too_many_tries" and may add "tries_limit". Every string is cleaned of
// control characters and anything that looks like a token.
type Error struct {
	Status         int
	Code           string
	Message        string
	RetryAfter     time.Duration
	TokenPrefix    string
	TokenName      string
	TokenExpiresAt *time.Time
	LimitName      string
	Reason         string // what lifts a billing block, e.g. outstanding_invoice; or too_many_tries
	TriesLimit     int    // with too_many_tries: the deploys a day the org may start, failed ones included (0 if not given)
	Method         string
	Path           string // relative to the API base, e.g. /registry/credentials
	Ctx            ErrContext
}

func (e *Error) Error() string {
	code := e.Code
	if code == "" {
		code = "error"
	}
	if e.Message == "" {
		return fmt.Sprintf("%s %s: %s (status %d)", e.Method, e.Path, code, e.Status)
	}
	return fmt.Sprintf("%s %s: %s: %s (status %d)", e.Method, e.Path, code, e.Message, e.Status)
}

// NetworkError means the request never got an HTTP answer.
type NetworkError struct{ Err error }

func (e *NetworkError) Error() string { return "network error: " + redactTokens(e.Err.Error()) }
func (e *NetworkError) Unwrap() error { return e.Err }

const (
	maxTriesLimit = 1_000_000
	maxMessageLen = 300
	maxCodeLen    = 64
	maxNameLen    = 64
)

// parseErrorBody turns a non-2xx response into an *Error. It never fails:
// a body that is not the API's JSON shape gives an Error with an empty Code
// and the (cleaned) start of the body as its message.
func parseErrorBody(status int, body []byte, h http.Header) *Error {
	e := &Error{Status: status, RetryAfter: parseRetryAfter(h.Get("Retry-After"))}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		e.Message = clean(string(body), maxMessageLen)
		return e
	}
	str := func(key string) string {
		var s string
		if v, ok := raw[key]; ok && json.Unmarshal(v, &s) == nil {
			return s
		}
		return ""
	}
	e.Code = cleanCode(str("error"))
	e.Message = clean(str("message"), maxMessageLen)
	e.TokenName = clean(str("token_name"), maxNameLen)
	if p := str("token_prefix"); tokenPrefixRe.MatchString(p) {
		e.TokenPrefix = p
	}
	e.LimitName = cleanCode(str("limit_name"))
	e.Reason = cleanCode(str("reason"))
	var tries float64
	if v, ok := raw["tries_limit"]; ok && json.Unmarshal(v, &tries) == nil && tries >= 1 && tries <= maxTriesLimit && tries == math.Trunc(tries) {
		e.TriesLimit = int(tries)
	}
	if t, err := time.Parse(time.RFC3339, str("expires_at")); err == nil {
		e.TokenExpiresAt = &t
	}
	return e
}

var (
	tokenPrefixRe = regexp.MustCompile(`^kx_[0-9A-Za-z]{8}$`)
	codeRe        = regexp.MustCompile(`^[a-z0-9_]+$`)
	fullTokenRe   = regexp.MustCompile(`kx_[0-9A-Za-z]{40,}`)
	legacyTokenRe = regexp.MustCompile(`ct_[0-9A-Za-z]{6,}`)
)

func cleanCode(s string) string {
	if len(s) > maxCodeLen || !codeRe.MatchString(s) {
		return ""
	}
	return s
}

// clean makes server text safe to print: valid UTF-8, no control or
// format characters (so no terminal escapes or bidi overrides), single
// spaces, tokens redacted, at most max bytes.
func clean(s string, max int) string {
	s = redactTokens(strings.ToValidUTF8(s, ""))
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == utf8.RuneError {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := safetext.Neutralize(b.String())
	if len(out) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + "…"
	}
	return out
}

func redactTokens(s string) string {
	s = fullTokenRe.ReplaceAllStringFunc(s, func(t string) string { return t[:11] + "…" })
	return legacyTokenRe.ReplaceAllString(s, "ct_…")
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 && secs <= 24*3600 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 && d <= 24*time.Hour {
			return d.Round(time.Second)
		}
	}
	return 0
}
