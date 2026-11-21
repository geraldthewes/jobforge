package server

import (
	"strings"
	"testing"
	"time"

	"nomad-mcp-builder/pkg/config"
	"nomad-mcp-builder/pkg/types"
)

func TestValidateBuildJobStillRequiresImageFields(t *testing.T) {
	err := validateJobConfig(&types.JobConfig{Owner: "owner", RepoURL: "https://github.com/acme/app.git", GitRef: "main"})
	if err == nil || !strings.Contains(err.Error(), "dockerfile_path") {
		t.Fatalf("expected dockerfile_path error, got %v", err)
	}

	err = validateJobConfig(&types.JobConfig{
		Owner:          "owner",
		RepoURL:        "https://github.com/acme/app.git",
		GitRef:         "main",
		DockerfilePath: "Dockerfile",
		ImageName:      "app",
		RegistryURL:    "registry.example:5000/app",
	})
	if err != nil {
		t.Fatalf("valid build config: %v", err)
	}
}

func TestValidateAgentJob(t *testing.T) {
	cfg := validAgentConfig()
	if err := validateJobConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.CLI != "opencode" {
		t.Fatalf("cli default %s", cfg.Agent.CLI)
	}
	if cfg.Output.BranchPrefix != "agent/" || cfg.Output.APIURL != "https://api.github.com" {
		t.Fatalf("defaults were not applied: %+v", cfg.Output)
	}
	if cfg.Review.MaxIterations != 2 || cfg.Review.OnInsufficientEvidence != "fail" {
		t.Fatalf("review defaults %+v", cfg.Review)
	}

	missingImage := validAgentConfig()
	missingImage.Agent.Image = ""
	if err := validateJobConfig(missingImage); err == nil {
		t.Fatal("expected image to be required")
	}

	bothTasks := validAgentConfig()
	bothTasks.Agent.TaskPath = "TASK.md"
	if err := validateJobConfig(bothTasks); err == nil {
		t.Fatal("expected task and task_path to be exclusive")
	}

	gitea := validAgentConfig()
	gitea.Output.Provider = "gitea"
	gitea.Output.APIURL = ""
	if err := validateJobConfig(gitea); err == nil || !strings.Contains(err.Error(), "api_url") {
		t.Fatalf("expected gitea api_url error, got %v", err)
	}

	secrets := validAgentConfig()
	secrets.Agent.VaultPolicies = nil
	if err := validateJobConfig(secrets); err == nil || !strings.Contains(err.Error(), "vault_policies") {
		t.Fatalf("expected vault policy error, got %v", err)
	}
}

func TestValidateExampleAgentYAML(t *testing.T) {
	cfg, err := config.LoadJobConfigFromYAML("../../examples/agent-job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateJobConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.Timeout.Std() != 60*time.Minute {
		t.Fatalf("timeout %s", cfg.Agent.Timeout.Std())
	}
	if cfg.Output.Provider != "github" || cfg.Agent.FollowUpBuild == nil || !cfg.Agent.FollowUpBuild.Enabled {
		t.Fatalf("example did not decode follow-up or provider: %+v", cfg)
	}
}

func validAgentConfig() *types.JobConfig {
	return &types.JobConfig{
		Kind:    types.JobKindAgent,
		Owner:   "factory",
		RepoURL: "https://github.com/example/service.git",
		GitRef:  "main",
		Agent: &types.AgentConfig{
			Task:  "Add a health endpoint.",
			Image: "registry.example/jobforge-agent:latest",
			VaultSecrets: []types.VaultSecret{{
				Path:   "secret/data/llm",
				Fields: map[string]string{"api_key": "ANTHROPIC_API_KEY"},
			}},
			VaultPolicies: []string{"nomad-agent"},
		},
		Review: &types.ReviewConfig{
			Connection: types.ReviewConnection{
				Provider:      "anthropic",
				Model:         "claude-sonnet-4-5",
				CredentialEnv: "ANTHROPIC_API_KEY",
			},
		},
		Output: &types.OutputConfig{Provider: "github"},
	}
}
