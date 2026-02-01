package unit

import (
	"encoding/json"
	"testing"

	"nomad-mcp-builder/internal/mcp"
	"nomad-mcp-builder/pkg/types"
)

// TestMCPProtocolTypes tests the MCP protocol types and structures
func TestMCPProtocolTypes(t *testing.T) {
	// Test MCPRequest creation
	req := mcp.MCPRequest{
		JSONRPC: "2.0",
		ID:      "test-123",
		Method:  "tools/call",
		Params: map[string]interface{}{
			"name": "submitJob",
			"arguments": map[string]interface{}{
				"owner":        "test-user",
				"repo_url":     "https://github.com/test/repo.git",
				"registry_url": "docker.io/test",
				"image_name":   "test-app",
				"image_tags":   []string{"latest"},
			},
		},
	}

	if req.JSONRPC != "2.0" {
		t.Errorf("Expected JSONRPC version 2.0, got %s", req.JSONRPC)
	}

	if req.Method != "tools/call" {
		t.Errorf("Expected method 'tools/call', got %s", req.Method)
	}

	// Test MCPResponse creation
	result := map[string]interface{}{
		"job_id": "test-job-123",
		"status": "PENDING",
	}
	resp := mcp.NewMCPResponse(req.ID, result)

	if resp.JSONRPC != "2.0" {
		t.Errorf("Expected JSONRPC version 2.0, got %s", resp.JSONRPC)
	}

	if resp.ID != req.ID {
		t.Errorf("Expected response ID to match request ID")
	}

	if resp.Error != nil {
		t.Errorf("Expected no error in successful response, got %+v", resp.Error)
	}
}

// TestMCPErrorResponse tests MCP error response creation
func TestMCPErrorResponse(t *testing.T) {
	errorResp := mcp.NewMCPErrorResponse("test-456", mcp.MCPErrorInvalidParams, "Missing required parameter", "owner is required")

	if errorResp.Error == nil {
		t.Fatal("Expected error in error response")
	}

	if errorResp.Error.Code != mcp.MCPErrorInvalidParams {
		t.Errorf("Expected error code %d, got %d", mcp.MCPErrorInvalidParams, errorResp.Error.Code)
	}

	if errorResp.Error.Message != "Missing required parameter" {
		t.Errorf("Expected error message 'Missing required parameter', got %s", errorResp.Error.Message)
	}

	if errorResp.Result != nil {
		t.Errorf("Expected no result in error response, got %+v", errorResp.Result)
	}
}

// TestMCPToolDefinitions tests that all required MCP tools are defined correctly
func TestMCPToolDefinitions(t *testing.T) {
	tools := mcp.GetTools()

	// Expected tools
	expectedTools := []string{
		"submitJob",
		"getStatus",
		"getLogs",
		"killJob",
		"cleanup",
		"getHistory",
		"purgeFailedJob",
		"pruneStorage",
	}

	if len(tools) != len(expectedTools) {
		t.Errorf("Expected %d tools, got %d", len(expectedTools), len(tools))
	}

	// Check each tool exists and has required fields
	toolMap := make(map[string]mcp.Tool)
	for _, tool := range tools {
		toolMap[tool.Name] = tool
	}

	for _, expectedTool := range expectedTools {
		tool, exists := toolMap[expectedTool]
		if !exists {
			t.Errorf("Expected tool %s not found", expectedTool)
			continue
		}

		if tool.Description == "" {
			t.Errorf("Tool %s missing description", expectedTool)
		}

		if tool.InputSchema.Type != "object" {
			t.Errorf("Tool %s schema type should be 'object', got %s", expectedTool, tool.InputSchema.Type)
		}

		if tool.InputSchema.Properties == nil {
			t.Errorf("Tool %s missing properties in schema", expectedTool)
		}
	}
}

// TestMCPSubmitJobValidation tests that submitJob MCP tool validation matches REST interface
func TestMCPSubmitJobValidation(t *testing.T) {
	tools := mcp.GetTools()
	var submitJobTool mcp.Tool

	// Find submitJob tool
	for _, tool := range tools {
		if tool.Name == "submitJob" {
			submitJobTool = tool
			break
		}
	}

	if submitJobTool.Name == "" {
		t.Fatal("submitJob tool not found")
	}

	// Test required fields match what MCP interface actually requires (after applying defaults)
	expectedRequired := []string{"owner", "repo_url", "image_name", "registry_url"}

	if len(submitJobTool.InputSchema.Required) != len(expectedRequired) {
		t.Errorf("Expected %d required fields, got %d", len(expectedRequired), len(submitJobTool.InputSchema.Required))
	}

	// Check each required field
	requiredMap := make(map[string]bool)
	for _, req := range submitJobTool.InputSchema.Required {
		requiredMap[req] = true
	}

	for _, expected := range expectedRequired {
		if !requiredMap[expected] {
			t.Errorf("Required field %s missing from MCP schema", expected)
		}
	}

	// Test that the schema includes all properties that validateJobConfig checks
	expectedProperties := []string{
		"owner", "repo_url", "git_ref", "dockerfile_path", "image_name",
		"image_tags", "registry_url", "git_credentials_path", "registry_credentials_path",
		"test_commands", "test_entry_point", "resource_limits",
	}

	for _, prop := range expectedProperties {
		if _, exists := submitJobTool.InputSchema.Properties[prop]; !exists {
			t.Errorf("Property %s missing from MCP schema", prop)
		}
	}
}

// TestMCPParameterConsistencyWithValidation tests that MCP parameters align with validation requirements
func TestMCPParameterConsistencyWithValidation(t *testing.T) {
	// Test cases that should pass validation
	validCases := []struct {
		name string
		args map[string]interface{}
	}{
		{
			name: "minimal valid config",
			args: map[string]interface{}{
				"owner":        "test-user",
				"repo_url":     "https://github.com/test/repo.git",
				"git_ref":      "main",
				"dockerfile_path": "Dockerfile",
				"image_name":   "test-app",
				"image_tags":   []interface{}{"latest"},
				"registry_url": "docker.io/test",
			},
		},
		{
			name: "config with all fields",
			args: map[string]interface{}{
				"owner":                     "test-user",
				"repo_url":                  "https://github.com/test/repo.git",
				"git_ref":                   "main",
				"git_credentials_path":      "secret/git-creds",
				"dockerfile_path":           "Dockerfile",
				"image_name":                "test-app",
				"image_tags":                []interface{}{"latest", "v1.0.0"},
				"registry_url":              "docker.io/test/app",
				"registry_credentials_path": "secret/registry-creds",
				"test_commands":             []interface{}{"npm test", "npm run e2e"},
				"test_entry_point":          true,
			},
		},
		{
			name: "empty image_tags (should default to job-id)",
			args: map[string]interface{}{
				"owner":        "test-user",
				"repo_url":     "https://github.com/test/repo.git",
				"git_ref":      "main",
				"dockerfile_path": "Dockerfile",
				"image_name":   "test-app",
				"image_tags":   []interface{}{},
				"registry_url": "docker.io/test",
			},
		},
	}

	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			// Convert MCP arguments to internal job config (similar to mcpSubmitJob)
			var jobConfig types.JobConfig

			if owner, ok := tc.args["owner"].(string); ok {
				jobConfig.Owner = owner
			}
			if repoURL, ok := tc.args["repo_url"].(string); ok {
				jobConfig.RepoURL = repoURL
			}
			if gitRef, ok := tc.args["git_ref"].(string); ok {
				jobConfig.GitRef = gitRef
			} else {
				jobConfig.GitRef = "main"
			}
			if gitCreds, ok := tc.args["git_credentials_path"].(string); ok {
				jobConfig.GitCredentialsPath = gitCreds
			} else {
				jobConfig.GitCredentialsPath = "secret/nomad/jobs/git-credentials"
			}
			if dockerfile, ok := tc.args["dockerfile_path"].(string); ok {
				jobConfig.DockerfilePath = dockerfile
			} else {
				jobConfig.DockerfilePath = "Dockerfile"
			}
			if imageName, ok := tc.args["image_name"].(string); ok {
				jobConfig.ImageName = imageName
			}
			if regURL, ok := tc.args["registry_url"].(string); ok {
				jobConfig.RegistryURL = regURL
			}
			if regCreds, ok := tc.args["registry_credentials_path"].(string); ok {
				jobConfig.RegistryCredentialsPath = regCreds
			} else {
				jobConfig.RegistryCredentialsPath = "secret/nomad/jobs/registry-credentials"
			}

			// Convert image tags
			if tagsInterface, ok := tc.args["image_tags"].([]interface{}); ok {
				for _, tag := range tagsInterface {
					if tagStr, ok := tag.(string); ok {
						jobConfig.ImageTags = append(jobConfig.ImageTags, tagStr)
					}
				}
			}

			// Convert test commands and entry point into Test config
			var testCommands []string
			var testEntryPoint bool

			if testCmdsInterface, ok := tc.args["test_commands"].([]interface{}); ok {
				for _, cmd := range testCmdsInterface {
					if cmdStr, ok := cmd.(string); ok {
						testCommands = append(testCommands, cmdStr)
					}
				}
			}

			if entryPoint, ok := tc.args["test_entry_point"].(bool); ok {
				testEntryPoint = entryPoint
			}

			// Create Test config if there are any test settings
			if len(testCommands) > 0 || testEntryPoint {
				jobConfig.Test = &types.TestConfig{
					Commands:   testCommands,
					EntryPoint: testEntryPoint,
				}
			}

			// This simulates the validateJobConfig call that should happen in mcpSubmitJob
			err := validateJobConfigMock(&jobConfig)
			if err != nil {
				t.Errorf("Valid config failed validation: %v", err)
			}
		})
	}

	// Test cases that should fail validation
	invalidCases := []struct {
		name        string
		args        map[string]interface{}
		expectedErr string
	}{
		{
			name:        "missing owner",
			args:        map[string]interface{}{
				"repo_url":     "https://github.com/test/repo.git",
				"registry_url": "docker.io/test",
				"image_name":   "test-app",
				"image_tags":   []interface{}{"latest"},
			},
			expectedErr: "owner is required",
		},
		{
			name:        "missing repo_url",
			args:        map[string]interface{}{
				"owner":        "test-user",
				"registry_url": "docker.io/test",
				"image_name":   "test-app",
				"image_tags":   []interface{}{"latest"},
			},
			expectedErr: "repo_url is required",
		},
		{
			name:        "missing image_name",
			args:        map[string]interface{}{
				"owner":        "test-user",
				"repo_url":     "https://github.com/test/repo.git",
				"registry_url": "docker.io/test",
				"image_tags":   []interface{}{"latest"},
			},
			expectedErr: "image_name is required",
		},
		{
			name:        "missing registry_url",
			args:        map[string]interface{}{
				"owner":      "test-user",
				"repo_url":   "https://github.com/test/repo.git",
				"image_name": "test-app",
				"image_tags": []interface{}{"latest"},
			},
			expectedErr: "registry_url is required",
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			// Convert MCP arguments to internal job config
			var jobConfig types.JobConfig

			if owner, ok := tc.args["owner"].(string); ok {
				jobConfig.Owner = owner
			}
			if repoURL, ok := tc.args["repo_url"].(string); ok {
				jobConfig.RepoURL = repoURL
			}
			if gitRef, ok := tc.args["git_ref"].(string); ok {
				jobConfig.GitRef = gitRef
			} else {
				jobConfig.GitRef = "main"
			}
			if gitCreds, ok := tc.args["git_credentials_path"].(string); ok {
				jobConfig.GitCredentialsPath = gitCreds
			} else {
				jobConfig.GitCredentialsPath = "secret/nomad/jobs/git-credentials"
			}
			if dockerfile, ok := tc.args["dockerfile_path"].(string); ok {
				jobConfig.DockerfilePath = dockerfile
			} else {
				jobConfig.DockerfilePath = "Dockerfile"
			}
			if imageName, ok := tc.args["image_name"].(string); ok {
				jobConfig.ImageName = imageName
			}
			if regURL, ok := tc.args["registry_url"].(string); ok {
				jobConfig.RegistryURL = regURL
			}
			if regCreds, ok := tc.args["registry_credentials_path"].(string); ok {
				jobConfig.RegistryCredentialsPath = regCreds
			} else {
				jobConfig.RegistryCredentialsPath = "secret/nomad/jobs/registry-credentials"
			}

			// Convert image tags
			if tagsInterface, ok := tc.args["image_tags"].([]interface{}); ok {
				for _, tag := range tagsInterface {
					if tagStr, ok := tag.(string); ok {
						jobConfig.ImageTags = append(jobConfig.ImageTags, tagStr)
					}
				}
			}

			// Convert test commands and entry point into Test config
			var testCommands []string
			var testEntryPoint bool

			if testCmdsInterface, ok := tc.args["test_commands"].([]interface{}); ok {
				for _, cmd := range testCmdsInterface {
					if cmdStr, ok := cmd.(string); ok {
						testCommands = append(testCommands, cmdStr)
					}
				}
			}

			if entryPoint, ok := tc.args["test_entry_point"].(bool); ok {
				testEntryPoint = entryPoint
			}

			// Create Test config if there are any test settings
			if len(testCommands) > 0 || testEntryPoint {
				jobConfig.Test = &types.TestConfig{
					Commands:   testCommands,
					EntryPoint: testEntryPoint,
				}
			}

			// This simulates the validateJobConfig call that should happen in mcpSubmitJob
			err := validateJobConfigMock(&jobConfig)
			if err == nil {
				t.Errorf("Expected validation error for %s", tc.name)
			} else if err.Error() != tc.expectedErr {
				t.Errorf("Expected error '%s', got '%s'", tc.expectedErr, err.Error())
			}
		})
	}
}

// TestMCPContentHelpers tests the MCP content creation helpers
func TestMCPContentHelpers(t *testing.T) {
	// Test text content creation
	textContent := mcp.NewMCPTextContent("Test message")
	if len(textContent) != 1 {
		t.Errorf("Expected 1 content item, got %d", len(textContent))
	}
	if textContent[0].Type != "text" {
		t.Errorf("Expected content type 'text', got %s", textContent[0].Type)
	}
	if textContent[0].Text != "Test message" {
		t.Errorf("Expected text 'Test message', got %s", textContent[0].Text)
	}

	// Test JSON content creation
	testData := map[string]interface{}{
		"job_id": "test-123",
		"status": "PENDING",
	}
	jsonContent := mcp.NewMCPJSONContent(testData)
	if len(jsonContent) != 1 {
		t.Errorf("Expected 1 content item, got %d", len(jsonContent))
	}
	if jsonContent[0].Type != "text" {
		t.Errorf("Expected content type 'text', got %s", jsonContent[0].Type)
	}

	// Verify the JSON is properly formatted
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(jsonContent[0].Text), &parsed); err != nil {
		t.Errorf("Failed to parse JSON content: %v", err)
	}
	if parsed["job_id"] != "test-123" {
		t.Errorf("Expected job_id 'test-123', got %v", parsed["job_id"])
	}
}

// TestMCPToolCallResult tests the ToolCallResult structure
func TestMCPToolCallResult(t *testing.T) {
	result := mcp.ToolCallResult{
		Content: mcp.NewMCPTextContent("Operation completed successfully"),
		IsError: false,
	}

	if result.IsError {
		t.Errorf("Expected IsError to be false, got true")
	}

	if len(result.Content) != 1 {
		t.Errorf("Expected 1 content item, got %d", len(result.Content))
	}

	// Test error result
	errorResult := mcp.ToolCallResult{
		Content: mcp.NewMCPTextContent("Operation failed"),
		IsError: true,
	}

	if !errorResult.IsError {
		t.Errorf("Expected IsError to be true, got false")
	}
}

// Mock validation function for testing (mirrors the actual validateJobConfig logic)
func validateJobConfigMock(config *types.JobConfig) error {
	// Required fields
	if config.Owner == "" {
		return &ValidationError{"owner is required"}
	}
	if config.RepoURL == "" {
		return &ValidationError{"repo_url is required"}
	}
	if config.GitRef == "" {
		return &ValidationError{"git_ref is required"}
	}
	if config.DockerfilePath == "" {
		return &ValidationError{"dockerfile_path is required"}
	}
	if config.ImageName == "" {
		return &ValidationError{"image_name is required"}
	}
	// image_tags is optional - will default to job-id if not provided
	if config.RegistryURL == "" {
		return &ValidationError{"registry_url is required"}
	}

	return nil
}

// ValidationError represents a validation error
type ValidationError struct {
	message string
}

func (e *ValidationError) Error() string {
	return e.message
}