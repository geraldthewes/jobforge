package nomad

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"nomad-mcp-builder/internal/config"
	"nomad-mcp-builder/pkg/types"
)

func TestCreateAgentJobSpec(t *testing.T) {
	nc := testNomadClient()
	job := &types.Job{
		ID: "job-123",
		Config: types.JobConfig{
			Kind:               types.JobKindAgent,
			Owner:              "factory",
			RepoURL:            "https://github.com/example/service.git",
			GitRef:             "main",
			GitCredentialsPath: "secret/data/git",
			Agent: &types.AgentConfig{
				Task:  "Add a health endpoint.",
				Image: "registry.example/jobforge-agent:latest",
				Model: "anthropic/claude-sonnet-4-5",
				VaultSecrets: []types.VaultSecret{{
					Path:   "secret/data/llm",
					Fields: map[string]string{"api_key": "ANTHROPIC_API_KEY"},
				}},
				VaultPolicies: []string{"nomad-agent"},
				FollowUpBuild: &types.FollowUpBuildConfig{
					Enabled:        true,
					DockerfilePath: "Dockerfile",
					ImageName:      "service",
					RegistryURL:    "registry.example/service",
				},
			},
			Review: &types.ReviewConfig{
				Connection: types.ReviewConnection{
					Provider:      "anthropic",
					Model:         "claude-sonnet-4-5",
					CredentialEnv: "ANTHROPIC_API_KEY",
				},
			},
			Output: &types.OutputConfig{Provider: "github", BaseBranch: "main"},
		},
	}

	spec, err := nc.createAgentJobSpec(job)
	if err != nil {
		t.Fatal(err)
	}
	if spec.TaskGroups[0].Tasks[0].Driver != "docker" {
		t.Fatalf("driver %s", spec.TaskGroups[0].Tasks[0].Driver)
	}
	task := spec.TaskGroups[0].Tasks[0]
	if task.Config["privileged"] != false {
		t.Fatalf("privileged = %#v", task.Config["privileged"])
	}
	if task.Config["image"] != "registry.example/jobforge-agent:latest" {
		t.Fatalf("image %#v", task.Config["image"])
	}
	encoded, _ := json.Marshal(task.Config)
	if strings.Contains(string(encoded), "docker.sock") || strings.Contains(string(encoded), "privileged\":true") {
		t.Fatalf("agent container is not unprivileged: %s", encoded)
	}
	if task.Vault == nil || len(task.Vault.Policies) != 2 || task.Vault.Policies[0] != "nomad-agent" {
		t.Fatalf("policies %#v", task.Vault)
	}
	if task.Env["OUTPUT_PROVIDER"] != "github" || task.Env["REPO_OWNER"] != "example" || task.Env["REPO_NAME"] != "service" {
		t.Fatalf("env %#v", task.Env)
	}
	if task.Env["OPEN_PR"] != "true" || task.Env["SR_MAX_ITERATIONS"] != "2" {
		t.Fatalf("defaults missing from env: %#v", task.Env)
	}
	if task.Env["FOLLOWUP_DOCKERFILE"] != "Dockerfile" || task.Env["AGENT_MODEL"] != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("follow-up or model env missing: %#v", task.Env)
	}
	if !strings.Contains(task.Env["TASK_B64"], "QWRk") && task.Env["TASK_B64"] == "" {
		t.Fatal("task was not passed to the runner")
	}

	var script string
	var sawGitToken bool
	for _, tmpl := range task.Templates {
		if tmpl.DestPath != nil && *tmpl.DestPath == "local/agent-runner.sh" {
			script = *tmpl.EmbeddedTmpl
			if tmpl.LeftDelim == nil || *tmpl.LeftDelim == "{{" {
				t.Fatal("runner template must not use consul-template delimiters")
			}
		}
		if tmpl.EmbeddedTmpl != nil && strings.Contains(*tmpl.EmbeddedTmpl, "GIT_TOKEN") {
			sawGitToken = true
		}
	}
	if script == "" {
		t.Fatal("runner script was not embedded")
	}
	if strings.Contains(script, "[%") || strings.Contains(script, "%]") {
		t.Fatal("runner script contains the Nomad template delimiter")
	}
	for _, needle := range []string{
		"JOBFORGE_RESULT",
		"opencode run --standalone --auto",
		"sourced-review catalog trust",
		"sourced-review run",
		"knowledge/reviews/",
		"timeout --signal=TERM",
		"x-access-token",
		"oauth2",
		"/api/v1/repos/",
		"application/vnd.github+json",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("runner script missing %q", needle)
		}
	}
	if !sawGitToken {
		t.Fatal("git credential template was not rendered")
	}
}

func TestBuildSpecUsesGitCredentials(t *testing.T) {
	nc := testNomadClient()
	job := &types.Job{
		ID: "build-1",
		Config: types.JobConfig{
			Owner:              "factory",
			RepoURL:            "https://github.com/example/service.git",
			GitRef:             "main",
			DockerfilePath:     "Dockerfile",
			ImageName:          "service",
			ImageTags:          []string{"latest"},
			RegistryURL:        "registry.example/service",
			GitCredentialsPath: "secret/data/git",
		},
	}
	spec, err := nc.createBuildJobSpec(job)
	if err != nil {
		t.Fatal(err)
	}
	script := spec.TaskGroups[0].Tasks[0].Config["args"].([]string)[1]
	if !strings.Contains(script, "/secrets/git-creds") || !strings.Contains(script, "x-access-token") {
		t.Fatal("build clone does not use git credentials")
	}
	if !strings.Contains(script, "https://github.com/example/service.git") {
		t.Fatal("build clone lost the repo URL")
	}
}

func TestParseAgentResultAndPhase(t *testing.T) {
	logs := []string{
		"[main/stdout] JOBFORGE_PHASE AGENT_RUNNING",
		"[main/stdout] cloning",
		"[main/stdout] JOBFORGE_PHASE REVIEWING",
		`[main/stdout] JOBFORGE_RESULT {"branch":"agent/jobforge-abc","pr_url":"https://github.com/example/service/pull/4","verdict":"Ready","iterations":2,"findings_count":3,"status":"succeeded","dockerfile_present":true}`,
		`[main/stdout] JOBFORGE_RESULT {"branch":"agent/jobforge-abc","pr_url":"https://github.com/example/service/pull/4","verdict":"Ready","iterations":2,"findings_count":1,"status":"succeeded","dockerfile_present":false}`,
	}
	if parseAgentPhaseFromLogs(logs) != types.StatusReviewing {
		t.Fatal("expected the last phase marker")
	}
	result, err := parseAgentResultFromLogs(logs)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Branch != "agent/jobforge-abc" || result.Iterations != 2 || result.FindingsCount != 1 || result.PRURL == "" {
		t.Fatalf("result %+v", result)
	}
	if result.DockerfilePresent {
		t.Fatal("expected the last result line to win")
	}

	if _, err := parseAgentResultFromLogs([]string{"[main/stdout] JOBFORGE_RESULT {not-json"}); err == nil {
		t.Fatal("expected malformed result to fail")
	}
	missing, err := parseAgentResultFromLogs([]string{"[main/stdout] no result"})
	if err != nil || missing != nil {
		t.Fatalf("missing result: %+v %v", missing, err)
	}
}

func TestTruncateLogLinesKeepsHeadAndTail(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, strings.Repeat("a", 50)+strings.Repeat("0", 0))
		lines[i] = strings.Repeat("x", 40) + "-" + strings.Repeat("0123456789", 2) + "-" + string(rune('A'+i%26))
	}
	lines[0] = "HEAD-MARKER"
	lines[len(lines)-1] = "TAIL-MARKER JOBFORGE_RESULT {}"
	truncated := truncateLogLines(lines, 400)
	joined := strings.Join(truncated, "\n")
	if !strings.Contains(joined, "HEAD-MARKER") || !strings.Contains(joined, "TAIL-MARKER") {
		t.Fatalf("head or tail missing: %s", joined)
	}
	if !strings.Contains(joined, "truncated") {
		t.Fatal("expected a truncation marker")
	}
	if len(joined) > 500 {
		t.Fatalf("truncated log is still %d bytes", len(joined))
	}

	short := truncateLogLines([]string{"only"}, 1000)
	if len(short) != 1 || short[0] != "only" {
		t.Fatalf("short log changed: %#v", short)
	}
}

func TestFollowUpBuildJobConfig(t *testing.T) {
	parent := &types.Job{
		Config: types.JobConfig{
			Kind:               types.JobKindAgent,
			Owner:              "factory",
			RepoURL:            "https://github.com/example/service.git",
			GitRef:             "main",
			GitCredentialsPath: "secret/data/git",
			Agent: &types.AgentConfig{
				FollowUpBuild: &types.FollowUpBuildConfig{
					Enabled:        true,
					DockerfilePath: "Dockerfile",
					ImageName:      "service",
					RegistryURL:    "registry.example/service",
					ImageTags:      []string{"latest"},
				},
			},
		},
		AgentResult: &types.AgentResult{Branch: "agent/jobforge-abc", DockerfilePresent: true},
	}
	cfg, err := FollowUpBuildJobConfig(parent)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.Kind != types.JobKindBuild || cfg.GitRef != "agent/jobforge-abc" || cfg.ImageName != "service" {
		t.Fatalf("follow-up %+v", cfg)
	}
	parent.AgentResult.DockerfilePresent = false
	cfg, err = FollowUpBuildJobConfig(parent)
	if err != nil || cfg != nil {
		t.Fatalf("expected skip when dockerfile is absent, cfg=%+v err=%v", cfg, err)
	}
}

func TestGenerateAgentLockKey(t *testing.T) {
	key := GenerateAgentLockKey("https://github.com/example/service.git", "feature/one")
	if key != "agent-github-com-example-service-git-feature-one" {
		t.Fatalf("lock key %s", key)
	}
	other := GenerateAgentLockKey("https://github.com/example/service.git", "main")
	if key == other {
		t.Fatal("different refs must not share a lock key")
	}
}

func TestAgentRunnerScriptParses(t *testing.T) {
	cmd := exec.Command("bash", "-n", "scripts/agent-runner.sh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

func testNomadClient() *Client {
	logger := logrus.New()
	logger.SetLevel(logrus.PanicLevel)
	return &Client{
		config: &config.Config{
			Nomad: config.NomadConfig{
				Namespace:   "default",
				Region:      "global",
				Datacenters: []string{"dc1"},
			},
			Build: config.BuildConfig{
				KillTimeout:          30 * time.Second,
				DockerLogMaxFiles:    4,
				DockerLogMaxFileSize: "10m",
			},
		},
		logger: logger,
	}
}
