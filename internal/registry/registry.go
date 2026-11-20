// Package registry asks a container registry what an image tag holds, over
// the registry HTTP API, with the same short-lived credential kx pushes
// with. kx deploy uses it so that it never moves a version tag that already
// holds another image.
//
// The credential only ever goes to the registry's own host over HTTPS: a
// token service on another host, or without TLS, is refused. The token kx
// asks for can only read the one repository.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
)

// ErrUnauthorized means the registry refused the credential.
var ErrUnauthorized = errors.New("the registry refused the credential")

// manifestTypes are the manifests a tag can hold. Asking for all of them
// makes the registry report the digest of what the tag holds, not of a
// converted manifest.
const manifestTypes = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

const (
	timeout          = 30 * time.Second
	maxManifestBytes = 4 << 20
	maxTokenBytes    = 1 << 20
	maxDrainBytes    = 64 << 10
)

var (
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	repoRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Client asks a registry about tags. The zero value is ready to use.
type Client struct {
	// HTTP sends the requests; nil means a client with a 30-second
	// timeout. Redirects are never followed either way.
	HTTP *http.Client
}

// Digest returns the digest ("sha256:…") of what the tag in ref
// (host/repository:tag) holds, or "" when the registry has no such tag.
func (c Client) Digest(ctx context.Context, ref string, cred api.RegistryCredential) (string, error) {
	host, repo, tag, ok := splitRef(ref)
	if !ok {
		return "", fmt.Errorf("registry: %q is not an image name with a tag", ref)
	}
	if cred.Username == "" || cred.Password == "" {
		return "", errors.New("registry: the credential is incomplete")
	}
	if !strings.EqualFold(cred.Host(), host) {
		return "", errors.New("registry: the credential is for another registry")
	}
	hc := c.client()
	manifest := "https://" + host + "/v2/" + repo + "/manifests/" + tag

	// The registry's base answers without the credential how to
	// authenticate (or that nothing is needed). Asking the base, not the
	// tag, means a registry that hides private repositories from anonymous
	// requests (a 404, not a 401) can't make a tag look free.
	a, err := sendTwice(ctx, hc, http.MethodGet, "https://"+host+"/v2/", "")
	if err != nil {
		return "", err
	}
	auth := ""
	switch a.status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		if auth, err = authorize(ctx, hc, host, repo, a.header.Get("WWW-Authenticate"), cred); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("registry: %s answered %d when kx asked how to log in", host, a.status)
	}
	if a, err = sendTwice(ctx, hc, http.MethodHead, manifest, auth); err != nil {
		return "", err
	}
	switch a.status {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrUnauthorized
	default:
		return "", fmt.Errorf("registry: %s answered %d when kx asked about the tag %s", host, a.status, tag)
	}
	if d := a.header.Get("Docker-Content-Digest"); d != "" {
		if !digestRe.MatchString(d) {
			return "", fmt.Errorf("registry: %s reported a digest kx can't read", host)
		}
		return d, nil
	}
	// No digest in the answer: fetch the manifest and hash it.
	if a, err = sendTwice(ctx, hc, http.MethodGet, manifest, auth); err != nil {
		return "", err
	}
	switch a.status {
	case http.StatusOK:
		sum := sha256.Sum256(a.body)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	case http.StatusNotFound:
		return "", nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrUnauthorized
	}
	return "", fmt.Errorf("registry: %s answered %d when kx asked about the tag %s", host, a.status, tag)
}

func (c Client) client() *http.Client {
	hc := &http.Client{Timeout: timeout}
	if c.HTTP != nil {
		copied := *c.HTTP
		hc = &copied
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return hc
}

// splitRef splits host/repository:tag.
func splitRef(ref string) (host, repo, tag string, ok bool) {
	host, rest, found := strings.Cut(ref, "/")
	if !found || !hostRe.MatchString(host) {
		return "", "", "", false
	}
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return "", "", "", false
	}
	repo, tag = rest[:i], rest[i+1:]
	if !repoRe.MatchString(repo) || !tagRe.MatchString(tag) {
		return "", "", "", false
	}
	return host, repo, tag, true
}

// authorize answers the registry's challenge: Basic sends the credential
// as it is; Bearer exchanges it at the token service for a token that can
// read repo.
func authorize(ctx context.Context, hc *http.Client, host, repo, challenge string, cred api.RegistryCredential) (string, error) {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+cred.Password))
	scheme, params := parseChallenge(challenge)
	switch scheme {
	case "basic":
		return basic, nil
	case "bearer":
	default:
		return "", fmt.Errorf("registry: %s asked for a login kx doesn't know", host)
	}
	// The credential goes to the registry's own host over TLS, nowhere else.
	realm, err := url.Parse(params["realm"])
	switch {
	case err != nil || !strings.EqualFold(realm.Host, host) || realm.User != nil:
		return "", fmt.Errorf("registry: %s sent kx to a token service on another address", host)
	case realm.Scheme != "https":
		return "", fmt.Errorf("registry: %s sent kx to a token service without TLS", host)
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+repo+":pull")
	realm.RawQuery = q.Encode()

	a, err := sendTwice(ctx, hc, http.MethodGet, realm.String(), basic)
	if err != nil {
		return "", err
	}
	switch a.status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrUnauthorized
	default:
		return "", fmt.Errorf("registry: %s's token service answered %d", host, a.status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(a.body, &tok); err != nil {
		return "", fmt.Errorf("registry: %s's token service answered with something kx can't read", host)
	}
	t := tok.Token
	if t == "" {
		t = tok.AccessToken
	}
	if t == "" || strings.ContainsAny(t, "\r\n") {
		return "", fmt.Errorf("registry: %s's token service gave no token", host)
	}
	return "Bearer " + t, nil
}

// parseChallenge reads a WWW-Authenticate value such as
// Bearer realm="https://host/v2/token",service="host": the scheme in
// lowercase and its parameters.
func parseChallenge(h string) (string, map[string]string) {
	h = strings.TrimSpace(h)
	scheme, rest, _ := strings.Cut(h, " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, found := strings.Cut(rest, "=")
		if !found {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var val strings.Builder
		after = strings.TrimLeft(after, " ")
		if strings.HasPrefix(after, `"`) {
			i := 1
			for ; i < len(after) && after[i] != '"'; i++ {
				if after[i] == '\\' && i+1 < len(after) {
					i++
				}
				val.WriteByte(after[i])
			}
			rest = after[min(i+1, len(after)):]
		} else {
			v, r, _ := strings.Cut(after, ",")
			val.WriteString(strings.TrimSpace(v))
			rest = "," + r
		}
		params[key] = val.String()
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
	}
	return strings.ToLower(scheme), params
}

// answer is what kx keeps of a registry response.
type answer struct {
	status int
	header http.Header
	body   []byte // GET 200 only
}

// sendTwice sends the request again, once, after a network failure or a
// server error.
func sendTwice(ctx context.Context, hc *http.Client, method, u, auth string) (answer, error) {
	a, err := send(ctx, hc, method, u, auth)
	if (err == nil && a.status < 500) || ctx.Err() != nil {
		return a, err
	}
	return send(ctx, hc, method, u, auth)
}

func send(ctx context.Context, hc *http.Client, method, u, auth string) (answer, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return answer{}, err
	}
	req.Header.Set("Accept", manifestTypes)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return answer{}, ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL adds nothing the caller doesn't know
		}
		return answer{}, fmt.Errorf("registry: %s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	a := answer{status: resp.StatusCode, header: resp.Header}
	if method == http.MethodGet && resp.StatusCode == http.StatusOK {
		limit := int64(maxManifestBytes)
		if strings.HasSuffix(req.URL.Path, "/v2/token") {
			limit = maxTokenBytes
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return answer{}, fmt.Errorf("registry: %s: %w", req.URL.Host, err)
		}
		if int64(len(body)) > limit {
			return answer{}, fmt.Errorf("registry: %s answered with more than kx reads", req.URL.Host)
		}
		a.body = body
		return a, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	return a, nil
}
