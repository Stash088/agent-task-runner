package runner

import (
	"regexp"

	"github.com/Stash088/agent-task-runner/internal/config"

	"k8s.io/apimachinery/pkg/api/resource"
)

// testConfig returns a fully-populated configuration for tests. It uses the
// same digest-pinned image referenced by the spike so golden tests match
// reality. No secrets are involved.
func testConfig() *config.Config {
	return &config.Config{
		ListenAddr:             ":8080",
		APIKey:                 "test-key",
		WorkerNamespace:        "agent-tasks",
		WorkerImage:            "nousresearch/hermes-agent@sha256:adb7890916de63245523492be8f68f9d2095fecbec4e2a6dab7960381f0fc002",
		LLMSecretName:          "agent-task-llm",
		MaxConcurrentJobs:      3,
		DefaultTimeoutSeconds:  1800,
		MaxTimeoutSeconds:      7200,
		DefaultModel:           "deepseek-v4.1-flash",
		DefaultProvider:        "opencode-go",
		DefaultToolsets:        "web",
		AllowedProviders:       []string{"opencode-go", "zai"},
		AllowedModels:          regexp.MustCompile(config.DefaultAllowedModels),
		AllowedToolsets:        []string{"web", "files", "terminal", "memory"},
		DefaultCPU:             resource.MustParse(config.DefaultCPU),
		DefaultMemory:          resource.MustParse(config.DefaultMemory),
		MaxCPU:                 resource.MustParse("2"),
		MaxMemory:              resource.MustParse("4Gi"),
		MaxLogBytes:            262144,
		TTLSeconds:             3600,
		ShutdownTimeoutSeconds: 15,
	}
}
