// Package api is the Kubernetix API client kx uses, plus the local token
// check and Explain, which turns any kx error into two lines and an exit
// code.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/kubernetix-ai/kx/internal/version"
)

const (
	defaultTimeout   = 30 * time.Second
	defaultRetries   = 3
	maxResponseBytes = 16 << 20
	maxErrorBytes    = 64 << 10
	maxRetryAfter    = 10 * time.Second
	appsPageSize     = 100
	maxAppsPages     = 50
)

// Client talks to the Kubernetix API. Create it with New, then adjust the
// exported fields before the first request.
type Client struct {
	// HTTPClient sends the requests. New sets a 30-second timeout and
	// refuses redirects, so the token is never sent to another host.
	HTTPClient *http.Client
	// UserAgent defaults to UserAgent(version, "").
	UserAgent string
	// MaxRetries is how many times an idempotent request is retried after
	// a 502, 503 or 504 or a connection failure.
	MaxRetries int
	// Sleep waits between retries; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Ctx is attached to every *Error the client returns.
	Ctx ErrContext

	base    string
	baseErr error
	token   string
}

// New returns a client for the API at baseURL (for example
// https://platform.kubernetix.ai/api/v1) that authenticates with token. A
// base URL that is not https (or http to a loopback address) makes every
// request fail with *kxerr.InsecureURL before anything is sent.
func New(baseURL, token string) *Client {
	base, err := CheckBaseURL(baseURL, "")
	return &Client{
		HTTPClient: &http.Client{
			Timeout: defaultTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		UserAgent:  UserAgent(version.Get().Version, ""),
		MaxRetries: defaultRetries,
		Sleep:      sleepCtx,
		base:       base,
		baseErr:    err,
		token:      token,
	}
}

// UserAgent builds kx/<version> (<os>/<arch>) or, in CI,
// kx/<version> (<os>/<arch>; <CI name>).
func UserAgent(ver, ciName string) string {
	plat := runtime.GOOS + "/" + runtime.GOARCH
	if ciName != "" {
		return fmt.Sprintf("kx/%s (%s; %s)", ver, plat, ciName)
	}
	return fmt.Sprintf("kx/%s (%s)", ver, plat)
}

// BaseURL returns the validated API base URL ("" if it was rejected).
func (c *Client) BaseURL() string { return c.base }

// WhoAmI returns who the token belongs to.
func (c *Client) WhoAmI(ctx context.Context) (*WhoAmI, error) {
	var out WhoAmI
	return &out, c.do(ctx, http.MethodGet, "/whoami", nil, nil, "", &out)
}

// ListApps returns every app in the token's organization.
func (c *Client) ListApps(ctx context.Context) ([]App, error) {
	var all []App
	for page := 0; page < maxAppsPages; page++ {
		var out struct {
			Apps  []App `json:"apps"`
			Total int   `json:"total"`
		}
		q := url.Values{"limit": {strconv.Itoa(appsPageSize)}, "offset": {strconv.Itoa(page * appsPageSize)}}
		if err := c.do(ctx, http.MethodGet, "/apps", q, nil, "", &out); err != nil {
			return nil, err
		}
		all = append(all, out.Apps...)
		if len(out.Apps) < appsPageSize || len(all) >= out.Total {
			break
		}
	}
	return all, nil
}

// GetApp returns one app by ID.
func (c *Client) GetApp(ctx context.Context, appID string) (*App, error) {
	var out App
	return &out, c.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(appID), nil, nil, "", &out)
}

// CreateApp creates an app.
func (c *Client) CreateApp(ctx context.Context, req CreateAppRequest) (*App, error) {
	var out App
	return &out, c.do(ctx, http.MethodPost, "/apps", nil, req, "", &out)
}

// ListDeployments returns an app's newest deployments first (limit 1-100).
func (c *Client) ListDeployments(ctx context.Context, appID string, limit int) ([]Deployment, error) {
	var out struct {
		Deployments []Deployment `json:"deployments"`
	}
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if err := c.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(appID)+"/deployments", q, nil, "", &out); err != nil {
		return nil, err
	}
	return out.Deployments, nil
}

// GetDeployment returns one deployment by ID.
func (c *Client) GetDeployment(ctx context.Context, deploymentID string) (*Deployment, error) {
	var out Deployment
	return &out, c.do(ctx, http.MethodGet, "/deployments/"+url.PathEscape(deploymentID), nil, nil, "", &out)
}

// CreateDeployment starts a deployment. idempotencyKey must be a UUID and
// must be reused when retrying the same deploy; the request is retried
// with it on 502/503/504.
func (c *Client) CreateDeployment(ctx context.Context, appID string, req CreateDeploymentRequest, idempotencyKey string) (*Deployment, error) {
	if idempotencyKey == "" {
		return nil, errors.New("api: CreateDeployment needs an idempotency key")
	}
	var out Deployment
	ans, err := c.send(ctx, http.MethodPost, "/apps/"+url.PathEscape(appID)+"/deployments", nil, req, idempotencyKey, &out)
	// 202 is a new deployment and 200 one made earlier with this key. After
	// a retry, a 200 may be this request's own first try that got through.
	out.Replayed = err == nil && ans.status == http.StatusOK && !ans.retried
	return &out, err
}

// DeploymentLogs returns a deployment's build and rollout logs.
func (c *Client) DeploymentLogs(ctx context.Context, deploymentID string) (*DeploymentLogs, error) {
	var out DeploymentLogs
	return &out, c.do(ctx, http.MethodGet, "/deployments/"+url.PathEscape(deploymentID)+"/logs", nil, nil, "", &out)
}

// Rollback redeploys the given (ready) deployment as a new version.
func (c *Client) Rollback(ctx context.Context, deploymentID, idempotencyKey string) (*Deployment, error) {
	if idempotencyKey == "" {
		return nil, errors.New("api: Rollback needs an idempotency key")
	}
	var out Deployment
	return &out, c.do(ctx, http.MethodPost, "/deployments/"+url.PathEscape(deploymentID)+"/rollback", nil, nil, idempotencyKey, &out)
}

// Replicas returns the app's running instances.
func (c *Client) Replicas(ctx context.Context, appID string) ([]Replica, error) {
	var out []Replica
	err := c.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(appID)+"/replicas", nil, nil, "", &out)
	return out, err
}

// Usage returns the organization's usage against its plan.
func (c *Client) Usage(ctx context.Context, org string) (*Usage, error) {
	var out Usage
	return &out, c.do(ctx, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/usage", nil, nil, "", &out)
}

// RegistryCredentials mints a one-hour push credential for the
// organization's registry. It is rate limited and never retried.
func (c *Client) RegistryCredentials(ctx context.Context) (*RegistryCredential, error) {
	var out RegistryCredential
	return &out, c.do(ctx, http.MethodPost, "/registry/credentials", nil, nil, "", &out)
}

// ListTokens lists org's tokens, only those for appID when it is set.
func (c *Client) ListTokens(ctx context.Context, org, appID string) ([]Token, error) {
	var out struct {
		Tokens []Token `json:"tokens"`
	}
	q := url.Values{}
	if appID != "" {
		q.Set("app_id", appID)
	}
	if err := c.do(ctx, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/tokens", q, nil, "", &out); err != nil {
		return nil, err
	}
	return out.Tokens, nil
}

// RevokeToken revokes a token (and the tokens it created). Revoking an
// already revoked token succeeds.
func (c *Client) RevokeToken(ctx context.Context, org, tokenID string) error {
	return c.do(ctx, http.MethodDelete, "/orgs/"+url.PathEscape(org)+"/tokens/"+url.PathEscape(tokenID), nil, nil, "", nil)
}

// RuntimeLogs returns runtime log lines, oldest first.
func (c *Client) RuntimeLogs(ctx context.Context, appID string, q LogQuery) ([]LogLine, error) {
	v := url.Values{}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if !q.Start.IsZero() {
		v.Set("start", strconv.FormatInt(q.Start.UnixNano(), 10))
	}
	if !q.End.IsZero() {
		v.Set("end", strconv.FormatInt(q.End.UnixNano(), 10))
	}
	if q.Forward {
		v.Set("direction", "forward")
	} else {
		v.Set("direction", "backward")
	}
	var out struct {
		Data struct {
			Result []struct {
				Stream map[string]string   `json:"stream"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(appID)+"/logs/runtime", v, nil, "", &out); err != nil {
		return nil, err
	}
	var lines []LogLine
	for _, stream := range out.Data.Result {
		pod := clean(stream.Stream["pod"], maxNameLen)
		for _, val := range stream.Values {
			if len(val) < 2 {
				continue
			}
			var tsStr, line string
			if json.Unmarshal(val[0], &tsStr) != nil || json.Unmarshal(val[1], &line) != nil {
				continue
			}
			ns, err := strconv.ParseInt(tsStr, 10, 64)
			if err != nil {
				continue
			}
			lines = append(lines, LogLine{Time: time.Unix(0, ns), NS: ns, Line: line, Pod: pod})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].NS < lines[j].NS })
	return lines, nil
}

// do sends one API request, retrying idempotent ones (GET, PUT, DELETE,
// or anything with an Idempotency-Key) on 502, 503, 504 and connection
// failures.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, idempotencyKey string, out any) error {
	_, err := c.send(ctx, method, path, query, body, idempotencyKey, out)
	return err
}

// answer is what send saw of the response that ended a request: its
// status, and whether the request had been sent more than once.
type answer struct {
	status  int
	retried bool
}

// send is do, also reporting the answer.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, body any, idempotencyKey string, out any) (answer, error) {
	if c.baseErr != nil {
		return answer{}, c.baseErr
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return answer{}, err
		}
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	idempotent := method == http.MethodGet || method == http.MethodPut || method == http.MethodDelete || idempotencyKey != ""

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
		if err != nil {
			return answer{}, err
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.UserAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return answer{}, ctxErr
			}
			if idempotent && attempt < c.MaxRetries && !isTimeout(err) {
				if serr := c.Sleep(ctx, backoff(attempt, 0)); serr != nil {
					return answer{}, serr
				}
				continue
			}
			return answer{}, &NetworkError{Err: unwrapURLError(err)}
		}

		if retryable(resp.StatusCode) && idempotent && attempt < c.MaxRetries {
			wait := backoff(attempt, parseRetryAfter(resp.Header.Get("Retry-After")))
			drain(resp)
			if serr := c.Sleep(ctx, wait); serr != nil {
				return answer{}, serr
			}
			continue
		}
		return answer{status: resp.StatusCode, retried: attempt > 0}, c.finish(resp, method, path, out)
	}
}

func (c *Client) finish(resp *http.Response, method, path string, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		e := parseErrorBody(resp.StatusCode, data, resp.Header)
		e.Method, e.Path, e.Ctx = method, path, c.Ctx
		return e
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
	if err := dec.Decode(out); err != nil {
		return &Error{Status: resp.StatusCode, Code: "bad_response", Message: "the response could not be read", Method: method, Path: path, Ctx: c.Ctx}
	}
	return nil
}

func retryable(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// backoff waits 0.5 s, 1 s, 2 s, ... or the server's Retry-After when it
// is longer, capped at 10 s.
func backoff(attempt int, retryAfter time.Duration) time.Duration {
	d := 500 * time.Millisecond << attempt
	if retryAfter > d {
		d = retryAfter
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// unwrapURLError drops the *url.Error wrapper, whose message repeats the
// full request URL.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBytes))
	_ = resp.Body.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
