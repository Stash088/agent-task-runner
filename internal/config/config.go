// Package config loads the agent-task-runner service configuration from
// environment variables, applies defaults, and validates the result.
//
// The configuration is intentionally environment-driven (no viper, no
// external config files): every value can be overridden via an env var, and
// everything with a sane default falls back to it. Only API_KEY and
// WORKER_IMAGE are mandatory.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Default values for every overridable setting. They double as documentation
// of the supported environment variables.
const (
	DefaultListenAddr        = ":8080"
	DefaultWorkerNamespace   = "agent-tasks"
	DefaultLLMSecretName     = "agent-task-llm"
	DefaultMaxConcurrentJobs = 3
	DefaultTimeoutSeconds    = 1800
	DefaultMaxTimeoutSeconds = 7200
	DefaultModel             = "deepseek-v4.1-flash"
	DefaultProvider          = "opencode-go"
	DefaultToolsets          = "web"
	DefaultAllowedProviders  = "opencode-go,zai"
	// Model names are restricted to a closed regex. The `|` alternation is
	// escaped nowhere: this is a Go regexp, not a shell string.
	DefaultAllowedModels   = `^(deepseek-v4\.[0-9](-[a-z]+)?|glm-5[.0-9]*(-flash)?|qwen3\.[0-9](-flash)?)$`
	DefaultAllowedToolsets = "web,files,terminal,memory"
	DefaultCPU             = "500m"
	DefaultMemory          = "1Gi"
	DefaultMaxCPU          = "2"
	DefaultMaxMemory       = "4Gi"
	DefaultMaxLogBytes     = 262144
	DefaultTTLSeconds      = 3600
	DefaultShutdownTimeout = 15
)

// Lower bounds used both for config sanity checks and request clamping.
const (
	MinTimeoutSeconds = 60
	MinCPU            = "100m"
	MinMemory         = "256Mi"
)

// Config is the fully resolved service configuration. Fields that hold parsed
// values (quantities, regexp, csv lists) are stored in their parsed form so
// that the hot path never re-parses them.
type Config struct {
	ListenAddr             string
	APIKey                 string
	WorkerNamespace        string
	WorkerImage            string
	LLMSecretName          string
	MaxConcurrentJobs      int
	DefaultTimeoutSeconds  int
	MaxTimeoutSeconds      int
	DefaultModel           string
	DefaultProvider        string
	DefaultToolsets        string
	AllowedProviders       []string
	AllowedModels          *regexp.Regexp
	AllowedToolsets        []string
	DefaultCPU             resource.Quantity
	DefaultMemory          resource.Quantity
	MaxCPU                 resource.Quantity
	MaxMemory              resource.Quantity
	MaxLogBytes            int64
	TTLSeconds             int32
	ShutdownTimeoutSeconds int
}

// Load reads the environment, applies defaults, parses structured values, and
// validates the result. It returns an error describing the first problem.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:       getenv("LISTEN_ADDR", DefaultListenAddr),
		APIKey:           os.Getenv("API_KEY"),
		WorkerNamespace:  getenv("WORKER_NAMESPACE", DefaultWorkerNamespace),
		WorkerImage:      os.Getenv("WORKER_IMAGE"),
		LLMSecretName:    getenv("LLM_SECRET_NAME", DefaultLLMSecretName),
		DefaultModel:     getenv("DEFAULT_MODEL", DefaultModel),
		DefaultProvider:  getenv("DEFAULT_PROVIDER", DefaultProvider),
		DefaultToolsets:  getenv("DEFAULT_TOOLSETS", DefaultToolsets),
		AllowedProviders: splitCSV(getenv("ALLOWED_PROVIDERS", DefaultAllowedProviders)),
		AllowedToolsets:  splitCSV(getenv("ALLOWED_TOOLSETS", DefaultAllowedToolsets)),
	}

	var err error
	if cfg.MaxConcurrentJobs, err = getenvInt("MAX_CONCURRENT_JOBS", DefaultMaxConcurrentJobs); err != nil {
		return nil, err
	}
	if cfg.DefaultTimeoutSeconds, err = getenvInt("DEFAULT_TIMEOUT_SECONDS", DefaultTimeoutSeconds); err != nil {
		return nil, err
	}
	if cfg.MaxTimeoutSeconds, err = getenvInt("MAX_TIMEOUT_SECONDS", DefaultMaxTimeoutSeconds); err != nil {
		return nil, err
	}
	if cfg.MaxLogBytes, err = getenvInt64("MAX_LOG_BYTES", DefaultMaxLogBytes); err != nil {
		return nil, err
	}
	var ttl int
	if ttl, err = getenvInt("TTL_SECONDS", DefaultTTLSeconds); err != nil {
		return nil, err
	}
	cfg.TTLSeconds = int32(ttl)
	if cfg.ShutdownTimeoutSeconds, err = getenvInt("SHUTDOWN_TIMEOUT_SECONDS", DefaultShutdownTimeout); err != nil {
		return nil, err
	}

	if cfg.AllowedModels, err = regexp.Compile(getenv("ALLOWED_MODELS", DefaultAllowedModels)); err != nil {
		return nil, fmt.Errorf("invalid ALLOWED_MODELS regex: %w", err)
	}
	if cfg.DefaultCPU, err = resource.ParseQuantity(getenv("DEFAULT_CPU", DefaultCPU)); err != nil {
		return nil, fmt.Errorf("invalid DEFAULT_CPU: %w", err)
	}
	if cfg.DefaultMemory, err = resource.ParseQuantity(getenv("DEFAULT_MEMORY", DefaultMemory)); err != nil {
		return nil, fmt.Errorf("invalid DEFAULT_MEMORY: %w", err)
	}
	if cfg.MaxCPU, err = resource.ParseQuantity(getenv("MAX_CPU", DefaultMaxCPU)); err != nil {
		return nil, fmt.Errorf("invalid MAX_CPU: %w", err)
	}
	if cfg.MaxMemory, err = resource.ParseQuantity(getenv("MAX_MEMORY", DefaultMaxMemory)); err != nil {
		return nil, fmt.Errorf("invalid MAX_MEMORY: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.APIKey == "" {
		return fmt.Errorf("API_KEY is required")
	}
	if c.WorkerImage == "" {
		return fmt.Errorf("WORKER_IMAGE is required")
	}
	if c.WorkerNamespace == "" {
		return fmt.Errorf("WORKER_NAMESPACE must not be empty")
	}
	if c.MaxConcurrentJobs < 1 {
		return fmt.Errorf("MAX_CONCURRENT_JOBS must be >= 1, got %d", c.MaxConcurrentJobs)
	}
	if c.MaxTimeoutSeconds < MinTimeoutSeconds {
		return fmt.Errorf("MAX_TIMEOUT_SECONDS must be >= %d, got %d", MinTimeoutSeconds, c.MaxTimeoutSeconds)
	}
	if c.DefaultTimeoutSeconds > c.MaxTimeoutSeconds {
		return fmt.Errorf("DEFAULT_TIMEOUT_SECONDS (%d) must not exceed MAX_TIMEOUT_SECONDS (%d)",
			c.DefaultTimeoutSeconds, c.MaxTimeoutSeconds)
	}
	if c.MaxLogBytes < 1 {
		return fmt.Errorf("MAX_LOG_BYTES must be >= 1, got %d", c.MaxLogBytes)
	}
	if c.TTLSeconds < 0 {
		return fmt.Errorf("TTL_SECONDS must be >= 0, got %d", c.TTLSeconds)
	}
	if !c.AllowedModels.MatchString(c.DefaultModel) {
		return fmt.Errorf("DEFAULT_MODEL %q does not match ALLOWED_MODELS", c.DefaultModel)
	}
	if !containsString(c.AllowedProviders, c.DefaultProvider) {
		return fmt.Errorf("DEFAULT_PROVIDER %q is not in ALLOWED_PROVIDERS", c.DefaultProvider)
	}
	for _, ts := range splitCSV(c.DefaultToolsets) {
		if !containsString(c.AllowedToolsets, ts) {
			return fmt.Errorf("DEFAULT_TOOLSETS value %q is not in ALLOWED_TOOLSETS", ts)
		}
	}
	if c.MaxCPU.Cmp(resource.MustParse(MinCPU)) < 0 {
		return fmt.Errorf("MAX_CPU must be >= %s, got %s", MinCPU, c.MaxCPU.String())
	}
	if c.MaxMemory.Cmp(resource.MustParse(MinMemory)) < 0 {
		return fmt.Errorf("MAX_MEMORY must be >= %s, got %s", MinMemory, c.MaxMemory.String())
	}
	if c.DefaultCPU.Cmp(resource.MustParse(MinCPU)) < 0 {
		return fmt.Errorf("DEFAULT_CPU must be >= %s, got %s", MinCPU, c.DefaultCPU.String())
	}
	if c.DefaultCPU.Cmp(c.MaxCPU) > 0 {
		return fmt.Errorf("DEFAULT_CPU (%s) must not exceed MAX_CPU (%s)", c.DefaultCPU.String(), c.MaxCPU.String())
	}
	if c.DefaultMemory.Cmp(resource.MustParse(MinMemory)) < 0 {
		return fmt.Errorf("DEFAULT_MEMORY must be >= %s, got %s", MinMemory, c.DefaultMemory.String())
	}
	if c.DefaultMemory.Cmp(c.MaxMemory) > 0 {
		return fmt.Errorf("DEFAULT_MEMORY (%s) must not exceed MAX_MEMORY (%s)", c.DefaultMemory.String(), c.MaxMemory.String())
	}
	return nil
}

// getenv returns the environment value or the default when the variable is
// unset or empty.
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, v, err)
	}
	return n, nil
}

func getenvInt64(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, v, err)
	}
	return n, nil
}

// splitCSV splits a comma-separated list, trimming whitespace and dropping
// empty entries.
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

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
