package githost

import (
	"strings"
	"testing"
)

func TestParseRepoURL(t *testing.T) {
	tests := []struct {
		raw   string
		host  string
		owner string
		name  string
	}{
		{"https://github.com/example/service.git", "github.com", "example", "service"},
		{"https://gitea.example.com/org/team/service", "gitea.example.com", "org/team", "service"},
		{"git@github.com:example/service.git", "github.com", "example", "service"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			repo, err := ParseRepoURL(tt.raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if repo.Host != tt.host || repo.Owner != tt.owner || repo.Name != tt.name {
				t.Fatalf("got %+v", repo)
			}
		})
	}
}

func TestCreatePullRequestGitHub(t *testing.T) {
	repo, err := ParseRepoURL("https://github.com/example/service.git")
	if err != nil {
		t.Fatal(err)
	}
	req, err := CreatePullRequest(ProviderGitHub, "", "gh-token", repo, PullRequest{
		Title: "jobforge: add health",
		Body:  "verdict: Ready",
		Head:  "agent/jobforge-abc",
		Base:  "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" {
		t.Fatalf("method %s", req.Method)
	}
	if req.URL != "https://api.github.com/repos/example/service/pulls" {
		t.Fatalf("url %s", req.URL)
	}
	if req.Headers["Authorization"] != "Bearer gh-token" {
		t.Fatalf("auth %s", req.Headers["Authorization"])
	}
	if req.Headers["Accept"] != "application/vnd.github+json" {
		t.Fatalf("accept %s", req.Headers["Accept"])
	}
	head, err := BodyField(req.Body, "head")
	if err != nil || head != "agent/jobforge-abc" {
		t.Fatalf("head %q err %v", head, err)
	}
}

func TestCreatePullRequestGitea(t *testing.T) {
	repo, err := ParseRepoURL("https://gitea.example.com/example/service.git")
	if err != nil {
		t.Fatal(err)
	}
	req, err := CreatePullRequest(ProviderGitea, "https://gitea.example.com/api/v1/", "tea-token", repo, PullRequest{
		Title: "jobforge: add health",
		Body:  "body",
		Head:  "agent/jobforge-abc",
		Base:  "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.URL != "https://gitea.example.com/api/v1/repos/example/service/pulls" {
		t.Fatalf("url %s", req.URL)
	}
	if req.Headers["Authorization"] != "token tea-token" {
		t.Fatalf("auth %s", req.Headers["Authorization"])
	}
	if _, err := CreatePullRequest(ProviderGitea, "", "tea-token", repo, PullRequest{Title: "t", Head: "h", Base: "b"}); err == nil {
		t.Fatal("expected gitea api_url to be required")
	}
}

func TestAuthenticatedCloneURL(t *testing.T) {
	github, err := AuthenticatedCloneURL(ProviderGitHub, "https://github.com/example/service.git", "sekret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(github, "x-access-token:") || !strings.Contains(github, "sekret@github.com") {
		t.Fatalf("github url %s", github)
	}
	gitea, err := AuthenticatedCloneURL(ProviderGitea, "https://gitea.example.com/example/service.git", "sekret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gitea, "oauth2:") || !strings.Contains(gitea, "sekret@gitea.example.com") {
		t.Fatalf("gitea url %s", gitea)
	}
}

func TestDecodePullRequestURL(t *testing.T) {
	url, err := DecodePullRequestURL([]byte(`{"html_url":"https://github.com/example/service/pull/7"}`))
	if err != nil || url != "https://github.com/example/service/pull/7" {
		t.Fatalf("url %q err %v", url, err)
	}
}
