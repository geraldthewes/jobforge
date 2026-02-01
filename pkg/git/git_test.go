package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "SSH format with colon",
			input:    "git@github.com:user/repo.git",
			expected: "https://github.com/user/repo",
		},
		{
			name:     "SSH format without .git",
			input:    "git@github.com:user/repo",
			expected: "https://github.com/user/repo",
		},
		{
			name:     "SSH URL format",
			input:    "ssh://git@github.com/user/repo.git",
			expected: "https://github.com/user/repo",
		},
		{
			name:     "HTTPS format with .git",
			input:    "https://github.com/user/repo.git",
			expected: "https://github.com/user/repo",
		},
		{
			name:     "HTTPS format without .git",
			input:    "https://github.com/user/repo",
			expected: "https://github.com/user/repo",
		},
		{
			name:     "HTTP format converted to HTTPS",
			input:    "http://github.com/user/repo.git",
			expected: "https://github.com/user/repo",
		},
		{
			name:     "GitLab SSH format",
			input:    "git@gitlab.com:mygroup/myproject.git",
			expected: "https://gitlab.com/mygroup/myproject",
		},
		{
			name:     "Bitbucket SSH format",
			input:    "git@bitbucket.org:team/repo.git",
			expected: "https://bitbucket.org/team/repo",
		},
		{
			name:     "URL with trailing whitespace",
			input:    "https://github.com/user/repo.git  ",
			expected: "https://github.com/user/repo",
		},
		{
			name:     "URL with leading whitespace",
			input:    "  https://github.com/user/repo.git",
			expected: "https://github.com/user/repo",
		},
		{
			name:     "Nested path SSH format",
			input:    "git@github.com:org/subgroup/repo.git",
			expected: "https://github.com/org/subgroup/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeRepoURL(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizeRepoURL(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsGitRepository(t *testing.T) {
	// Test with non-git directory
	tmpDir, err := os.MkdirTemp("", "git-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if IsGitRepository(tmpDir) {
		t.Errorf("IsGitRepository should return false for non-git directory")
	}

	// Test with git repository
	cmd := exec.Command("git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to init git repo: %v", err)
	}

	if !IsGitRepository(tmpDir) {
		t.Errorf("IsGitRepository should return true for git directory")
	}
}

func TestCheckUnpushedCommits_NotAGitRepo(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "git-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	status, err := CheckUnpushedCommits(tmpDir, "main", "https://github.com/user/repo")
	if err != nil {
		t.Fatalf("CheckUnpushedCommits returned error: %v", err)
	}

	if status.IsGitRepo {
		t.Errorf("IsGitRepo should be false for non-git directory")
	}
	if status.HasUnpushed {
		t.Errorf("HasUnpushed should be false for non-git directory")
	}
}

func TestCheckUnpushedCommits_NoRemote(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "git-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize git repo without remote
	cmd := exec.Command("git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to init git repo: %v", err)
	}

	status, err := CheckUnpushedCommits(tmpDir, "main", "https://github.com/user/repo")
	if err != nil {
		t.Fatalf("CheckUnpushedCommits returned error: %v", err)
	}

	if !status.IsGitRepo {
		t.Errorf("IsGitRepo should be true")
	}
	// RepoMatches should be false since no remote is configured
	if status.RepoMatches {
		t.Errorf("RepoMatches should be false when no remote is configured")
	}
}

func TestCheckUnpushedCommits_DifferentRepo(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "git-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize git repo with different remote
	cmd := exec.Command("git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to init git repo: %v", err)
	}

	cmd = exec.Command("git", "remote", "add", "origin", "https://github.com/other/repo.git")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to add remote: %v", err)
	}

	status, err := CheckUnpushedCommits(tmpDir, "main", "https://github.com/user/repo")
	if err != nil {
		t.Fatalf("CheckUnpushedCommits returned error: %v", err)
	}

	if !status.IsGitRepo {
		t.Errorf("IsGitRepo should be true")
	}
	if status.RepoMatches {
		t.Errorf("RepoMatches should be false for different repositories")
	}
}

func TestCheckUnpushedCommits_MatchingRepoSSHvsHTTPS(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "git-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize git repo with SSH remote
	cmd := exec.Command("git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to init git repo: %v", err)
	}

	cmd = exec.Command("git", "remote", "add", "origin", "git@github.com:user/repo.git")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to add remote: %v", err)
	}

	// Check with HTTPS URL - should match due to normalization
	status, err := CheckUnpushedCommits(tmpDir, "main", "https://github.com/user/repo")
	if err != nil {
		t.Fatalf("CheckUnpushedCommits returned error: %v", err)
	}

	if !status.IsGitRepo {
		t.Errorf("IsGitRepo should be true")
	}
	if !status.RepoMatches {
		t.Errorf("RepoMatches should be true - SSH and HTTPS URLs for same repo should match")
	}
}

func TestGetRemoteURL(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "git-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to init git repo: %v", err)
	}

	// No remote initially
	_, err = GetRemoteURL(tmpDir)
	if err == nil {
		t.Errorf("GetRemoteURL should return error when no remote exists")
	}

	// Add remote
	expectedURL := "https://github.com/test/repo.git"
	cmd = exec.Command("git", "remote", "add", "origin", expectedURL)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to add remote: %v", err)
	}

	url, err := GetRemoteURL(tmpDir)
	if err != nil {
		t.Fatalf("GetRemoteURL returned error: %v", err)
	}
	if url != expectedURL {
		t.Errorf("GetRemoteURL() = %q, want %q", url, expectedURL)
	}
}

func TestGetCurrentBranch(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "git-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize git repo
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to init git repo: %v", err)
	}

	// Configure git for the test repo
	cmd = exec.Command("git", "config", "user.email", "test@test.com")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to configure git email: %v", err)
	}

	cmd = exec.Command("git", "config", "user.name", "Test User")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to configure git name: %v", err)
	}

	// Create initial commit so we have a proper branch
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	cmd = exec.Command("git", "add", "test.txt")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to add file: %v", err)
	}

	cmd = exec.Command("git", "commit", "-m", "Initial commit")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	branch, err := GetCurrentBranch(tmpDir)
	if err != nil {
		t.Fatalf("GetCurrentBranch returned error: %v", err)
	}
	if branch != "main" {
		t.Errorf("GetCurrentBranch() = %q, want %q", branch, "main")
	}
}
