// Package git provides utilities for detecting unpushed commits and other git operations.
package git

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// UnpushedStatus contains information about unpushed commits in a git repository.
type UnpushedStatus struct {
	// IsGitRepo indicates whether the directory is a git repository
	IsGitRepo bool
	// RepoMatches indicates whether the local remote matches the config repo_url
	RepoMatches bool
	// HasUnpushed indicates whether there are unpushed commits
	HasUnpushed bool
	// UnpushedCount is the number of unpushed commits
	UnpushedCount int
	// LocalBranch is the current local branch name
	LocalBranch string
	// ConfigGitRef is the git_ref from the job config
	ConfigGitRef string
	// CommitSummary contains the first 5 unpushed commit messages
	CommitSummary []string
}

// IsGitRepository checks if the given directory is a git repository.
func IsGitRepository(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = dir
	err := cmd.Run()
	return err == nil
}

// GetRemoteURL returns the URL of the origin remote for the repository.
func GetRemoteURL(dir string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// NormalizeRepoURL normalizes a repository URL to a canonical HTTPS format.
// This allows comparison between SSH and HTTPS URLs for the same repository.
// Examples:
//   - git@github.com:user/repo.git -> https://github.com/user/repo
//   - https://github.com/user/repo.git -> https://github.com/user/repo
//   - ssh://git@github.com/user/repo -> https://github.com/user/repo
func NormalizeRepoURL(url string) string {
	url = strings.TrimSpace(url)

	// Remove trailing .git
	url = strings.TrimSuffix(url, ".git")

	// Handle SSH URL format: git@github.com:user/repo
	sshPattern := regexp.MustCompile(`^git@([^:]+):(.+)$`)
	if matches := sshPattern.FindStringSubmatch(url); matches != nil {
		host := matches[1]
		path := matches[2]
		return "https://" + host + "/" + path
	}

	// Handle SSH URL format: ssh://git@github.com/user/repo
	sshURLPattern := regexp.MustCompile(`^ssh://git@([^/]+)/(.+)$`)
	if matches := sshURLPattern.FindStringSubmatch(url); matches != nil {
		host := matches[1]
		path := matches[2]
		return "https://" + host + "/" + path
	}

	// Handle HTTPS URL format: https://github.com/user/repo
	// Already normalized, just ensure lowercase protocol
	if path, found := strings.CutPrefix(url, "http://"); found {
		url = "https://" + path
	}

	return url
}

// GetCurrentBranch returns the current branch name.
func GetCurrentBranch(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// GetUnpushedCommitCount returns the number of commits ahead of the upstream.
func GetUnpushedCommitCount(dir string) (int, error) {
	cmd := exec.Command("git", "rev-list", "@{u}..HEAD", "--count")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		// Could be no upstream configured
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, err
	}
	return count, nil
}

// GetUnpushedCommitSummary returns the oneline summaries of unpushed commits (max 5).
func GetUnpushedCommitSummary(dir string, maxCount int) ([]string, error) {
	cmd := exec.Command("git", "log", "@{u}..HEAD", "--oneline", "-n", strconv.Itoa(maxCount))
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var summaries []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			summaries = append(summaries, line)
		}
	}
	return summaries, nil
}

// HasUpstreamConfigured checks if the current branch has an upstream configured.
func HasUpstreamConfigured(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "@{u}")
	cmd.Dir = dir
	err := cmd.Run()
	return err == nil
}

// CheckUnpushedCommits checks for unpushed commits in the given directory.
// It compares the local repository state with the configured gitRef and repoURL.
// Returns nil if not in a git repo or if repo URLs don't match.
func CheckUnpushedCommits(dir, gitRef, repoURL string) (*UnpushedStatus, error) {
	status := &UnpushedStatus{
		ConfigGitRef: gitRef,
	}

	// Check if directory is a git repository
	if !IsGitRepository(dir) {
		status.IsGitRepo = false
		return status, nil
	}
	status.IsGitRepo = true

	// Get local remote URL
	localRemote, err := GetRemoteURL(dir)
	if err != nil {
		// No remote configured - skip check
		return status, nil
	}

	// Compare normalized URLs
	normalizedLocal := NormalizeRepoURL(localRemote)
	normalizedConfig := NormalizeRepoURL(repoURL)

	if normalizedLocal != normalizedConfig {
		// Different repository - skip check
		status.RepoMatches = false
		return status, nil
	}
	status.RepoMatches = true

	// Get current branch
	branch, err := GetCurrentBranch(dir)
	if err != nil {
		return status, nil
	}
	status.LocalBranch = branch

	// Check if upstream is configured
	if !HasUpstreamConfigured(dir) {
		// No upstream configured - can't check for unpushed commits
		return status, nil
	}

	// Count unpushed commits
	count, err := GetUnpushedCommitCount(dir)
	if err != nil {
		// Could be various git errors, skip check
		return status, nil
	}

	if count > 0 {
		status.HasUnpushed = true
		status.UnpushedCount = count

		// Get commit summaries (max 5)
		summaries, err := GetUnpushedCommitSummary(dir, 5)
		if err == nil {
			status.CommitSummary = summaries
		}
	}

	return status, nil
}
