// Package githost builds GitHub and Gitea REST calls used by agent jobs.
// The agent runner script implements the same URL and header rules.
package githost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Provider identifies the git host API.
type Provider string

const (
	ProviderGitHub Provider = "github"
	ProviderGitea  Provider = "gitea"
)

// Repo is a parsed repository identity.
type Repo struct {
	Host  string
	Owner string
	Name  string
}

// PullRequest is the body of a create-pull-request call.
type PullRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

// Request is an HTTP request the runner issues with curl.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// ParseRepoURL accepts https://host/owner/repo.git and git@host:owner/repo.git.
// Nested owner paths (org/team) are preserved in Owner.
func ParseRepoURL(raw string) (Repo, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Repo{}, fmt.Errorf("repo URL is empty")
	}
	raw = strings.TrimSuffix(raw, ".git")

	if strings.HasPrefix(raw, "git@") {
		rest := strings.TrimPrefix(raw, "git@")
		host, path, ok := strings.Cut(rest, ":")
		if !ok || host == "" || path == "" {
			return Repo{}, fmt.Errorf("invalid git SSH URL %q", raw)
		}
		return repoFromPath(host, path)
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return Repo{}, fmt.Errorf("invalid repo URL: %w", err)
	}
	if parsed.Host == "" || parsed.Path == "" {
		return Repo{}, fmt.Errorf("repo URL %q is missing a host or path", raw)
	}
	return repoFromPath(parsed.Host, parsed.Path)
}

func repoFromPath(host, path string) (Repo, error) {
	path = strings.Trim(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[len(parts)-1] == "" {
		return Repo{}, fmt.Errorf("repo path %q must include owner and name", path)
	}
	return Repo{
		Host:  host,
		Owner: strings.Join(parts[:len(parts)-1], "/"),
		Name:  parts[len(parts)-1],
	}, nil
}

// EscapedOwner returns the owner path segment for a REST URL.
func (r Repo) EscapedOwner() string {
	return url.PathEscape(r.Owner)
}

// EscapedName returns the repository name path segment for a REST URL.
func (r Repo) EscapedName() string {
	return url.PathEscape(r.Name)
}

// AuthenticatedCloneURL injects a token into an http(s) clone URL.
// GitHub uses the x-access-token user. Gitea uses the oauth2 user.
func AuthenticatedCloneURL(provider Provider, rawURL, token string) (string, error) {
	if token == "" {
		return rawURL, nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid clone URL: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("authenticated clone requires an http(s) URL")
	}
	switch provider {
	case ProviderGitHub:
		parsed.User = url.UserPassword("x-access-token", token)
	case ProviderGitea:
		parsed.User = url.UserPassword("oauth2", token)
	default:
		return "", fmt.Errorf("unknown git provider %q", provider)
	}
	return parsed.String(), nil
}

// CreatePullRequest builds the REST request that opens a pull request.
// apiURL is the API root. An empty GitHub apiURL uses https://api.github.com.
// A Gitea apiURL is the server root (https://gitea.example.com), with or without a trailing /api/v1.
func CreatePullRequest(provider Provider, apiURL, token string, repo Repo, pr PullRequest) (Request, error) {
	if token == "" {
		return Request{}, fmt.Errorf("git token is empty")
	}
	if repo.Owner == "" || repo.Name == "" {
		return Request{}, fmt.Errorf("repository owner and name are required")
	}
	if pr.Head == "" || pr.Base == "" || pr.Title == "" {
		return Request{}, fmt.Errorf("pull request title, head, and base are required")
	}

	body, err := json.Marshal(pr)
	if err != nil {
		return Request{}, err
	}

	headers := map[string]string{
		"Content-Type": "application/json",
	}
	var endpoint string
	switch provider {
	case ProviderGitHub:
		base := strings.TrimRight(apiURL, "/")
		if base == "" {
			base = "https://api.github.com"
		}
		endpoint = fmt.Sprintf("%s/repos/%s/%s/pulls", base, repo.EscapedOwner(), repo.EscapedName())
		headers["Authorization"] = "Bearer " + token
		headers["Accept"] = "application/vnd.github+json"
	case ProviderGitea:
		base := strings.TrimRight(apiURL, "/")
		if base == "" {
			return Request{}, fmt.Errorf("gitea api_url is required")
		}
		base = strings.TrimSuffix(base, "/api/v1")
		endpoint = fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls", base, repo.EscapedOwner(), repo.EscapedName())
		headers["Authorization"] = "token " + token
		headers["Accept"] = "application/json"
	default:
		return Request{}, fmt.Errorf("unknown git provider %q", provider)
	}

	return Request{
		Method:  "POST",
		URL:     endpoint,
		Headers: headers,
		Body:    body,
	}, nil
}

// DecodePullRequestURL reads html_url (or url) from a create-PR response body.
func DecodePullRequestURL(body []byte) (string, error) {
	var payload struct {
		HTMLURL string `json:"html_url"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if payload.HTMLURL != "" {
		return payload.HTMLURL, nil
	}
	if payload.URL != "" {
		return payload.URL, nil
	}
	return "", fmt.Errorf("pull request response has no url")
}

// BodyField returns a JSON string field from a request body, for tests.
func BodyField(body []byte, field string) (string, error) {
	var payload map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&payload); err != nil {
		return "", err
	}
	value, ok := payload[field]
	if !ok {
		return "", fmt.Errorf("field %s missing", field)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("field %s is not a string", field)
	}
	return text, nil
}
