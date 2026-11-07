package runner

import (
	"github.com/Stash088/agent-task-runner/internal/config"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label and annotation keys shared between the Job spec and the service.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelTaskID    = "atr.io/task-id"

	AnnPromptPreview = "atr.io/prompt-preview"
	AnnToolsets      = "atr.io/toolsets"

	appName    = "agent-task-runner"
	workerName = "agent-task-worker"
)

// Worker pod constants (fixed by the spike — do not change without a re-spike).
const (
	containerName       = "hermes"
	initContainerName   = "chown-home"
	homeVolumeName      = "home"
	workerHomeConfigMap = "agent-task-worker-home" // plugin bundle (z.ai web search/extract)
	workerPluginsPath   = homeMountPath + "/plugins/zai-web-mcp"
	pluginsVolumeName   = "worker-plugins"
	homeMountPath       = "/data/home"
	homeSizeLimit       = "2Gi"
	busyboxImage        = "busybox:1.36"
	runAsUID            = int64(10000)
	runAsGID            = int64(10000)

	initCPURequest    = "50m"
	initMemoryRequest = "32Mi"
	initCPULimit      = "200m"
	initMemoryLimit   = "128Mi"
)

// PromptPreviewRuneLimit is how many runes of the prompt are stored in the
// Job annotation for task_list (truncated by rune, not by byte).
const PromptPreviewRuneLimit = 120

// BuildJob assembles the worker Job exactly as specified in section 2 of the
// plan. Empty optional fields fall back to the configuration defaults, and the
// prompt reaches the pod only through the TASK_PROMPT environment variable —
// never through Command or Args.
func BuildJob(taskID string, req *TaskRunRequest, cfg *config.Config) *batchv1.Job {
	model := req.Model
	if model == "" {
		model = cfg.DefaultModel
	}
	provider := req.Provider
	if provider == "" {
		provider = cfg.DefaultProvider
	}
	toolsets := req.Toolsets
	if toolsets == "" {
		toolsets = cfg.DefaultToolsets
	}
	timeout := req.TimeoutSeconds
	if timeout == 0 {
		timeout = cfg.DefaultTimeoutSeconds
	}
	cpu := req.CPU
	if cpu == "" {
		cpu = cfg.DefaultCPU.String()
	}
	memory := req.Memory
	if memory == "" {
		memory = cfg.DefaultMemory.String()
	}

	// Values are canonical by now (validated, or the config default), so
	// MustParse cannot fail.
	cpuQty := resource.MustParse(cpu)
	memQty := resource.MustParse(memory)

	runAsNonRoot := true
	allowPrivilegeEscalation := false
	enableServiceLinks := false
	backoffLimit := int32(0)
	automountSA := false
	ttl := cfg.TTLSeconds
	activeDeadline := int64(timeout)

	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskID,
			Namespace: cfg.WorkerNamespace,
			Labels: map[string]string{
				LabelName:      appName,
				LabelManagedBy: appName,
				LabelTaskID:    taskID,
			},
			Annotations: map[string]string{
				AnnPromptPreview: runeTruncate(req.Prompt, PromptPreviewRuneLimit),
				AnnToolsets:      toolsets,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			ActiveDeadlineSeconds:   &activeDeadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelName:   workerName,
						LabelTaskID: taskID,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &automountSA,
					EnableServiceLinks:           &enableServiceLinks,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:    ptr(runAsUID),
						RunAsGroup:   ptr(runAsGID),
						RunAsNonRoot: &runAsNonRoot,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					InitContainers: []corev1.Container{
						{
							Name:  initContainerName,
							Image: busyboxImage,
							SecurityContext: &corev1.SecurityContext{
								RunAsUser:                ptr(int64(0)),
								RunAsNonRoot:             ptr(false),
								AllowPrivilegeEscalation: &allowPrivilegeEscalation,
							},
							Command: []string{"sh", "-c", "mkdir -p /data/home && chown 10000:10000 /data/home && chmod 0755 /data/home"},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(initCPURequest),
									corev1.ResourceMemory: resource.MustParse(initMemoryRequest),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(initCPULimit),
									corev1.ResourceMemory: resource.MustParse(initMemoryLimit),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: homeVolumeName, MountPath: homeMountPath},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            containerName,
							Image:           cfg.WorkerImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env: []corev1.EnvVar{
								{Name: "HERMES_HOME", Value: homeMountPath},
								{Name: "HOME", Value: homeMountPath},
								// The image defaults this to <hermes_home>, which is outside the
								// writable workspace; the agent must write inside HERMES_HOME.
								{Name: "HERMES_WRITE_SAFE_ROOT", Value: homeMountPath},
								{
									Name: "GLM_API_KEY", // z.ai web search/extract backend
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: cfg.LLMSecretName},
											Key:                  "GLM_API_KEY",
										},
									},
								},
								{Name: "TASK_PROMPT", Value: req.Prompt},
								{Name: "TASK_MODEL", Value: model},
								{Name: "TASK_PROVIDER", Value: provider},
								{Name: "TASK_TOOLSETS", Value: toolsets},
								{
									Name: "OPENCODE_GO_API_KEY",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: cfg.LLMSecretName},
											Key:                  "OPENCODE_GO_API_KEY",
										},
									},
								},
							},
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{`exec hermes -z "$TASK_PROMPT" -m "$TASK_MODEL" --provider "$TASK_PROVIDER" -t "$TASK_TOOLSETS" --ignore-user-config --yolo`},
							VolumeMounts: []corev1.VolumeMount{
								{Name: homeVolumeName, MountPath: homeMountPath},
								{Name: pluginsVolumeName, MountPath: workerPluginsPath, ReadOnly: true},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQty,
									corev1.ResourceMemory: memQty,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQty,
									corev1.ResourceMemory: memQty,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: pluginsVolumeName,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: workerHomeConfigMap},
									Optional:             ptr(true),
								},
							},
						},
						{
							Name: homeVolumeName,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									SizeLimit: ptr(resource.MustParse(homeSizeLimit)),
								},
							},
						},
					},
				},
			},
		},
	}
}

// runeTruncate returns s truncated to at most max runes, splitting on rune
// boundaries rather than raw bytes.
func runeTruncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func ptr[T any](v T) *T { return &v }
