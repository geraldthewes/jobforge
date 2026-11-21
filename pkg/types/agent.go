package types

// Job kind values. Empty kind is treated as a build job.
const (
	JobKindBuild = "build"
	JobKindAgent = "agent"
)

// AgentResult is the machine-readable outcome printed by the agent runner.
type AgentResult struct {
	Branch            string `json:"branch,omitempty"`
	PRURL             string `json:"pr_url,omitempty"`
	Verdict           string `json:"verdict,omitempty"`
	Iterations        int    `json:"iterations,omitempty"`
	FindingsCount     int    `json:"findings_count,omitempty"`
	Status            string `json:"status,omitempty"`
	DockerfilePresent bool   `json:"dockerfile_present,omitempty"`
}

// AgentConfig configures a one-allocation coding-agent job.
type AgentConfig struct {
	// Task is the instruction text. Mutually exclusive with TaskPath.
	Task string `json:"task,omitempty" yaml:"task,omitempty"`
	// TaskPath is a path inside the cloned repository whose contents are the task.
	TaskPath string `json:"task_path,omitempty" yaml:"task_path,omitempty"`
	// Image is the unprivileged container image (opencode, git, review CLI).
	Image string `json:"image" yaml:"image"`
	// CLI is the coding agent. The built-in command is opencode.
	CLI string `json:"cli,omitempty" yaml:"cli,omitempty"`
	// Model is an optional provider/model passed to opencode (--model).
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// CommandTemplate overrides the agent invocation. It is a shell command
	// run with bash -c. $TASK_FILE, $REPO, and $AGENT_MODEL are set.
	CommandTemplate string `json:"command_template,omitempty" yaml:"command_template,omitempty"`
	// VaultSecrets inject LLM keys and git tokens as environment variables.
	VaultSecrets []VaultSecret `json:"vault_secrets,omitempty" yaml:"vault_secrets,omitempty"`
	// VaultPolicies are the Nomad Vault policies used to read those secrets.
	VaultPolicies []string `json:"vault_policies,omitempty" yaml:"vault_policies,omitempty"`
	// Timeout is the overall job deadline enforced by the monitor.
	Timeout *FlexibleDuration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// StepTimeout bounds each pipeline step (clone, agent, review, push, PR).
	StepTimeout *FlexibleDuration `json:"step_timeout,omitempty" yaml:"step_timeout,omitempty"`
	// ResourceLimits override the agent container's CPU, memory, and disk.
	ResourceLimits *PhaseResourceLimits `json:"resource_limits,omitempty" yaml:"resource_limits,omitempty"`
	// FollowUpBuild optionally runs the existing build/test pipeline on the pushed branch.
	FollowUpBuild *FollowUpBuildConfig `json:"follow_up_build,omitempty" yaml:"follow_up_build,omitempty"`
}

// FollowUpBuildConfig submits an independent build job after the agent pushes a branch.
// The follow-up is skipped when the Dockerfile is not present in the checkout.
type FollowUpBuildConfig struct {
	Enabled                 bool              `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	DockerfilePath          string            `json:"dockerfile_path,omitempty" yaml:"dockerfile_path,omitempty"`
	DockerfileContext       string            `json:"dockerfile_context,omitempty" yaml:"dockerfile_context,omitempty"`
	ImageName               string            `json:"image_name,omitempty" yaml:"image_name,omitempty"`
	ImageTags               []string          `json:"image_tags,omitempty" yaml:"image_tags,omitempty"`
	RegistryURL             string            `json:"registry_url,omitempty" yaml:"registry_url,omitempty"`
	RegistryCredentialsPath string            `json:"registry_credentials_path,omitempty" yaml:"registry_credentials_path,omitempty"`
	Test                    *TestConfig       `json:"test,omitempty" yaml:"test,omitempty"`
	BuildTimeout            *FlexibleDuration `json:"build_timeout,omitempty" yaml:"build_timeout,omitempty"`
}

// ReviewConfig drives sourced-review inside the agent allocation.
type ReviewConfig struct {
	Connection             ReviewConnection `json:"connection" yaml:"connection"`
	Base                   string           `json:"base,omitempty" yaml:"base,omitempty"`
	Check                  string           `json:"check,omitempty" yaml:"check,omitempty"`
	FixOn                  ReviewFixOn      `json:"fix_on,omitempty" yaml:"fix_on,omitempty"`
	OnInsufficientEvidence string           `json:"on_insufficient_evidence,omitempty" yaml:"on_insufficient_evidence,omitempty"`
	MaxIterations          int              `json:"max_iterations,omitempty" yaml:"max_iterations,omitempty"`
	Concurrency            int              `json:"concurrency,omitempty" yaml:"concurrency,omitempty"`
	MaxOutputTokens        int              `json:"max_output_tokens,omitempty" yaml:"max_output_tokens,omitempty"`
	// CatalogRoot overrides the image default (/opt/sourced-review/catalog).
	CatalogRoot string `json:"catalog_root,omitempty" yaml:"catalog_root,omitempty"`
}

// ReviewConnection is the non-interactive sourced-review provider setup.
type ReviewConnection struct {
	Provider      string `json:"provider" yaml:"provider"`
	Model         string `json:"model" yaml:"model"`
	CredentialEnv string `json:"credential_env" yaml:"credential_env"`
	BaseURL       string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
}

// ReviewFixOn selects which review findings are sent back to the agent.
// A finding matches when its disposition is listed and either its severity is
// listed or (when BlocksMerge is true) the finding itself blocks merge.
type ReviewFixOn struct {
	Dispositions []string `json:"dispositions,omitempty" yaml:"dispositions,omitempty"`
	BlocksMerge  *bool    `json:"blocks_merge,omitempty" yaml:"blocks_merge,omitempty"`
	Severities   []string `json:"severities,omitempty" yaml:"severities,omitempty"`
}

// OutputConfig describes where the agent branch is pushed and how the PR is opened.
type OutputConfig struct {
	// Provider is "github" or "gitea".
	Provider string `json:"provider" yaml:"provider"`
	// APIURL is the REST API root. Required for gitea. Defaults to https://api.github.com.
	APIURL string `json:"api_url,omitempty" yaml:"api_url,omitempty"`
	// BranchPrefix is prepended to the generated branch name. Default "agent/".
	BranchPrefix string `json:"branch_prefix,omitempty" yaml:"branch_prefix,omitempty"`
	// BaseBranch is the PR base. Defaults to git_ref.
	BaseBranch string `json:"base_branch,omitempty" yaml:"base_branch,omitempty"`
	// OpenPR opens a pull request when true. Defaults to true.
	OpenPR *bool `json:"open_pr,omitempty" yaml:"open_pr,omitempty"`
	// TokenEnv is the environment variable holding the git host token.
	// Defaults to GH_TOKEN for GitHub and GITEA_TOKEN for Gitea.
	TokenEnv string `json:"token_env,omitempty" yaml:"token_env,omitempty"`
}

// IsAgent reports whether the job runs the agent pipeline.
func (c *JobConfig) IsAgent() bool {
	return c != nil && c.Kind == JobKindAgent
}

// ApplyAgentDefaults fills unset agent, review, and output fields.
// It does not invent required values such as the task, image, or provider.
func ApplyAgentDefaults(config *JobConfig) {
	if config == nil || !config.IsAgent() || config.Agent == nil {
		return
	}
	if config.Agent.CLI == "" {
		config.Agent.CLI = "opencode"
	}
	if config.Output != nil {
		if config.Output.BranchPrefix == "" {
			config.Output.BranchPrefix = "agent/"
		}
		if config.Output.BaseBranch == "" {
			config.Output.BaseBranch = config.GitRef
		}
		if config.Output.OpenPR == nil {
			open := true
			config.Output.OpenPR = &open
		}
		if config.Output.TokenEnv == "" {
			if config.Output.Provider == "gitea" {
				config.Output.TokenEnv = "GITEA_TOKEN"
			} else {
				config.Output.TokenEnv = "GH_TOKEN"
			}
		}
		if config.Output.Provider == "github" && config.Output.APIURL == "" {
			config.Output.APIURL = "https://api.github.com"
		}
	}
	if config.Review != nil {
		review := config.Review
		if review.Check == "" {
			review.Check = "local"
		}
		if review.MaxIterations <= 0 {
			review.MaxIterations = 2
		}
		if review.OnInsufficientEvidence == "" {
			review.OnInsufficientEvidence = "fail"
		}
		if len(review.FixOn.Dispositions) == 0 {
			review.FixOn.Dispositions = []string{"confirmed"}
		}
		if len(review.FixOn.Severities) == 0 {
			review.FixOn.Severities = []string{"critical", "high"}
		}
		if review.FixOn.BlocksMerge == nil {
			blocks := true
			review.FixOn.BlocksMerge = &blocks
		}
		if review.CatalogRoot == "" {
			review.CatalogRoot = "/opt/sourced-review/catalog"
		}
	}
	if config.Agent.FollowUpBuild != nil && config.Agent.FollowUpBuild.Enabled && config.Agent.FollowUpBuild.DockerfilePath == "" {
		config.Agent.FollowUpBuild.DockerfilePath = "Dockerfile"
	}
}
