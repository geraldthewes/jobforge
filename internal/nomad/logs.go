package nomad

import (
	"fmt"
	"strings"
)

// maxStoredLogBytes keeps a Consul job record under the ~512KB value limit.
const maxStoredLogBytes = 256 * 1024

const (
	agentLogHeadBytes = 96 * 1024
	agentLogTailBytes = 96 * 1024
)

// truncateLogLines keeps the head and tail of a log when it exceeds maxBytes.
func truncateLogLines(lines []string, maxBytes int) []string {
	if maxBytes <= 0 || len(lines) == 0 {
		return lines
	}
	total := 0
	for _, line := range lines {
		total += len(line) + 1
	}
	if total <= maxBytes {
		return lines
	}

	marker := fmt.Sprintf("... log truncated to %d bytes (original %d bytes); showing head and tail ...", maxBytes, total)
	budget := maxBytes - len(marker) - 1
	if budget < 2 {
		return []string{marker}
	}
	headBudget := budget / 2
	tailBudget := budget - headBudget

	var head []string
	used := 0
	headEnd := 0
	for i, line := range lines {
		need := len(line) + 1
		if used+need > headBudget {
			break
		}
		head = append(head, line)
		used += need
		headEnd = i + 1
	}

	var tail []string
	used = 0
	for i := len(lines) - 1; i >= headEnd; i-- {
		line := lines[i]
		need := len(line) + 1
		if used+need > tailBudget {
			break
		}
		tail = append(tail, line)
		used += need
	}
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}

	out := make([]string, 0, len(head)+1+len(tail))
	out = append(out, head...)
	out = append(out, marker)
	out = append(out, tail...)
	return out
}

func splitLogLines(content string) []string {
	if content == "" {
		return nil
	}
	raw := strings.Split(content, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func logLineContent(line string) string {
	if strings.HasPrefix(line, "[") {
		if idx := strings.Index(line, "] "); idx != -1 {
			return line[idx+2:]
		}
	}
	return line
}
