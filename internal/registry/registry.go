// Package registry talks to a container registry over the registry HTTP
// API, with the same short-lived credential kx pushes with: it looks up
// what a tag holds (the image's digest and labels), and points a new tag at
// an image the repository already has. kx deploy uses it so that it never
// moves a version tag to another image, and so that it can deploy the image
// it built for a commit before instead of building it again.
//
// The credential only ever goes to the registry's own host over HTTPS: a
// token service on another host, or without TLS, is refused, and a blob
// download on another host gets no credential. Each token kx asks for
// covers the one repository: pull to look, pull and push to tag.
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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
	maxRedirects     = 5
)

var (
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	repoRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Image is what a tag holds: the digest of its manifest (or index), and
// the labels of its linux/amd64 image. Digest is "" when there is no such
// tag; Labels is empty when the image has none.
type Image struct {
	Digest string
	Labels map[string]string
}

// Client talks to registries. The zero value is ready to use.
type Client struct {
	// HTTP sends the requests; nil means a client with a 30-second
	// timeout. Redirects are only followed for blob downloads either way.
	HTTP *http.Client
}

// Lookup returns what the tag in ref (host/repository:tag) holds.
func (c Client) Lookup(ctx context.Context, ref string, cred api.RegistryCredential) (Image, error) {
	host, repo, tag, ok := splitRef(ref)
	if !ok {
		return Image{}, fmt.Errorf("registry: %q is not an image name with a tag", ref)
	}
	s, err := c.open(ctx, host, repo, "pull", cred)
	if err != nil {
		return Image{}, err
	}
	digest, err := s.digest(ctx, tag)
	if err != nil || digest == "" {
		return Image{}, err
	}
	labels, err := s.labels(ctx, digest)
	if err != nil {
		return Image{}, err
	}
	return Image{Digest: digest, Labels: labels}, nil
}

// Tag points tag at the image digest that the repository repo
// (host/repository) already holds, without uploading anything.
func (c Client) Tag(ctx context.Context, repo, digest, tag string, cred api.RegistryCredential) error {
	host, path, found := strings.Cut(repo, "/")
	if !found || !hostRe.MatchString(host) || !repoRe.MatchString(path) || !digestRe.MatchString(digest) || !tagRe.MatchString(tag) {
		return fmt.Errorf("registry: can't tag %s@%s as %q", repo, digest, tag)
	}
	s, err := c.open(ctx, host, path, "pull,push", cred)
	if err != nil {
		return err
	}
	body, mediaType, err := s.manifest(ctx, digest)
	if err != nil {
		return err
	}
	if body == nil {
		return fmt.Errorf("registry: %s has no image %s", repo, digest)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.url("manifests/"+tag), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mediaType)
	if s.auth != "" {
		req.Header.Set("Authorization", s.auth)
	}
	a, err := do(ctx, s.hc, req)
	if err != nil {
		return err
	}
	switch a.status {
	case http.StatusOK, http.StatusCreated:
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	default:
		return fmt.Errorf("registry: %s answered %d when kx tagged %s", host, a.status, tag)
	}
	if d := a.header.Get("Docker-Content-Digest"); d != "" && d != digest {
		return fmt.Errorf("registry: %s tagged another image as %s", host, tag)
	}
	return nil
}

// session is one repository, with the credential exchanged for what the
// registry wants.
type session struct {
	hc         *http.Client
	host, repo string
	auth       string // the Authorization header; "" when none is needed
}

func (s *session) url(rest string) string { return "https://" + s.host + "/v2/" + s.repo + "/" + rest }

// open checks the credential and logs in for actions on repo.
func (c Client) open(ctx context.Context, host, repo, actions string, cred api.RegistryCredential) (*session, error) {
	if cred.Username == "" || cred.Password == "" {
		return nil, errors.New("registry: the credential is incomplete")
	}
	if !strings.EqualFold(cred.Host(), host) {
		return nil, errors.New("registry: the credential is for another registry")
	}
	s := &session{hc: c.client(), host: host, repo: repo}
	// The registry's base answers without the credential how to
	// authenticate (or that nothing is needed). Asking the base, not a
	// tag, means a registry that hides private repositories from anonymous
	// requests (a 404, not a 401) can't make a tag look free.
	a, err := sendTwice(ctx, s.hc, http.MethodGet, "https://"+host+"/v2/", "")
	if err != nil {
		return nil, err
	}
	switch a.status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		if s.auth, err = authorize(ctx, s.hc, host, repo, actions, a.header.Get("WWW-Authenticate"), cred); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("registry: %s answered %d when kx asked how to log in", host, a.status)
	}
	return s, nil
}

// digest returns the digest of what tag holds, or "" when there is no
// such tag.
func (s *session) digest(ctx context.Context, tag string) (string, error) {
	a, err := sendTwice(ctx, s.hc, http.MethodHead, s.url("manifests/"+tag), s.auth)
	if err != nil {
		return "", err
	}
	if err := s.check(a, "the tag "+tag); err != nil || a.status == http.StatusNotFound {
		return "", err
	}
	if d := a.header.Get("Docker-Content-Digest"); d != "" {
		if !digestRe.MatchString(d) {
			return "", fmt.Errorf("registry: %s reported a digest kx can't read", s.host)
		}
		return d, nil
	}
	// No digest in the answer: fetch the manifest and hash it.
	body, _, err := s.manifest(ctx, tag)
	if err != nil || body == nil {
		return "", err
	}
	return digestOf(body), nil
}

// manifest fetches the manifest ref (a tag or a digest) and its media
// type; body is nil when there is no such manifest. One fetched by digest
// must hash to it.
func (s *session) manifest(ctx context.Context, ref string) ([]byte, string, error) {
	a, err := sendTwice(ctx, s.hc, http.MethodGet, s.url("manifests/"+ref), s.auth)
	if err != nil {
		return nil, "", err
	}
	if err := s.check(a, ref); err != nil || a.status == http.StatusNotFound {
		return nil, "", err
	}
	if digestRe.MatchString(ref) && digestOf(a.body) != ref {
		return nil, "", fmt.Errorf("registry: %s sent a manifest that doesn't match %s", s.host, ref)
	}
	mediaType, _, err := mime.ParseMediaType(a.header.Get("Content-Type"))
	if err != nil {
		var m struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(a.body, &m)
		mediaType = m.MediaType
	}
	return a.body, mediaType, nil
}

// manifestDoc is the part of an image manifest or index kx reads.
type manifestDoc struct {
	Config *struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform *struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
}

// labels returns the labels of the linux/amd64 image digest names (an
// image, or an index holding one); none when it has no such image.
func (s *session) labels(ctx context.Context, digest string) (map[string]string, error) {
	doc, err := s.manifestDoc(ctx, digest)
	if err != nil {
		return nil, err
	}
	if len(doc.Manifests) > 0 {
		child := ""
		for _, m := range doc.Manifests {
			if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" && digestRe.MatchString(m.Digest) {
				child = m.Digest
				break
			}
		}
		if child == "" {
			return nil, nil
		}
		if doc, err = s.manifestDoc(ctx, child); err != nil {
			return nil, err
		}
	}
	if doc.Config == nil || !digestRe.MatchString(doc.Config.Digest) {
		return nil, nil
	}
	cfg, err := s.blob(ctx, doc.Config.Digest)
	if err != nil {
		return nil, err
	}
	var c struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return nil, fmt.Errorf("registry: %s sent an image configuration kx can't read", s.host)
	}
	return c.Config.Labels, nil
}

func (s *session) manifestDoc(ctx context.Context, digest string) (manifestDoc, error) {
	var doc manifestDoc
	body, _, err := s.manifest(ctx, digest)
	if err != nil {
		return doc, err
	}
	if body == nil {
		return doc, fmt.Errorf("registry: %s no longer has %s", s.host, digest)
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return doc, fmt.Errorf("registry: %s sent a manifest kx can't read", s.host)
	}
	return doc, nil
}

// blob downloads the blob digest, which must hash to it. Registries often
// hand blobs out from storage elsewhere, so redirects are followed, over
// HTTPS only and without the credential once they leave the registry's
// host.
func (s *session) blob(ctx context.Context, digest string) ([]byte, error) {
	hc := *s.hc
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects || req.URL.Scheme != "https" {
			return fmt.Errorf("registry: %s sent kx somewhere it doesn't follow", s.host)
		}
		if !strings.EqualFold(req.URL.Host, s.host) {
			req.Header.Del("Authorization")
		}
		return nil
	}
	a, err := sendTwice(ctx, &hc, http.MethodGet, s.url("blobs/"+digest), s.auth)
	if err != nil {
		return nil, err
	}
	if err := s.check(a, digest); err != nil {
		return nil, err
	}
	if a.status != http.StatusOK || digestOf(a.body) != digest {
		return nil, fmt.Errorf("registry: %s didn't send the blob %s", s.host, digest)
	}
	return a.body, nil
}

// check turns an answer other than 200 or 404 into an error.
func (s *session) check(a answer, what string) error {
	switch a.status {
	case http.StatusOK, http.StatusNotFound:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	}
	return fmt.Errorf("registry: %s answered %d when kx asked about %s", s.host, a.status, what)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
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
// as it is; Bearer exchanges it at the token service for a token for
// actions ("pull", or "pull,push") on repo.
func authorize(ctx context.Context, hc *http.Client, host, repo, actions, challenge string, cred api.RegistryCredential) (string, error) {
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
	q.Set("scope", "repository:"+repo+":"+actions)
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
	return do(ctx, hc, req)
}

func do(ctx context.Context, hc *http.Client, req *http.Request) (answer, error) {
	host := req.URL.Host
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return answer{}, ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL adds nothing the caller doesn't know
		}
		return answer{}, fmt.Errorf("registry: %s: %w", host, err)
	}
	defer resp.Body.Close()
	a := answer{status: resp.StatusCode, header: resp.Header}
	if req.Method == http.MethodGet && resp.StatusCode == http.StatusOK {
		limit := int64(maxManifestBytes)
		if strings.HasSuffix(req.URL.Path, "/v2/token") {
			limit = maxTokenBytes
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return answer{}, fmt.Errorf("registry: %s: %w", host, err)
		}
		if int64(len(body)) > limit {
			return answer{}, fmt.Errorf("registry: %s answered with more than kx reads", host)
		}
		a.body = body
		return a, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	return a, nil
}
