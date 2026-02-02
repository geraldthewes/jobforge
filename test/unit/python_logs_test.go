package unit

import (
	"strings"
	"testing"
)

// extractPythonTestOutput mirrors the function in internal/server/server.go
// for testing purposes. This ensures the logic is correct.
func extractPythonTestOutput(logs []string) []string {
	var pythonOutput []string
	inPythonSection := false
	for _, line := range logs {
		if strings.HasPrefix(line, "=== Python Test Output") {
			inPythonSection = true
		}
		if inPythonSection {
			pythonOutput = append(pythonOutput, line)
		}
	}
	return pythonOutput
}

func TestExtractPythonTestOutput(t *testing.T) {
	tests := []struct {
		name           string
		logs           []string
		expectedOutput []string
	}{
		{
			name: "extracts stdout and stderr python output",
			logs: []string{
				"[main/stdout] Swagger UI is available at http://0.0.0.0:8000/docs",
				"[main/stdout] INFO: Starting server...",
				"[main/stderr] INFO: Model loaded successfully",
				"=== Python Test Output (stdout) ===",
				"============================= test session starts ==============================",
				"collected 5 items",
				"test_api_integration.py::test_health_endpoint PASSED",
				"============================= 5 passed in 12.34s ===============================",
				"=== Python Test Output (stderr) ===",
				"Some debug output",
			},
			expectedOutput: []string{
				"=== Python Test Output (stdout) ===",
				"============================= test session starts ==============================",
				"collected 5 items",
				"test_api_integration.py::test_health_endpoint PASSED",
				"============================= 5 passed in 12.34s ===============================",
				"=== Python Test Output (stderr) ===",
				"Some debug output",
			},
		},
		{
			name: "returns nil when no python output present",
			logs: []string{
				"[main/stdout] Swagger UI is available",
				"[main/stderr] INFO: Server started",
			},
			expectedOutput: nil,
		},
		{
			name:           "handles empty logs",
			logs:           []string{},
			expectedOutput: nil,
		},
		{
			name:           "handles nil logs",
			logs:           nil,
			expectedOutput: nil,
		},
		{
			name: "extracts only stdout python output",
			logs: []string{
				"[main/stdout] Service logs",
				"=== Python Test Output (stdout) ===",
				"test output line 1",
				"test output line 2",
			},
			expectedOutput: []string{
				"=== Python Test Output (stdout) ===",
				"test output line 1",
				"test output line 2",
			},
		},
		{
			name: "python output at end of logs",
			logs: []string{
				"[build/stdout] Building...",
				"[test/stdout] Testing...",
				"=== Python Test Output (stdout) ===",
				"All tests passed",
			},
			expectedOutput: []string{
				"=== Python Test Output (stdout) ===",
				"All tests passed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractPythonTestOutput(tt.logs)

			if tt.expectedOutput == nil {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
				return
			}

			if len(result) != len(tt.expectedOutput) {
				t.Errorf("expected %d lines, got %d", len(tt.expectedOutput), len(result))
				return
			}

			for i, line := range result {
				if line != tt.expectedOutput[i] {
					t.Errorf("line %d: expected %q, got %q", i, tt.expectedOutput[i], line)
				}
			}
		})
	}
}

func TestLogMergePreservesPythonOutput(t *testing.T) {
	// Simulates the scenario where:
	// 1. ReportTestResult appends python output to cached logs
	// 2. GetLogs fetches fresh Nomad logs (which don't have python output)
	// 3. The merge logic should preserve the python output

	// Original cached logs (after ReportTestResult)
	cachedLogs := []string{
		"[main/stdout] Service started",
		"[main/stderr] INFO: Ready",
		"=== Python Test Output (stdout) ===",
		"============================= test session starts ==============================",
		"test_example.py::test_one PASSED",
		"============================= 1 passed in 0.5s ================================",
	}

	// Fresh logs from Nomad (no python output)
	nomadLogs := []string{
		"[main/stdout] Service started",
		"[main/stderr] INFO: Ready",
		"[main/stdout] Some new log line",
	}

	// Extract python output from cached logs
	pythonOutput := extractPythonTestOutput(cachedLogs)

	// Merge: nomad logs + preserved python output
	mergedLogs := append(nomadLogs, pythonOutput...)

	// Verify python output is preserved
	if len(mergedLogs) != len(nomadLogs)+len(pythonOutput) {
		t.Errorf("expected %d merged lines, got %d", len(nomadLogs)+len(pythonOutput), len(mergedLogs))
	}

	// Verify the last lines are the python output
	expectedLastLine := "============================= 1 passed in 0.5s ================================"
	if mergedLogs[len(mergedLogs)-1] != expectedLastLine {
		t.Errorf("last line should be %q, got %q", expectedLastLine, mergedLogs[len(mergedLogs)-1])
	}

	// Verify python header is in the merged logs
	found := false
	for _, line := range mergedLogs {
		if line == "=== Python Test Output (stdout) ===" {
			found = true
			break
		}
	}
	if !found {
		t.Error("python output header not found in merged logs")
	}
}
