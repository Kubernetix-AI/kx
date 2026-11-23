// Package github is kx's GitHub Actions mode (kx deploy --github), which
// the Kubernetix-AI/deploy Action uses. It sets the commit status through
// the job's own GITHUB_TOKEN, writes the job summary and the step outputs,
// and prints workflow commands: ::add-mask:: for the registry credential,
// ::error:: with both lines of a failure, and ::warning:: when GitHub
// can't be told something (a missing permission never stops a deploy).
//
// Workflow command values are escaped (%, CR and LF), and outputs are
// written in the multi-line form with a random delimiter, so text from the
// API or the build can never add an output or a command.
package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/ui"
)

const (
	annotationTitle = "Kubernetix deploy"
	defaultAPIURL   = "https://api.github.com"
	apiVersion      = "2022-11-28"
	maxDescription  = 140 // GitHub's limit for a status description
	requestTimeout  = 15 * time.Second
)

// Commit status states.
const (
	StatePending = "pending"
	StateSuccess = "success"
	StateFailure = "failure"
	StateError   = "error"
)

// Reporter talks to GitHub from inside an Actions job.
type Reporter struct {
	// Out receives workflow commands. kx passes its standard error, so
	// standard output keeps only the URL.
	Out    io.Writer
	Getenv func(string) string
	// HTTPClient sends status requests; New sets a timeout and refuses
	// redirects, so the GitHub token only goes to GITHUB_API_URL.
	HTTPClient *http.Client
	UserAgent  string
	// Environment names the commit status: "Kubernetix / <Environment>".
	Environment string

	mu        sync.Mutex
	statusOff bool
	warned    map[string]bool
}

// New returns a Reporter that prints workflow commands to out.
func New(getenv func(string) string, out io.Writer, environment, userAgent string) *Reporter {
	return &Reporter{
		Out:    out,
		Getenv: getenv,
		HTTPClient: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		UserAgent:   userAgent,
		Environment: environment,
		warned:      map[string]bool{},
	}
}

// Context is the commit status context, e.g. "Kubernetix / production".
func (r *Reporter) Context() string { return "Kubernetix / " + r.Environment }

// EscapeData escapes a workflow command's value.
func EscapeData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

// EscapeProperty escapes a workflow command property such as title.
func EscapeProperty(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(s)
}

func (r *Reporter) command(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = io.WriteString(r.Out, line+"\n")
}

// Mask tells the runner to hide secret in the log from now on. Each line of
// a multi-line value is masked on its own.
func (r *Reporter) Mask(secret string) {
	for _, line := range strings.Split(strings.ReplaceAll(secret, "\r\n", "\n"), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			r.command("::add-mask::" + EscapeData(line))
		}
	}
}

// MaskCredential masks a registry credential's password and the encoded
// login Docker keeps in its settings.
func (r *Reporter) MaskCredential(c api.RegistryCredential) {
	r.Mask(c.Password)
	if c.Password != "" {
		r.Mask(base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password)))
	}
}

// Error prints a failure as one annotation holding both lines.
func (r *Reporter) Error(what, fix string) {
	r.command("::error title=" + EscapeProperty(annotationTitle) + "::" + EscapeData(what+"\n"+fix))
}

// Warning prints msg as an annotation, once per distinct message.
func (r *Reporter) Warning(msg string) {
	r.mu.Lock()
	seen := r.warned[msg]
	r.warned[msg] = true
	r.mu.Unlock()
	if !seen {
		r.command("::warning title=" + EscapeProperty(annotationTitle) + "::" + EscapeData(msg))
	}
}

var (
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaRe  = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

const (
	noTokenWarning    = "kx couldn't set the commit status because GITHUB_TOKEN is empty; the deploy continues. Give the job permissions: statuses: write and pass github-token."
	permissionWarning = "The job's GITHUB_TOKEN can't set commit statuses; the deploy continues. Add statuses: write to the workflow's permissions."
	refusedWarning    = "GitHub didn't accept the job's GITHUB_TOKEN, so kx can't set the commit status; the deploy continues."
	unreachedWarning  = "kx couldn't set the commit status on GitHub just now; the deploy continues."
)

// SetStatus sets the commit status "Kubernetix / <environment>" on the
// commit being deployed. state is pending, success, failure or error; desc
// is cut to GitHub's 140 characters; targetURL is sent only if it is https.
//
// Problems talking to GitHub become a ::warning:: and SetStatus returns
// nil, so the deploy continues; after a missing token or permission it
// stops asking. It returns an error only for an unknown state.
func (r *Reporter) SetStatus(ctx context.Context, state, desc, targetURL string) error {
	switch state {
	case StatePending, StateSuccess, StateFailure, StateError:
	default:
		return fmt.Errorf("github: unknown status state %q", state)
	}
	r.mu.Lock()
	off := r.statusOff
	r.mu.Unlock()
	if off {
		return nil
	}
	token := r.Getenv("GITHUB_TOKEN")
	if token == "" {
		r.stopStatuses(noTokenWarning)
		return nil
	}
	endpoint, err := r.statusURL()
	if err != nil {
		r.stopStatuses("kx couldn't set the commit status because " + err.Error() + "; the deploy continues.")
		return nil
	}

	body := map[string]string{"state": state, "context": r.Context(), "description": description(desc)}
	if u, err := url.Parse(targetURL); err == nil && u.Scheme == "https" && u.Host != "" {
		body["target_url"] = targetURL
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Content-Type", "application/json")
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			r.Warning(unreachedWarning)
		}
		return nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
	case resp.StatusCode == http.StatusUnauthorized:
		r.stopStatuses(refusedWarning)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
		r.stopStatuses(permissionWarning)
	default:
		r.Warning(unreachedWarning)
	}
	return nil
}

func (r *Reporter) stopStatuses(warning string) {
	r.mu.Lock()
	r.statusOff = true
	r.mu.Unlock()
	r.Warning(warning)
}

// statusURL builds the statuses endpoint from GITHUB_API_URL,
// GITHUB_REPOSITORY and GITHUB_SHA, refusing anything unexpected.
func (r *Reporter) statusURL() (string, error) {
	repo, sha := r.Getenv("GITHUB_REPOSITORY"), r.Getenv("GITHUB_SHA")
	if !repoRe.MatchString(repo) || strings.Contains(repo, "..") {
		return "", errors.New("GITHUB_REPOSITORY isn't owner/name")
	}
	if !shaRe.MatchString(sha) {
		return "", errors.New("GITHUB_SHA isn't a commit SHA")
	}
	base := r.Getenv("GITHUB_API_URL")
	if base == "" {
		base = defaultAPIURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname()))) {
		return "", errors.New("GITHUB_API_URL isn't an https address")
	}
	return strings.TrimRight(base, "/") + "/repos/" + repo + "/statuses/" + sha, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// description makes desc one line of at most 140 characters.
func description(desc string) string {
	s := oneLine(desc)
	if utf8.RuneCountInString(s) <= maxDescription {
		return s
	}
	r := []rune(s)
	return string(r[:maxDescription-1]) + "…"
}

// oneLine drops control characters and collapses whitespace.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToValidUTF8(s, ""), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// Outputs are the Action's outputs.
type Outputs struct {
	URL          string
	DeploymentID string
	Version      string // e.g. v15
	Image        string
}

// SetOutputs appends the non-empty outputs to $GITHUB_OUTPUT in the
// name<<DELIMITER form with a fresh random delimiter for each value.
func (r *Reporter) SetOutputs(o Outputs) error {
	path := r.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	var b strings.Builder
	for _, kv := range [][2]string{{"url", o.URL}, {"deployment-id", o.DeploymentID}, {"version", o.Version}, {"image", o.Image}} {
		if kv[1] == "" {
			continue
		}
		delim, err := delimiterFor(kv[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s<<%s\n%s\n%s\n", kv[0], delim, kv[1], delim)
	}
	return appendFile(path, b.String())
}

func delimiterFor(value string) (string, error) {
	for {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		d := "kx_" + hex.EncodeToString(buf)
		if !strings.Contains(value, d) {
			return d, nil
		}
	}
}

func appendFile(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: the runner's own file
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, s); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Summary is what the job summary shows.
type Summary struct {
	App     string
	Version int // 0 if no deployment was created
	URL     string
	Image   string
	Tags    []string // the tags the image was pushed under (image tags, so no | or `)
	Commit  string
	Took    time.Duration
	Started bool // --no-wait: the deploy started and kx didn't wait

	Failed    bool
	What, Fix string
	// Note says what is serving after a failure, when What doesn't, or
	// that the run had already deployed this version.
	Note string
}

// WriteSummary appends the deploy's summary to $GITHUB_STEP_SUMMARY.
func (r *Reporter) WriteSummary(s Summary) error {
	path := r.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	var b strings.Builder
	app := md(s.App)
	switch {
	case s.Failed && s.Version > 0:
		fmt.Fprintf(&b, "### %s v%d didn't go live\n\n", app, s.Version)
	case s.Failed:
		fmt.Fprintf(&b, "### %s wasn't deployed\n\n", orText(app, "The app"))
	case s.Started:
		fmt.Fprintf(&b, "### %s v%d is deploying\n\n", app, s.Version)
	default:
		fmt.Fprintf(&b, "### %s v%d is live\n\n", app, s.Version)
	}
	if s.Failed {
		fmt.Fprintf(&b, "**%s**\n\n%s\n\n", md(s.What), md(s.Fix))
	}
	if s.Note != "" {
		fmt.Fprintf(&b, "%s\n\n", md(s.Note))
	}
	rows := [][2]string{}
	if u := safeURL(s.URL); u != "" {
		rows = append(rows, [2]string{"URL", u})
	}
	if s.Version > 0 {
		rows = append(rows, [2]string{"Version", fmt.Sprintf("v%d", s.Version)})
	}
	if s.Image != "" {
		rows = append(rows, [2]string{"Image", code(s.Image)})
	}
	if len(s.Tags) > 0 {
		shown := make([]string, len(s.Tags))
		for i, t := range s.Tags {
			shown[i] = code(t)
		}
		rows = append(rows, [2]string{"Tags", strings.Join(shown, ", ")})
	}
	if c := s.Commit; c != "" {
		if len(c) > 12 {
			c = c[:12]
		}
		rows = append(rows, [2]string{"Commit", code(c)})
	}
	if s.Took > 0 {
		rows = append(rows, [2]string{"Took", ui.Duration(s.Took)})
	}
	if len(rows) > 0 {
		b.WriteString("| | |\n|---|---|\n")
		for _, row := range rows {
			fmt.Fprintf(&b, "| %s | %s |\n", row[0], row[1])
		}
		b.WriteString("\n")
	}
	if !s.Failed && s.App != "" {
		fmt.Fprintf(&b, "Roll back with `kx rollback --app %s`.\n\n", strings.ReplaceAll(s.App, "`", ""))
	}
	return appendFile(path, b.String())
}

func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// md makes untrusted text safe inside Markdown: one line, HTML and table
// characters escaped.
func md(s string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", "|", "&#124;",
		"`", "'", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`,
	).Replace(oneLine(s))
}

// code shows s in a code span (backticks removed from s).
func code(s string) string { return "`" + strings.ReplaceAll(oneLine(s), "`", "") + "`" }

// safeURL returns u if it is a plain https URL safe to show as-is.
func safeURL(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.Host == "" || strings.ContainsAny(u, " <>|`\"'\\\t\r\n") {
		return ""
	}
	return u
}
