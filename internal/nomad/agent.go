package nomad

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "embed"

	"github.com/google/uuid"
	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/sirupsen/logrus"

	"nomad-mcp-builder/internal/githost"
	"nomad-mcp-builder/pkg/types"
)

//go:embed scripts/agent-runner.sh
var agentRunnerScript string

// GenerateAgentLockKey locks one repository ref so two agent jobs cannot
// push the same base at the same time.
func GenerateAgentLockKey(repoURL, gitRef string) string {
	return fmt.Sprintf("agent-%s-%s", sanitizeLockPart(repoURL), sanitizeLockPart(gitRef))
}

func sanitizeLockPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	value = strings.NewReplacer(
		"/", "-",
		":", "-",
		".", "-",
		"@", "-",
		"_", "-",
		" ", "-",
	).Replace(value)
	return value
}

// createAgentJob submits the single-allocation agent pipeline.
func (nc *Client) createAgentJob(jobConfig *types.JobConfig) (*types.Job, error) {
	types.ApplyAgentDefaults(jobConfig)

	jobID := uuid.New().String()
	lockKey := GenerateAgentLockKey(jobConfig.RepoURL, jobConfig.GitRef)
	lockTTL := defaultAgentTimeout + 15*time.Minute
	if jobConfig.Agent != nil && jobConfig.Agent.Timeout.Std() > lockTTL {
		lockTTL = jobConfig.Agent.Timeout.Std() + 15*time.Minute
	}
	lockMetadata := &types.LockMetadata{
		JobID:       jobID,
		Owner:       jobConfig.Owner,
		RegistryURL: jobConfig.RepoURL,
		ImageName:   "agent",
		GitRef:      jobConfig.GitRef,
	}

	sessionID, err := nc.storage.AcquireLockWithMetadata(lockKey, lockTTL, lockMetadata)
	if err != nil {
		nc.logger.WithFields(logrus.Fields{
			"lock_key": lockKey,
			"job_id":   jobID,
			"repo_url": jobConfig.RepoURL,
			"git_ref":  jobConfig.GitRef,
		}).Warn("Failed to acquire agent lock - another agent job may be in progress")
		return nil, fmt.Errorf("cannot start agent job: %w", err)
	}

	now := time.Now()
	job := &types.Job{
		ID:            jobID,
		Config:        *jobConfig,
		Status:        types.StatusPending,
		CreatedAt:     now,
		UpdatedAt:     now,
		CurrentPhase:  "agent",
		Logs:          types.JobLogs{},
		Metrics:       types.JobMetrics{JobStart: &now},
		LockKey:       lockKey,
		LockSessionID: sessionID,
	}

	jobSpec, err := nc.createAgentJobSpec(job)
	if err != nil {
		nc.releaseBuildLock(job)
		return nil, fmt.Errorf("failed to create agent job spec: %w", err)
	}

	nc.logJobSpec(jobSpec, "agent")
	registerOpts := &nomadapi.RegisterOptions{}
	writeOpts := &nomadapi.WriteOptions{
		Region:    nc.config.Nomad.Region,
		Namespace: nc.config.Nomad.Namespace,
	}
	evalID, _, err := nc.client.Jobs().RegisterOpts(jobSpec, registerOpts, writeOpts)
	if err != nil {
		nc.releaseBuildLock(job)
		return nil, fmt.Errorf("failed to submit agent job to Nomad: %w", err)
	}

	job.AgentJobID = *jobSpec.ID
	job.Status = types.StatusAgentRunning
	job.StartedAt = &now

	nc.logger.WithFields(logrus.Fields{
		"job_id":    jobID,
		"nomad_job": *jobSpec.ID,
		"eval_id":   evalID,
	}).Info("Agent job submitted to Nomad")
	return job, nil
}

// createAgentJobSpec builds one unprivileged batch job that runs the agent pipeline.
func (nc *Client) createAgentJobSpec(job *types.Job) (*nomadapi.Job, error) {
	if job.Config.Agent == nil || job.Config.Review == nil || job.Config.Output == nil {
		return nil, fmt.Errorf("agent jobs require agent, review, and output configuration")
	}
	types.ApplyAgentDefaults(&job.Config)

	repo, err := githost.ParseRepoURL(job.Config.RepoURL)
	if err != nil {
		return nil, err
	}

	defaults := types.PhaseResourceLimits{CPU: "2000", Memory: "4096", Disk: "20480"}
	limits := defaults
	if job.Config.Agent.ResourceLimits != nil {
		limits = *job.Config.Agent.ResourceLimits
		if limits.CPU == "" {
			limits.CPU = defaults.CPU
		}
		if limits.Memory == "" {
			limits.Memory = defaults.Memory
		}
		if limits.Disk == "" {
			limits.Disk = defaults.Disk
		}
	}
	var cpu, memory, disk int
	fmt.Sscanf(limits.CPU, "%d", &cpu)
	fmt.Sscanf(limits.Memory, "%d", &memory)
	fmt.Sscanf(limits.Disk, "%d", &disk)
	if cpu == 0 {
		cpu = 2000
	}
	if memory == 0 {
		memory = 4096
	}
	if disk == 0 {
		disk = 20480
	}

	jobID := fmt.Sprintf("agent-%s", job.ID)
	stepTimeout := defaultStepTimeout
	if job.Config.Agent.StepTimeout.Std() > 0 {
		stepTimeout = job.Config.Agent.StepTimeout.Std()
	}
	agentTimeout := defaultAgentTimeout
	if job.Config.Agent.Timeout.Std() > 0 {
		agentTimeout = job.Config.Agent.Timeout.Std()
	}

	taskB64 := ""
	if job.Config.Agent.Task != "" {
		taskB64 = base64.StdEncoding.EncodeToString([]byte(job.Config.Agent.Task))
	}
	followDockerfile := ""
	if job.Config.Agent.FollowUpBuild != nil && job.Config.Agent.FollowUpBuild.Enabled {
		followDockerfile = job.Config.Agent.FollowUpBuild.DockerfilePath
	}

	review := job.Config.Review
	output := job.Config.Output
	env := map[string]string{
		"JOB_ID":              job.ID,
		"REPO_URL":            job.Config.RepoURL,
		"GIT_REF":             job.Config.GitRef,
		"REPO_OWNER":          repo.EscapedOwner(),
		"REPO_NAME":           repo.EscapedName(),
		"OUTPUT_PROVIDER":     output.Provider,
		"API_URL":             output.APIURL,
		"BRANCH_PREFIX":       output.BranchPrefix,
		"BASE_BRANCH":         agentBaseBranch(review, output),
		"OPEN_PR":             boolString(output.OpenPR != nil && *output.OpenPR),
		"TOKEN_ENV":           output.TokenEnv,
		"TASK_B64":            taskB64,
		"TASK_PATH":           job.Config.Agent.TaskPath,
		"AGENT_COMMAND":       job.Config.Agent.CommandTemplate,
		"AGENT_MODEL":         job.Config.Agent.Model,
		"STEP_TIMEOUT":        stepTimeout.String(),
		"AGENT_TIMEOUT":       agentTimeout.String(),
		"SR_PROVIDER":         review.Connection.Provider,
		"SR_MODEL":            review.Connection.Model,
		"SR_CREDENTIAL_ENV":   review.Connection.CredentialEnv,
		"SR_BASE_URL":         review.Connection.BaseURL,
		"SR_CATALOG":          review.CatalogRoot,
		"SR_CHECK":            review.Check,
		"SR_MAX_ITERATIONS":   fmt.Sprintf("%d", review.MaxIterations),
		"SR_ON_INSUFFICIENT":  review.OnInsufficientEvidence,
		"FIX_DISPOSITIONS":    mustJSON(review.FixOn.Dispositions),
		"FIX_SEVERITIES":      mustJSON(review.FixOn.Severities),
		"FIX_BLOCKS_MERGE":    boolString(review.FixOn.BlocksMerge != nil && *review.FixOn.BlocksMerge),
		"FOLLOWUP_DOCKERFILE": followDockerfile,
		"XDG_DATA_HOME":       "/alloc/sr",
	}
	if review.Concurrency > 0 {
		env["SR_CONCURRENCY"] = fmt.Sprintf("%d", review.Concurrency)
	}
	if review.MaxOutputTokens > 0 {
		env["SR_MAX_OUTPUT_TOKENS"] = fmt.Sprintf("%d", review.MaxOutputTokens)
	}

	dockerConfig := map[string]interface{}{
		"image":      job.Config.Agent.Image,
		"command":    "/bin/bash",
		"args":       []string{"/local/agent-runner.sh"},
		"privileged": false,
		"logging": map[string]interface{}{
			"type": "json-file",
			"config": []map[string]interface{}{
				{
					"max-file": fmt.Sprintf("%d", nc.config.Build.DockerLogMaxFiles),
					"max-size": nc.config.Build.DockerLogMaxFileSize,
				},
			},
		},
	}

	task := &nomadapi.Task{
		Name:   "main",
		Driver: "docker",
		Config: dockerConfig,
		Env:    env,
		Resources: &nomadapi.Resources{
			CPU:      intPtr(cpu),
			MemoryMB: intPtr(memory),
			DiskMB:   intPtr(disk),
		},
		KillTimeout: &nc.config.Build.KillTimeout,
		LogConfig: &nomadapi.LogConfig{
			MaxFiles:      intPtr(logFileCount(nc.config.Build.DockerLogMaxFiles)),
			MaxFileSizeMB: intPtr(10),
		},
		Templates: agentTemplates(job),
	}
	if policies := agentVaultPolicies(job); len(policies) > 0 {
		task.Vault = &nomadapi.Vault{
			Policies:   policies,
			ChangeMode: stringPtr("restart"),
			Role:       "nomad-workloads",
		}
	}

	return &nomadapi.Job{
		ID:          &jobID,
		Name:        &jobID,
		Type:        stringPtr("batch"),
		Namespace:   stringPtr(nc.config.Nomad.Namespace),
		Region:      stringPtr(nc.config.Nomad.Region),
		Datacenters: nc.config.Nomad.Datacenters,
		Meta: map[string]string{
			"build-service-job-id": job.ID,
			"phase":                "agent",
			"kind":                 types.JobKindAgent,
		},
		TaskGroups: []*nomadapi.TaskGroup{
			{
				Name:  stringPtr("agent"),
				Count: intPtr(1),
				RestartPolicy: &nomadapi.RestartPolicy{
					Attempts: intPtr(0),
				},
				ReschedulePolicy: &nomadapi.ReschedulePolicy{
					Attempts:  intPtr(0),
					Unlimited: boolPtr(false),
				},
				Tasks: []*nomadapi.Task{task},
				EphemeralDisk: &nomadapi.EphemeralDisk{
					SizeMB: intPtr(disk),
				},
			},
		},
		Stop: boolPtr(false),
	}, nil
}

func agentTemplates(job *types.Job) []*nomadapi.Template {
	templates := []*nomadapi.Template{
		{
			DestPath:     stringPtr("local/agent-runner.sh"),
			EmbeddedTmpl: stringPtr(agentRunnerScript),
			ChangeMode:   stringPtr("noop"),
			Perms:        stringPtr("0755"),
			LeftDelim:    stringPtr("[%"),
			RightDelim:   stringPtr("%]"),
		},
	}
	if job.Config.GitCredentialsPath != "" {
		templates = append(templates, &nomadapi.Template{
			DestPath:   stringPtr("/secrets/git-creds"),
			ChangeMode: stringPtr("noop"),
			EmbeddedTmpl: stringPtr(fmt.Sprintf(`
{{- with secret "%s" -}}
export GIT_USERNAME="{{ .Data.data.username }}"
export GIT_PASSWORD="{{ .Data.data.password }}"
export GIT_TOKEN="{{ .Data.data.token }}"
export GIT_SSH_KEY="{{ .Data.data.ssh_key }}"
{{- end -}}
`, job.Config.GitCredentialsPath)),
		})
	}
	if job.Config.Agent != nil {
		for i, secret := range job.Config.Agent.VaultSecrets {
			templates = append(templates, generateVaultSecretTemplate(secret, i))
		}
	}
	return templates
}

func agentVaultPolicies(job *types.Job) []string {
	var policies []string
	if job.Config.Agent != nil {
		policies = append(policies, job.Config.Agent.VaultPolicies...)
	}
	if job.Config.GitCredentialsPath != "" && !containsString(policies, "nomad-build-service") {
		policies = append(policies, "nomad-build-service")
	}
	if len(policies) == 0 && job.Config.Agent != nil && len(job.Config.Agent.VaultSecrets) > 0 {
		policies = append(policies, "nomad-build-service")
	}
	return policies
}

func agentBaseBranch(review *types.ReviewConfig, output *types.OutputConfig) string {
	if review != nil && review.Base != "" {
		return review.Base
	}
	if output != nil {
		return output.BaseBranch
	}
	return ""
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (nc *Client) updateAgentJobStatus(job *types.Job) (*types.Job, error) {
	if job.AgentJobID == "" {
		return job, nil
	}
	status, err := nc.getJobStatus(job.AgentJobID)
	if err != nil {
		return job, fmt.Errorf("failed to get agent job status: %w", err)
	}

	switch status {
	case "pending", "running":
		if err := nc.captureAgentLogs(job); err != nil {
			nc.logger.WithError(err).WithField("job_id", job.ID).Warn("Failed to capture agent logs")
		}
		nc.applyAgentPhase(job)
	case "complete":
		if err := nc.captureAgentLogs(job); err != nil {
			nc.logger.WithError(err).WithField("job_id", job.ID).Warn("Failed to capture agent logs")
		}
		nc.storeAgentResult(job)
		now := time.Now()
		job.Status = types.StatusSucceeded
		job.CurrentPhase = "agent"
		if job.FinishedAt == nil {
			job.FinishedAt = &now
		}
		job.Metrics.JobEnd = &now
		if job.Metrics.JobStart != nil {
			job.Metrics.TotalDuration = now.Sub(*job.Metrics.JobStart)
		}
		nc.releaseBuildLock(job)
	case "failed":
		if err := nc.captureAgentLogs(job); err != nil {
			nc.logger.WithError(err).WithField("job_id", job.ID).Warn("Failed to capture failed agent logs")
		}
		nc.storeAgentResult(job)
		now := time.Now()
		job.Status = types.StatusFailed
		job.CurrentPhase = "agent"
		job.FailedPhase = "agent"
		errorMsg, err := nc.getJobErrorDetails(job.AgentJobID)
		if err != nil || errorMsg == "" {
			job.Error = "Agent phase failed"
		} else {
			job.Error = fmt.Sprintf("Agent phase failed: %s", errorMsg)
		}
		if job.FinishedAt == nil {
			job.FinishedAt = &now
		}
		job.Metrics.JobEnd = &now
		if job.Metrics.JobStart != nil {
			job.Metrics.TotalDuration = now.Sub(*job.Metrics.JobStart)
		}
		nc.releaseBuildLock(job)
	}
	job.UpdatedAt = time.Now()
	return job, nil
}

func (nc *Client) applyAgentPhase(job *types.Job) {
	phase := parseAgentPhaseFromLogs(job.Logs.Agent)
	switch phase {
	case types.StatusReviewing:
		job.Status = types.StatusReviewing
		job.CurrentPhase = "review"
	default:
		if job.Status != types.StatusReviewing {
			job.Status = types.StatusAgentRunning
			job.CurrentPhase = "agent"
		}
	}
}

func (nc *Client) storeAgentResult(job *types.Job) {
	result, err := parseAgentResultFromLogs(job.Logs.Agent)
	if err != nil {
		nc.logger.WithError(err).WithField("job_id", job.ID).Warn("Failed to parse JOBFORGE_RESULT")
		return
	}
	if result != nil {
		job.AgentResult = result
	}
}

func (nc *Client) captureAgentLogs(job *types.Job) error {
	if job.AgentJobID == "" {
		return nil
	}
	logs, err := nc.getWindowedJobLogs(job.AgentJobID, "agent")
	if err != nil {
		return err
	}
	job.Logs.Agent = truncateLogLines(logs, maxStoredLogBytes)
	return nil
}

// FollowUpBuildJobConfig builds a normal build job for the agent's pushed branch.
// It returns nil when the follow-up is disabled or the checkout has no Dockerfile.
func FollowUpBuildJobConfig(parent *types.Job) (*types.JobConfig, error) {
	if parent == nil || !parent.Config.IsAgent() {
		return nil, nil
	}
	agent := parent.Config.Agent
	if agent == nil || agent.FollowUpBuild == nil || !agent.FollowUpBuild.Enabled {
		return nil, nil
	}
	if parent.AgentResult == nil || !parent.AgentResult.DockerfilePresent || parent.AgentResult.Branch == "" {
		return nil, nil
	}
	follow := agent.FollowUpBuild
	cfg := &types.JobConfig{
		Kind:                    types.JobKindBuild,
		Owner:                   parent.Config.Owner,
		RepoURL:                 parent.Config.RepoURL,
		GitRef:                  parent.AgentResult.Branch,
		GitCredentialsPath:      parent.Config.GitCredentialsPath,
		DockerfilePath:          follow.DockerfilePath,
		DockerfileContext:       follow.DockerfileContext,
		ImageName:               follow.ImageName,
		ImageTags:               append([]string{}, follow.ImageTags...),
		RegistryURL:             follow.RegistryURL,
		RegistryCredentialsPath: follow.RegistryCredentialsPath,
		Test:                    follow.Test,
		WebhookURL:              parent.Config.WebhookURL,
		WebhookSecret:           parent.Config.WebhookSecret,
		WebhookOnSuccess:        parent.Config.WebhookOnSuccess,
		WebhookOnFailure:        parent.Config.WebhookOnFailure,
		WebhookHeaders:          parent.Config.WebhookHeaders,
	}
	if cfg.RegistryCredentialsPath == "" {
		cfg.RegistryCredentialsPath = parent.Config.RegistryCredentialsPath
	}
	if follow.BuildTimeout.Std() > 0 {
		timeout := follow.BuildTimeout.Std()
		cfg.BuildTimeout = &timeout
	}
	return cfg, nil
}

func parseAgentResultFromLogs(logs []string) (*types.AgentResult, error) {
	var last string
	for _, line := range logs {
		content := strings.TrimSpace(logLineContent(line))
		if strings.HasPrefix(content, "JOBFORGE_RESULT ") {
			last = strings.TrimSpace(strings.TrimPrefix(content, "JOBFORGE_RESULT "))
		}
	}
	if last == "" {
		return nil, nil
	}
	var result types.AgentResult
	if err := json.Unmarshal([]byte(last), &result); err != nil {
		return nil, fmt.Errorf("parse JOBFORGE_RESULT: %w", err)
	}
	return &result, nil
}

func parseAgentPhaseFromLogs(logs []string) types.JobStatus {
	var phase types.JobStatus
	for _, line := range logs {
		content := strings.TrimSpace(logLineContent(line))
		if !strings.HasPrefix(content, "JOBFORGE_PHASE ") {
			continue
		}
		switch strings.TrimSpace(strings.TrimPrefix(content, "JOBFORGE_PHASE ")) {
		case "REVIEWING":
			phase = types.StatusReviewing
		case "AGENT_RUNNING":
			phase = types.StatusAgentRunning
		}
	}
	return phase
}

func (nc *Client) getWindowedJobLogs(nomadJobID, phase string) ([]string, error) {
	allocs, _, err := nc.client.Jobs().Allocations(nomadJobID, false, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get allocations for job %s: %w", nomadJobID, err)
	}
	if len(allocs) == 0 {
		return []string{"No allocations found - job may have failed to schedule"}, nil
	}

	var all []string
	for _, allocStub := range allocs {
		alloc, _, err := nc.client.Allocations().Info(allocStub.ID, nil)
		if err != nil {
			nc.logger.WithError(err).WithField("alloc_id", allocStub.ID).Warn("Failed to read allocation for agent logs")
			continue
		}
		taskNames := make([]string, 0, len(alloc.TaskStates))
		for name := range alloc.TaskStates {
			taskNames = append(taskNames, name)
		}
		if len(taskNames) == 0 {
			taskNames = []string{"main"}
		}
		for _, taskName := range taskNames {
			for _, logType := range []string{"stdout", "stderr"} {
				head, truncated, err := nc.readTaskLogWindow(alloc, taskName, logType, "start", 0, agentLogHeadBytes)
				if err != nil {
					nc.logger.WithError(err).WithFields(logrus.Fields{
						"alloc_id": alloc.ID,
						"task":     taskName,
						"log_type": logType,
					}).Warn("Failed to read agent log head")
					continue
				}
				content := head
				if truncated {
					tail, _, tailErr := nc.readTaskLogWindow(alloc, taskName, logType, "end", -agentLogTailBytes, agentLogTailBytes)
					if tailErr != nil {
						nc.logger.WithError(tailErr).WithField("task", taskName).Warn("Failed to read agent log tail")
					} else if tail != "" {
						content = head + "\n... log truncated; tail follows ...\n" + tail
					}
				}
				for _, line := range splitLogLines(content) {
					all = append(all, fmt.Sprintf("[%s/%s] %s", taskName, logType, line))
				}
			}
		}
	}
	if len(all) == 0 {
		return []string{fmt.Sprintf("No %s logs were available yet", phase)}, nil
	}
	return all, nil
}

func (nc *Client) readTaskLogWindow(alloc *nomadapi.Allocation, taskName, logType, origin string, offset int64, maxBytes int) (string, bool, error) {
	cancelCh := make(chan struct{})
	defer close(cancelCh)

	frames, errCh := nc.client.AllocFS().Logs(alloc, false, taskName, logType, origin, offset, cancelCh, nil)
	if frames == nil {
		if errCh != nil {
			err := <-errCh
			if err != nil {
				return "", false, err
			}
		}
		return "", false, fmt.Errorf("log stream was not started")
	}

	var buf strings.Builder
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return buf.String(), false, nil
			}
			if frame != nil && len(frame.Data) > 0 {
				buf.Write(frame.Data)
				if maxBytes > 0 && buf.Len() >= maxBytes {
					return buf.String(), true, nil
				}
			}
		case err, ok := <-errCh:
			if !ok || err == nil {
				return buf.String(), false, nil
			}
			if buf.Len() > 0 {
				return buf.String(), buf.Len() >= maxBytes, nil
			}
			return "", false, err
		case <-timer.C:
			return buf.String(), buf.Len() >= maxBytes || buf.Len() > 0, nil
		}
	}
}

func mustJSON(value interface{}) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func logFileCount(configured int) int {
	if configured <= 0 {
		return 5
	}
	return configured
}
