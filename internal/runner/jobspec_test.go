package runner

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "update golden files")

// TestBuildJobGolden serialises a fully-populated Job with sigs.k8s.io/yaml and
// compares it byte-for-byte against testdata/job.golden.yaml. The golden file
// documents the exact worker spec from section 2 of the plan.
func TestBuildJobGolden(t *testing.T) {
	cfg := testConfig()
	req := &TaskRunRequest{
		Prompt:         `reply with exactly: OK"; rm -rf / # $(id)`,
		Model:          "deepseek-v4.1-flash",
		Provider:       "opencode-go",
		Toolsets:       "web,files",
		TimeoutSeconds: 1800,
		CPU:            "500m",
		Memory:         "1Gi",
	}
	job := BuildJob("atr-1f3c9a7b2d41", req, cfg)

	got, err := yaml.Marshal(job)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}

	golden := filepath.Join("testdata", "job.golden.yaml")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden %s: %v", golden, err)
	}
	if string(got) != string(want) {
		t.Errorf("Job YAML mismatch.\n--- got ---\n%s\n--- want (%s) ---\n%s", got, golden, want)
	}
}

// TestBuildJobPromptNotInCommandOrArgs is the hard security assertion: the
// prompt must reach the pod only through the TASK_PROMPT env var and never
// appear in any container's Command or Args.
func TestBuildJobPromptNotInCommandOrArgs(t *testing.T) {
	cfg := testConfig()
	prompt := `"; rm -rf /; $(id); ` + "`whoami`" + "\nsecond line"
	req := &TaskRunRequest{Prompt: prompt}
	job := BuildJob("atr-abcdef123456", req, cfg)

	var cmdArgs []string
	for _, c := range job.Spec.Template.Spec.InitContainers {
		cmdArgs = append(cmdArgs, c.Command...)
		cmdArgs = append(cmdArgs, c.Args...)
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		cmdArgs = append(cmdArgs, c.Command...)
		cmdArgs = append(cmdArgs, c.Args...)
	}
	for _, s := range cmdArgs {
		if strings.Contains(s, prompt) {
			t.Errorf("prompt leaked into command/args: %q", s)
		}
	}

	found := false
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "TASK_PROMPT" {
				if e.Value != prompt {
					t.Errorf("TASK_PROMPT = %q, want %q", e.Value, prompt)
				}
				found = true
			}
		}
	}
	if !found {
		t.Error("TASK_PROMPT env var not found in worker container")
	}
}

// TestBuildJobSpecProperties asserts the structural invariants that the
// golden test alone (a single fixed input) does not lock down.
func TestBuildJobSpecProperties(t *testing.T) {
	cfg := testConfig()
	req := &TaskRunRequest{
		Prompt:         "hello",
		Model:          "deepseek-v4.1-flash",
		Provider:       "opencode-go",
		Toolsets:       "web,files",
		TimeoutSeconds: 900,
		CPU:            "500m",
		Memory:         "1Gi",
	}
	const taskID = "atr-000000000001"
	job := BuildJob(taskID, req, cfg)

	if job.Kind != "Job" || job.APIVersion != "batch/v1" {
		t.Errorf("TypeMeta = %s/%s, want Job/batch/v1", job.APIVersion, job.Kind)
	}
	if job.Name != taskID || job.Namespace != cfg.WorkerNamespace {
		t.Errorf("name/namespace = %s/%s, want %s/%s", job.Namespace, job.Name, cfg.WorkerNamespace, taskID)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit = %v, want pointer to 0", job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 900 {
		t.Errorf("ActiveDeadlineSeconds = %v, want 900", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("TTLSecondsAfterFinished = %v, want 3600", job.Spec.TTLSecondsAfterFinished)
	}

	if job.Labels[LabelTaskID] != taskID || job.Labels[LabelManagedBy] != appName || job.Labels[LabelName] != appName {
		t.Errorf("job labels = %v, want task-id/managed-by/name set", job.Labels)
	}
	if job.Annotations[AnnPromptPreview] != "hello" {
		t.Errorf("prompt preview = %q, want %q", job.Annotations[AnnPromptPreview], "hello")
	}
	if job.Annotations[AnnToolsets] != "web,files" {
		t.Errorf("toolsets annotation = %q, want %q", job.Annotations[AnnToolsets], "web,files")
	}

	pod := job.Spec.Template.Spec
	if job.Spec.Template.Labels[LabelName] != workerName || job.Spec.Template.Labels[LabelTaskID] != taskID {
		t.Errorf("pod labels = %v, want worker name + task id", job.Spec.Template.Labels)
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", pod.RestartPolicy)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("AutomountServiceAccountToken must be false")
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
		t.Error("EnableServiceLinks must be false")
	}
	initContainer := pod.InitContainers[0]
	if got := initContainer.Resources.Requests.Cpu().String(); got != initCPURequest {
		t.Errorf("init cpu request = %s, want %s", got, initCPURequest)
	}
	if got := initContainer.Resources.Requests.Memory().String(); got != initMemoryRequest {
		t.Errorf("init memory request = %s, want %s", got, initMemoryRequest)
	}
	if got := initContainer.Resources.Limits.Cpu().String(); got != initCPULimit {
		t.Errorf("init cpu limit = %s, want %s", got, initCPULimit)
	}
	if got := initContainer.Resources.Limits.Memory().String(); got != initMemoryLimit {
		t.Errorf("init memory limit = %s, want %s", got, initMemoryLimit)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != runAsUID {
		t.Errorf("runAsUser = %v, want %d", pod.SecurityContext, runAsUID)
	}
	if pod.SecurityContext == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("RunAsNonRoot must be true")
	}

	hermes := pod.Containers[0]
	if hermes.Image != cfg.WorkerImage {
		t.Errorf("worker image = %q, want %q", hermes.Image, cfg.WorkerImage)
	}
	if len(hermes.Command) != 2 || hermes.Command[0] != "/bin/sh" || hermes.Command[1] != "-c" {
		t.Errorf("Command = %v, want [/bin/sh -c]", hermes.Command)
	}
	if len(hermes.Args) != 1 {
		t.Fatalf("Args = %v, want exactly one element", hermes.Args)
	}
	if !strings.Contains(hermes.Args[0], `"$TASK_PROMPT"`) {
		t.Errorf("Args[0] = %q, want quoted $TASK_PROMPT reference", hermes.Args[0])
	}
}

// TestBuildJobAppliesDefaults verifies that empty optional fields fall back to
// the configuration defaults rather than empty strings.
func TestBuildJobAppliesDefaults(t *testing.T) {
	cfg := testConfig()
	job := BuildJob("atr-000000000002", &TaskRunRequest{Prompt: "hi"}, cfg)

	hermes := job.Spec.Template.Spec.Containers[0]
	if got := envValue(&hermes, "TASK_MODEL"); got != cfg.DefaultModel {
		t.Errorf("TASK_MODEL = %q, want default %q", got, cfg.DefaultModel)
	}
	if got := envValue(&hermes, "TASK_PROVIDER"); got != cfg.DefaultProvider {
		t.Errorf("TASK_PROVIDER = %q, want default %q", got, cfg.DefaultProvider)
	}
	if got := envValue(&hermes, "TASK_TOOLSETS"); got != cfg.DefaultToolsets {
		t.Errorf("TASK_TOOLSETS = %q, want default %q", got, cfg.DefaultToolsets)
	}
	if got := envValue(&hermes, "TASK_PROMPT"); got != "hi" {
		t.Errorf("TASK_PROMPT = %q, want %q", got, "hi")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64(cfg.DefaultTimeoutSeconds) {
		t.Errorf("ActiveDeadlineSeconds = %v, want default %d", job.Spec.ActiveDeadlineSeconds, cfg.DefaultTimeoutSeconds)
	}
	if hermes.Resources.Requests.Cpu().String() != cfg.DefaultCPU.String() {
		t.Errorf("cpu request = %s, want default %s", hermes.Resources.Requests.Cpu().String(), cfg.DefaultCPU.String())
	}
	if hermes.Resources.Requests.Memory().String() != cfg.DefaultMemory.String() {
		t.Errorf("memory request = %s, want default %s", hermes.Resources.Requests.Memory().String(), cfg.DefaultMemory.String())
	}
}

// TestBuildJobPromptPreviewRuneTruncated verifies the annotation is truncated
// on rune boundaries, not raw bytes.
func TestBuildJobPromptPreviewRuneTruncated(t *testing.T) {
	cfg := testConfig()
	long := strings.Repeat("é", 200) // 2 bytes per rune
	job := BuildJob("atr-000000000003", &TaskRunRequest{Prompt: long}, cfg)

	preview := job.Annotations[AnnPromptPreview]
	if got := len([]rune(preview)); got != PromptPreviewRuneLimit {
		t.Errorf("preview rune length = %d, want %d", got, PromptPreviewRuneLimit)
	}
	// If truncated by bytes, 120 bytes would hold only 60 runes of "é"; the
	// byte length must therefore exceed the rune limit.
	if len(preview) == PromptPreviewRuneLimit {
		t.Errorf("preview byte length == %d, indicates byte (not rune) truncation", PromptPreviewRuneLimit)
	}
}

func TestRuneTruncate(t *testing.T) {
	if got := runeTruncate("short", PromptPreviewRuneLimit); got != "short" {
		t.Errorf("runeTruncate(short) = %q, want %q", got, "short")
	}
	if got := len([]rune(runeTruncate(strings.Repeat("a", 130), PromptPreviewRuneLimit))); got != PromptPreviewRuneLimit {
		t.Errorf("runeTruncate ascii rune count = %d, want %d", got, PromptPreviewRuneLimit)
	}
	emoji := strings.Repeat("😀", 150) // 4 bytes per rune
	got := runeTruncate(emoji, PromptPreviewRuneLimit)
	if n := len([]rune(got)); n != PromptPreviewRuneLimit {
		t.Errorf("runeTruncate emoji rune count = %d, want %d", n, PromptPreviewRuneLimit)
	}
	if len(got) == PromptPreviewRuneLimit {
		t.Error("runeTruncate emoji appears byte-truncated")
	}
}

func envValue(c *corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
