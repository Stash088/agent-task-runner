package runner

import (
	"fmt"
	"strings"

	"github.com/Stash088/agent-task-runner/internal/config"

	"k8s.io/apimachinery/pkg/api/resource"
)

// MaxPromptBytes is the upper bound on the prompt size, matching the MCP
// contract "prompt: 1..262144 bytes".
const MaxPromptBytes = 262144

// ValidationError marks an invalid_argument: the request failed allowlist
// validation and must never be turned into a Job. Its presence lets callers
// (e.g. the MCP layer) distinguish it from busy/internal errors.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return "invalid_argument: " + e.Reason
	}
	return fmt.Sprintf("invalid_argument %s: %s", e.Field, e.Reason)
}

func invalid(field, format string, args ...any) *ValidationError {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Validate checks every allowlisted input against the configuration. Where the
// plan specifies a clamp rather than a rejection (timeout, cpu, memory) it
// normalises req in place; where it specifies a rejection (prompt, model,
// provider, toolsets) it returns a *ValidationError on the first failure.
//
// Injection-bearing prompts (`"; rm -rf /`, `$(id)`, backticks, newlines) are
// deliberately accepted: they travel to the pod only through the TASK_PROMPT
// environment variable and are never interpolated into a shell command.
func Validate(req *TaskRunRequest, cfg *config.Config) error {
	if req == nil {
		return invalid("request", "is nil")
	}

	if req.Prompt == "" {
		return invalid("prompt", "must not be empty")
	}
	if len(req.Prompt) > MaxPromptBytes {
		return invalid("prompt", "exceeds %d bytes", MaxPromptBytes)
	}
	if strings.ContainsRune(req.Prompt, 0) {
		return invalid("prompt", "contains NUL byte")
	}

	if req.Model != "" && !cfg.AllowedModels.MatchString(req.Model) {
		return invalid("model", "%q does not match ALLOWED_MODELS", req.Model)
	}

	if req.Provider != "" && !contains(cfg.AllowedProviders, req.Provider) {
		return invalid("provider", "%q is not in ALLOWED_PROVIDERS", req.Provider)
	}

	if req.Toolsets != "" {
		for _, t := range splitCSV(req.Toolsets) {
			if t != "" && !contains(cfg.AllowedToolsets, t) {
				return invalid("toolsets", "%q is not in ALLOWED_TOOLSETS", t)
			}
		}
	}

	// timeout: clamp to [MinTimeoutSeconds, MaxTimeoutSeconds].
	if req.TimeoutSeconds != 0 {
		if req.TimeoutSeconds < config.MinTimeoutSeconds {
			req.TimeoutSeconds = config.MinTimeoutSeconds
		}
		if req.TimeoutSeconds > cfg.MaxTimeoutSeconds {
			req.TimeoutSeconds = cfg.MaxTimeoutSeconds
		}
	}

	if req.CPU != "" {
		q, err := clampQuantity(req.CPU, config.MinCPU, cfg.MaxCPU)
		if err != nil {
			return invalid("cpu", "%q is not a valid quantity: %v", req.CPU, err)
		}
		req.CPU = q.String()
	}

	if req.Memory != "" {
		q, err := clampQuantity(req.Memory, config.MinMemory, cfg.MaxMemory)
		if err != nil {
			return invalid("memory", "%q is not a valid quantity: %v", req.Memory, err)
		}
		req.Memory = q.String()
	}

	return nil
}

// clampQuantity parses raw and clamps it into [minStr, max]. It returns the
// canonical string form of the result (or an error for an unparseable value).
func clampQuantity(raw, minStr string, max resource.Quantity) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return resource.Quantity{}, err
	}
	if min := resource.MustParse(minStr); q.Cmp(min) < 0 {
		q = min
	}
	if q.Cmp(max) > 0 {
		q = max
	}
	return q, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
