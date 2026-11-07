package config

import (
	"strings"
	"testing"
)

// allEnvKeys lists every environment variable the Loader reads so tests can
// start from a clean slate regardless of the host environment.
var allEnvKeys = []string{
	"LISTEN_ADDR",
	"API_KEY",
	"WORKER_NAMESPACE",
	"WORKER_IMAGE",
	"LLM_SECRET_NAME",
	"MAX_CONCURRENT_JOBS",
	"DEFAULT_TIMEOUT_SECONDS",
	"MAX_TIMEOUT_SECONDS",
	"DEFAULT_MODEL",
	"DEFAULT_PROVIDER",
	"DEFAULT_TOOLSETS",
	"ALLOWED_PROVIDERS",
	"ALLOWED_MODELS",
	"ALLOWED_TOOLSETS",
	"DEFAULT_CPU",
	"DEFAULT_MEMORY",
	"MAX_CPU",
	"MAX_MEMORY",
	"MAX_LOG_BYTES",
	"TTL_SECONDS",
	"SHUTDOWN_TIMEOUT_SECONDS",
}

// setEnv clears all known keys and applies the provided overrides.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range allEnvKeys {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// requiredEnv returns the two mandatory values so valid-config tests stay DRY.
func requiredEnv() map[string]string {
	return map[string]string{
		"API_KEY":      "test-key",
		"WORKER_IMAGE": "nousresearch/hermes-agent@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, requiredEnv())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.WorkerNamespace != DefaultWorkerNamespace {
		t.Errorf("WorkerNamespace = %q, want %q", cfg.WorkerNamespace, DefaultWorkerNamespace)
	}
	if cfg.LLMSecretName != DefaultLLMSecretName {
		t.Errorf("LLMSecretName = %q, want %q", cfg.LLMSecretName, DefaultLLMSecretName)
	}
	if cfg.MaxConcurrentJobs != DefaultMaxConcurrentJobs {
		t.Errorf("MaxConcurrentJobs = %d, want %d", cfg.MaxConcurrentJobs, DefaultMaxConcurrentJobs)
	}
	if cfg.DefaultTimeoutSeconds != DefaultTimeoutSeconds {
		t.Errorf("DefaultTimeoutSeconds = %d, want %d", cfg.DefaultTimeoutSeconds, DefaultTimeoutSeconds)
	}
	if cfg.MaxTimeoutSeconds != DefaultMaxTimeoutSeconds {
		t.Errorf("MaxTimeoutSeconds = %d, want %d", cfg.MaxTimeoutSeconds, DefaultMaxTimeoutSeconds)
	}
	if cfg.DefaultModel != DefaultModel {
		t.Errorf("DefaultModel = %q, want %q", cfg.DefaultModel, DefaultModel)
	}
	if cfg.DefaultProvider != DefaultProvider {
		t.Errorf("DefaultProvider = %q, want %q", cfg.DefaultProvider, DefaultProvider)
	}
	if cfg.DefaultToolsets != DefaultToolsets {
		t.Errorf("DefaultToolsets = %q, want %q", cfg.DefaultToolsets, DefaultToolsets)
	}
	if cfg.MaxLogBytes != DefaultMaxLogBytes {
		t.Errorf("MaxLogBytes = %d, want %d", cfg.MaxLogBytes, DefaultMaxLogBytes)
	}
	if cfg.TTLSeconds != DefaultTTLSeconds {
		t.Errorf("TTLSeconds = %d, want %d", cfg.TTLSeconds, DefaultTTLSeconds)
	}
	if cfg.ShutdownTimeoutSeconds != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeoutSeconds = %d, want %d", cfg.ShutdownTimeoutSeconds, DefaultShutdownTimeout)
	}
	if cfg.MaxCPU.String() != DefaultMaxCPU {
		t.Errorf("MaxCPU = %s, want %s", cfg.MaxCPU.String(), DefaultMaxCPU)
	}
	if cfg.MaxMemory.String() != DefaultMaxMemory {
		t.Errorf("MaxMemory = %s, want %s", cfg.MaxMemory.String(), DefaultMaxMemory)
	}
	if cfg.DefaultCPU.String() != DefaultCPU {
		t.Errorf("DefaultCPU = %s, want %s", cfg.DefaultCPU.String(), DefaultCPU)
	}
	if cfg.DefaultMemory.String() != DefaultMemory {
		t.Errorf("DefaultMemory = %s, want %s", cfg.DefaultMemory.String(), DefaultMemory)
	}
	if cfg.APIKey != "test-key" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "test-key")
	}
}

func TestLoadMissingAPIKey(t *testing.T) {
	setEnv(t, map[string]string{"WORKER_IMAGE": "img@sha256:0000000000000000000000000000000000000000000000000000000000000000"})

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want missing API_KEY error")
	} else if !strings.Contains(err.Error(), "API_KEY") {
		t.Fatalf("Load() error = %v, want mention of API_KEY", err)
	}
}

func TestLoadMissingWorkerImage(t *testing.T) {
	setEnv(t, map[string]string{"API_KEY": "test-key"})

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want missing WORKER_IMAGE error")
	} else if !strings.Contains(err.Error(), "WORKER_IMAGE") {
		t.Fatalf("Load() error = %v, want mention of WORKER_IMAGE", err)
	}
}

func TestLoadCSVTrimsAndDropsEmpty(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{
		"ALLOWED_PROVIDERS": " opencode-go , zai ,, ,custom ",
		"ALLOWED_TOOLSETS":  " web, files ,,terminal ",
	}))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantProviders := []string{"opencode-go", "zai", "custom"}
	if len(cfg.AllowedProviders) != len(wantProviders) {
		t.Fatalf("AllowedProviders = %v, want %v", cfg.AllowedProviders, wantProviders)
	}
	for i := range wantProviders {
		if cfg.AllowedProviders[i] != wantProviders[i] {
			t.Errorf("AllowedProviders[%d] = %q, want %q", i, cfg.AllowedProviders[i], wantProviders[i])
		}
	}

	wantToolsets := []string{"web", "files", "terminal"}
	if len(cfg.AllowedToolsets) != len(wantToolsets) {
		t.Fatalf("AllowedToolsets = %v, want %v", cfg.AllowedToolsets, wantToolsets)
	}
	for i := range wantToolsets {
		if cfg.AllowedToolsets[i] != wantToolsets[i] {
			t.Errorf("AllowedToolsets[%d] = %q, want %q", i, cfg.AllowedToolsets[i], wantToolsets[i])
		}
	}
}

func TestLoadCustomModelsRegex(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{
		"ALLOWED_MODELS": `^(gpt-5|claude-4)$`,
		"DEFAULT_MODEL":  "gpt-5",
	}))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	for _, m := range []string{"gpt-5", "claude-4"} {
		if !cfg.AllowedModels.MatchString(m) {
			t.Errorf("AllowedModels should match %q", m)
		}
	}
	if cfg.AllowedModels.MatchString("gpt-4") {
		t.Error("AllowedModels should not match gpt-4")
	}
}

func TestLoadInvalidInt(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"MAX_CONCURRENT_JOBS": "not-an-int"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want parse error")
	} else if !strings.Contains(err.Error(), "MAX_CONCURRENT_JOBS") {
		t.Fatalf("Load() error = %v, want mention of MAX_CONCURRENT_JOBS", err)
	}
}

func TestLoadInvalidModelsRegex(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"ALLOWED_MODELS": "("}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want regex compile error")
	} else if !strings.Contains(err.Error(), "ALLOWED_MODELS") {
		t.Fatalf("Load() error = %v, want mention of ALLOWED_MODELS", err)
	}
}

func TestLoadInvalidMaxCPU(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"MAX_CPU": "abc"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want quantity parse error")
	} else if !strings.Contains(err.Error(), "MAX_CPU") {
		t.Fatalf("Load() error = %v, want mention of MAX_CPU", err)
	}
}

func TestLoadInvalidMaxMemory(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"MAX_MEMORY": "zzz"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want quantity parse error")
	} else if !strings.Contains(err.Error(), "MAX_MEMORY") {
		t.Fatalf("Load() error = %v, want mention of MAX_MEMORY", err)
	}
}

func TestLoadMaxTimeoutBelowMin(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"MAX_TIMEOUT_SECONDS": "30"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want MAX_TIMEOUT_SECONDS validation error")
	}
}

func TestLoadMaxCPUBelowMin(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"MAX_CPU": "50m"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want MAX_CPU validation error")
	}
}

func TestLoadNegativeTTL(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"TTL_SECONDS": "-1"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want TTL_SECONDS validation error")
	}
}

func TestLoadDefaultTimeoutExceedsMax(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{
		"DEFAULT_TIMEOUT_SECONDS": "1000",
		"MAX_TIMEOUT_SECONDS":     "600",
	}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_TIMEOUT_SECONDS validation error")
	}
}

func TestLoadDefaultCPUBelowMin(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"DEFAULT_CPU": "50m"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_CPU validation error")
	} else if !strings.Contains(err.Error(), "DEFAULT_CPU") {
		t.Fatalf("Load() error = %v, want mention of DEFAULT_CPU", err)
	}
}

func TestLoadDefaultCPUExceedsMax(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"DEFAULT_CPU": "3"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_CPU validation error")
	} else if !strings.Contains(err.Error(), "DEFAULT_CPU") {
		t.Fatalf("Load() error = %v, want mention of DEFAULT_CPU", err)
	}
}

func TestLoadDefaultMemoryBelowMin(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"DEFAULT_MEMORY": "64Mi"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_MEMORY validation error")
	} else if !strings.Contains(err.Error(), "DEFAULT_MEMORY") {
		t.Fatalf("Load() error = %v, want mention of DEFAULT_MEMORY", err)
	}
}

func TestLoadDefaultMemoryExceedsMax(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"DEFAULT_MEMORY": "8Gi"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_MEMORY validation error")
	} else if !strings.Contains(err.Error(), "DEFAULT_MEMORY") {
		t.Fatalf("Load() error = %v, want mention of DEFAULT_MEMORY", err)
	}
}

func TestLoadInvalidDefaultCPU(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"DEFAULT_CPU": "abc"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_CPU parse error")
	} else if !strings.Contains(err.Error(), "DEFAULT_CPU") {
		t.Fatalf("Load() error = %v, want mention of DEFAULT_CPU", err)
	}
}

func TestLoadInvalidDefaultMemory(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{"DEFAULT_MEMORY": "zzz"}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_MEMORY parse error")
	} else if !strings.Contains(err.Error(), "DEFAULT_MEMORY") {
		t.Fatalf("Load() error = %v, want mention of DEFAULT_MEMORY", err)
	}
}

func TestLoadDefaultModelNotAllowed(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{
		"ALLOWED_MODELS": `^gpt-5$`,
		"DEFAULT_MODEL":  "deepseek-v4.1-flash",
	}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_MODEL allowlist error")
	} else if !strings.Contains(err.Error(), "DEFAULT_MODEL") {
		t.Fatalf("Load() error = %v, want mention of DEFAULT_MODEL", err)
	}
}

func TestLoadDefaultProviderNotAllowed(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{
		"ALLOWED_PROVIDERS": "zai",
		"DEFAULT_PROVIDER":  "opencode-go",
	}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_PROVIDER allowlist error")
	} else if !strings.Contains(err.Error(), "DEFAULT_PROVIDER") {
		t.Fatalf("Load() error = %v, want mention of DEFAULT_PROVIDER", err)
	}
}

func TestLoadDefaultToolsetNotAllowed(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{
		"ALLOWED_TOOLSETS": "files,terminal",
		"DEFAULT_TOOLSETS": "web",
	}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_TOOLSETS allowlist error")
	} else if !strings.Contains(err.Error(), "DEFAULT_TOOLSETS") {
		t.Fatalf("Load() error = %v, want mention of DEFAULT_TOOLSETS", err)
	}
}

func TestLoadDefaultOneOfSeveralToolsetsNotAllowed(t *testing.T) {
	setEnv(t, merge(requiredEnv(), map[string]string{
		"ALLOWED_TOOLSETS": "web,files",
		"DEFAULT_TOOLSETS": "web,terminal",
	}))

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want DEFAULT_TOOLSETS allowlist error")
	} else if !strings.Contains(err.Error(), "DEFAULT_TOOLSETS") {
		t.Fatalf("Load() error = %v, want mention of DEFAULT_TOOLSETS", err)
	}
}

func merge(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
