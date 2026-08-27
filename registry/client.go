// Package registry is the tool's own reader of a container registry: enough
// of the OCI distribution API to resolve a reference, read a manifest and
// read a blob. The tool renders a pack straight from the registry — the
// index, the schemas, the templates and an image's config — and never an
// image layer: pulling images is horchestra's job. Nothing here writes.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Scheme is the prefix of a registry reference, as in oci-packer's pack files.
const Scheme = "cr://"

// Ref is a parsed registry reference: host/repo:tag or host/repo@digest.
type Ref struct {
	Host   string
	Repo   string
	Tag    string
	Digest string
}

// Parse reads cr://host/repo:tag, cr://host/repo@sha256:…, or the same
// without the scheme. A reference names a registry when its first segment
// has a dot, a port or is localhost — a bare directory name does not.
func Parse(s string) (Ref, error) {
	s = strings.TrimPrefix(s, Scheme)
	host, rest, ok := strings.Cut(s, "/")
	if !ok || !IsRegistryHost(host) {
		return Ref{}, fmt.Errorf("%q is not a registry reference (host/repo:tag or host/repo@digest)", s)
	}
	r := Ref{Host: host}
	if before, after, ok := strings.Cut(rest, "@"); ok {
		r.Repo, r.Digest = before, after
	} else if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		r.Repo, r.Tag = rest[:i], rest[i+1:]
	} else {
		r.Repo, r.Tag = rest, "latest"
	}
	if len(r.Repo) == 0 {
		return Ref{}, fmt.Errorf("%q: empty repository", s)
	}
	return r, nil
}

// IsRegistryHost says whether the first path segment is a registry host.
func IsRegistryHost(host string) bool {
	return host == "localhost" || strings.ContainsAny(host, ".:")
}

// IsRef says whether s looks like a registry reference rather than a path.
func IsRef(s string) bool {
	if strings.HasPrefix(s, Scheme) {
		return true
	}
	_, err := Parse(s)
	return err == nil && !strings.HasPrefix(s, ".") && !strings.HasPrefix(s, "/")
}

// Reference is the tag or the digest — what goes into the manifests URL.
func (r Ref) Reference() string {
	if len(r.Digest) > 0 {
		return r.Digest
	}
	return r.Tag
}

func (r Ref) String() string {
	if len(r.Digest) > 0 {
		return r.Host + "/" + r.Repo + "@" + r.Digest
	}
	return r.Host + "/" + r.Repo + ":" + r.Tag
}

// Client talks to one or more registries. Zero value: HTTPS, anonymous.
type Client struct {
	PlainHTTP bool   // http:// instead of https://
	Username  string // basic credentials, also used to obtain a bearer token
	Password  string
	HTTP      *http.Client // nil: a default with a timeout

	mu     sync.Mutex
	tokens map[string]string // realm+scope → bearer token; guarded by mu — members are read concurrently
}

// Manifest media types the reader accepts, most specific first.
var acceptManifests = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Manifest fetches the manifest ref names in host/repo: its media type, its
// digest (the registry's Docker-Content-Digest, verified against the bytes
// when present, computed otherwise) and the bytes.
func (c *Client) Manifest(ctx context.Context, host, repo, ref string) (mediaType, digest string, body []byte, err error) {
	resp, err := c.get(ctx, host, repo, "/manifests/"+ref, acceptManifests)
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", "", nil, err
	}
	sum := sha256.Sum256(body)
	digest = "sha256:" + hex.EncodeToString(sum[:])
	if h := resp.Header.Get("Docker-Content-Digest"); len(h) > 0 && h != digest {
		return "", "", nil, fmt.Errorf("%s/%s@%s: the registry names digest %s but the bytes hash to %s", host, repo, ref, h, digest)
	}
	if strings.HasPrefix(ref, "sha256:") && ref != digest {
		return "", "", nil, fmt.Errorf("%s/%s: asked for %s, the bytes hash to %s", host, repo, ref, digest)
	}
	mediaType, _, _ = strings.Cut(resp.Header.Get("Content-Type"), ";")
	mediaType = strings.TrimSpace(mediaType)
	if len(mediaType) == 0 || mediaType == "application/octet-stream" {
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(body, &probe)
		mediaType = probe.MediaType
	}
	return mediaType, digest, body, nil
}

// Blob fetches a blob by digest and verifies it. limit caps the size: the
// tool reads schemas, templates and image configs — never a layer.
func (c *Client) Blob(ctx context.Context, host, repo, digest string, limit int64) ([]byte, error) {
	resp, err := c.get(ctx, host, repo, "/blobs/"+digest, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if limit <= 0 {
		limit = 8 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s/%s@%s: larger than %d bytes — not a schema, a template or an image config", host, repo, digest, limit)
	}
	sum := sha256.Sum256(body)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return nil, fmt.Errorf("%s/%s: blob %s hashes to %s", host, repo, digest, got)
	}
	return body, nil
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) base(host string) string {
	if c.PlainHTTP {
		return "http://" + host
	}
	return "https://" + host
}

// get performs one GET, answering a 401 with basic credentials or with a
// bearer token obtained from the realm the registry names.
func (c *Client) get(ctx context.Context, host, repo, path, accept string) (*http.Response, error) {
	u := c.base(host) + "/v2/" + repo + path
	do := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if len(accept) > 0 {
			req.Header.Set("Accept", accept)
		}
		if len(auth) > 0 {
			req.Header.Set("Authorization", auth)
		}
		return c.http().Do(req)
	}
	resp, err := do(c.cachedToken(repo))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		auth, err := c.authorize(ctx, challenge, repo)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u, err)
		}
		if resp, err = do(auth); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: %s: %s", u, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (c *Client) cachedToken(repo string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tokens[repo]; ok {
		return "Bearer " + t
	}
	return ""
}

// authorize turns a WWW-Authenticate challenge into an Authorization value:
// Basic with the credentials, or Bearer with a token from the realm.
func (c *Client) authorize(ctx context.Context, challenge, repo string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		if len(c.Username) == 0 {
			return "", errors.New("the registry wants credentials (basic)")
		}
		req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
		req.SetBasicAuth(c.Username, c.Password)
		return req.Header.Get("Authorization"), nil
	case "bearer":
		p := parseChallenge(params)
		if len(p["realm"]) == 0 {
			return "", fmt.Errorf("bearer challenge without a realm: %q", challenge)
		}
		q := url.Values{}
		if len(p["service"]) > 0 {
			q.Set("service", p["service"])
		}
		if len(p["scope"]) > 0 {
			q.Set("scope", p["scope"])
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p["realm"]+"?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		if len(c.Username) > 0 {
			req.SetBasicAuth(c.Username, c.Password)
		}
		resp, err := c.http().Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("token from %s: %s", p["realm"], resp.Status)
		}
		var tok struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		if err = json.NewDecoder(resp.Body).Decode(&tok); err != nil {
			return "", err
		}
		if len(tok.Token) == 0 {
			tok.Token = tok.AccessToken
		}
		if len(tok.Token) == 0 {
			return "", fmt.Errorf("token from %s: empty", p["realm"])
		}
		c.mu.Lock()
		if c.tokens == nil {
			c.tokens = map[string]string{}
		}
		c.tokens[repo] = tok.Token
		c.mu.Unlock()
		return "Bearer " + tok.Token, nil
	}
	return "", fmt.Errorf("unsupported challenge %q", challenge)
}

// parseChallenge reads realm="…",service="…",scope="…".
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for part := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.ToLower(k)] = strings.Trim(v, `"`)
	}
	return out
}
