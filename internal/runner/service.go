package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Stash088/agent-task-runner/internal/config"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// maxIDRetries bounds the retry loop that handles a random task-id
	// collision (Job create returns AlreadyExists).
	maxIDRetries = 5
	// defaultListLimit is the task_list limit when the caller does not set one.
	defaultListLimit = 20
)

// ErrBusy is returned by Run when the number of concurrently active jobs has
// reached MAX_CONCURRENT_JOBS. Clients are expected to retry or queue.
var ErrBusy = errors.New("max concurrent jobs exceeded")

// PodLogGetter abstracts pod log retrieval so tests can inject canned logs —
// the fake clientset cannot stream pod logs. The production implementation
// wraps the clientset's PodExpansion GetLogs.
type PodLogGetter interface {
	GetLogs(ctx context.Context, namespace, pod, container string, limitBytes int64) ([]byte, error)
}

// k8sLogGetter reads pod logs through the real clientset.
type k8sLogGetter struct {
	client kubernetes.Interface
}

func (g *k8sLogGetter) GetLogs(ctx context.Context, namespace, pod, container string, limitBytes int64) ([]byte, error) {
	opts := &corev1.PodLogOptions{Container: container, Timestamps: false}
	if limitBytes > 0 {
		opts.LimitBytes = &limitBytes
	}
	return g.client.CoreV1().Pods(namespace).GetLogs(pod, opts).DoRaw(ctx)
}

// NewPodLogGetter returns a production log getter backed by the clientset.
func NewPodLogGetter(client kubernetes.Interface) PodLogGetter {
	return &k8sLogGetter{client: client}
}

// Service orchestrates task lifecycle against the Kubernetes API. All state
// lives in the cluster (Job + labels), so the service is stateless.
type Service struct {
	client kubernetes.Interface
	logs   PodLogGetter
	cfg    *config.Config
	logger *slog.Logger
	// idFunc generates task ids; overridable in tests to force collisions.
	idFunc func() (string, error)
	// mu serialises the capacity check and Job creation so that two
	// concurrent Run calls cannot both pass the MAX_CONCURRENT_JOBS check.
	mu sync.Mutex
}

// NewService builds a Service. A nil logger falls back to slog.Default.
func NewService(client kubernetes.Interface, logs PodLogGetter, cfg *config.Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if logs == nil {
		logs = NewPodLogGetter(client)
	}
	return &Service{
		client: client,
		logs:   logs,
		cfg:    cfg,
		logger: logger,
		idFunc: newTaskID,
	}
}

// Run validates the request, enforces the concurrency cap, and creates the
// worker Job. On a random id collision (AlreadyExists) it retries with a fresh
// id.
func (s *Service) Run(ctx context.Context, req *TaskRunRequest) (*TaskRunResponse, error) {
	if err := Validate(req, s.cfg); err != nil {
		return nil, err
	}

	// The capacity check and the Job creation must be atomic: otherwise two
	// concurrent callers could both observe a free slot and exceed the cap.
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkCapacity(ctx); err != nil {
		return nil, err
	}

	var (
		created *batchv1.Job
		taskID  string
		err     error
	)
	for attempt := 0; attempt < maxIDRetries; attempt++ {
		if taskID, err = s.idFunc(); err != nil {
			return nil, fmt.Errorf("generate task id: %w", err)
		}
		created, err = s.client.BatchV1().Jobs(s.cfg.WorkerNamespace).Create(ctx, BuildJob(taskID, req, s.cfg), metav1.CreateOptions{})
		if err == nil {
			break
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create job: %w", err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("create job after %d id collisions: %w", maxIDRetries, err)
	}

	podName := s.findPodName(ctx, taskID)
	s.logger.Info("task created", "task_id", taskID, "pod", podName)

	createdAt := time.Now().UTC()
	if !created.CreationTimestamp.IsZero() {
		createdAt = created.CreationTimestamp.Time
	}

	return &TaskRunResponse{
		TaskID:    taskID,
		State:     StatePending,
		CreatedAt: createdAt,
		Pod:       podName,
	}, nil
}

// Ready reports whether the Kubernetes API is reachable by performing a
// minimal list of managed Jobs. It is used by the /readyz probe.
func (s *Service) Ready(ctx context.Context) error {
	if _, err := s.client.BatchV1().Jobs(s.cfg.WorkerNamespace).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	return nil
}

// Status reports the task state derived from the Job conditions and the worker
// pod phase/terminated status.
func (s *Service) Status(ctx context.Context, req TaskStatusRequest) (*TaskStatusResponse, error) {
	ns := s.cfg.WorkerNamespace
	job, err := s.client.BatchV1().Jobs(ns).Get(ctx, req.TaskID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &TaskStatusResponse{TaskID: req.TaskID, State: StateUnknown}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	pods, err := s.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelTaskID + "=" + req.TaskID})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	return deriveStatus(req.TaskID, job, pods.Items), nil
}

// Result streams the worker pod log (up to the configured limit) and reports
// the task state and exit code.
func (s *Service) Result(ctx context.Context, req TaskResultRequest) (*TaskResultResponse, error) {
	ns := s.cfg.WorkerNamespace
	job, err := s.client.BatchV1().Jobs(ns).Get(ctx, req.TaskID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &TaskResultResponse{TaskID: req.TaskID, State: StateUnknown, Message: "result expired"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}

	pods, err := s.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelTaskID + "=" + req.TaskID})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	pod := newestPod(pods.Items)
	if pod == nil {
		if jobTerminal(job) {
			return &TaskResultResponse{TaskID: req.TaskID, State: StateUnknown, Message: "result expired"}, nil
		}
		st := deriveStatus(req.TaskID, job, nil)
		return &TaskResultResponse{TaskID: req.TaskID, State: st.State}, nil
	}

	limit := req.MaxBytes
	if limit <= 0 || limit > s.cfg.MaxLogBytes {
		limit = s.cfg.MaxLogBytes
	}

	data, err := s.logs.GetLogs(ctx, ns, pod.Name, containerName, limit)
	if err != nil {
		// A pod may exist while its container is still waiting (image pull,
		// volume setup, scheduling). The log API answers BadRequest in that
		// window; that is not a task failure and must not turn into an error.
		if reason, ok := logUnavailableReason(pod, err); ok {
			st := deriveStatus(req.TaskID, job, pods.Items)
			return &TaskResultResponse{
				TaskID:   req.TaskID,
				State:    st.State,
				ExitCode: st.ExitCode,
				Message:  reason,
			}, nil
		}
		return nil, fmt.Errorf("get pod logs: %w", err)
	}

	st := deriveStatus(req.TaskID, job, pods.Items)
	return &TaskResultResponse{
		TaskID:    req.TaskID,
		State:     st.State,
		ExitCode:  st.ExitCode,
		Log:       string(data),
		Truncated: int64(len(data)) >= limit,
		LogBytes:  int64(len(data)),
	}, nil
}

// Cancel deletes the worker Job with propagationPolicy=Background.
func (s *Service) Cancel(ctx context.Context, req TaskCancelRequest) (*TaskCancelResponse, error) {
	policy := metav1.DeletePropagationBackground
	err := s.client.BatchV1().Jobs(s.cfg.WorkerNamespace).Delete(ctx, req.TaskID, metav1.DeleteOptions{PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return &TaskCancelResponse{TaskID: req.TaskID, Deleted: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delete job: %w", err)
	}
	return &TaskCancelResponse{TaskID: req.TaskID, Deleted: true}, nil
}

// List returns tasks managed by this service, newest first, optionally
// filtered by state and limited to req.Limit entries.
func (s *Service) List(ctx context.Context, req TaskListRequest) (*TaskListResponse, error) {
	jobs, err := s.client.BatchV1().Jobs(s.cfg.WorkerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + appName,
	})
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}

	items := make([]TaskListItem, 0, len(jobs.Items))
	for i := range jobs.Items {
		job := &jobs.Items[i]
		st := stateForList(job)
		if req.State != "" && string(st) != req.State {
			continue
		}
		items = append(items, TaskListItem{
			TaskID:        job.Name,
			State:         st,
			CreatedAt:     job.CreationTimestamp.Time,
			PromptPreview: job.Annotations[AnnPromptPreview],
		})
	}

	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})

	limit := req.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	count := len(items)
	if limit < len(items) {
		items = items[:limit]
	}
	return &TaskListResponse{Tasks: items, Count: count}, nil
}

// checkCapacity enforces MAX_CONCURRENT_JOBS by counting managed jobs that have
// not yet reached a terminal condition.
func (s *Service) checkCapacity(ctx context.Context) error {
	jobs, err := s.client.BatchV1().Jobs(s.cfg.WorkerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + appName,
	})
	if err != nil {
		return fmt.Errorf("list jobs for capacity check: %w", err)
	}
	active := 0
	for i := range jobs.Items {
		if jobActive(&jobs.Items[i]) {
			active++
		}
	}
	if active >= s.cfg.MaxConcurrentJobs {
		return ErrBusy
	}
	return nil
}

// findPodName best-effort resolves the worker pod name for a freshly created
// Job; the Job controller creates the pod asynchronously, so this is usually
// empty at creation time.
func (s *Service) findPodName(ctx context.Context, taskID string) string {
	pods, err := s.client.CoreV1().Pods(s.cfg.WorkerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelTaskID + "=" + taskID,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	return pods.Items[0].Name
}

func newTaskID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "atr-" + hex.EncodeToString(b[:]), nil
}

// jobActive reports whether a job still counts against the concurrency cap.
func jobActive(job *batchv1.Job) bool {
	return !jobTerminal(job)
}

func jobTerminal(job *batchv1.Job) bool {
	return hasCondition(job, batchv1.JobComplete) || hasCondition(job, batchv1.JobFailed)
}

func hasCondition(job *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// deriveStatus combines Job conditions with the worker pod to produce a
// TaskStatusResponse.
func deriveStatus(taskID string, job *batchv1.Job, pods []corev1.Pod) *TaskStatusResponse {
	resp := &TaskStatusResponse{TaskID: taskID}
	pod := newestPod(pods)

	switch {
	case hasCondition(job, batchv1.JobComplete):
		resp.State = StateSucceeded
		resp.ExitCode = ptr(int32(0))
		resp.CompletionTime = completionTime(job, pod)
	case hasCondition(job, batchv1.JobFailed):
		resp.State = StateFailed
		resp.ExitCode = exitCodeFromPod(pod)
		resp.CompletionTime = completionTime(job, pod)
		resp.Message = failureMessage(pod)
	case pod == nil:
		resp.State = StatePending
	default:
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			resp.State = StateSucceeded
			resp.ExitCode = ptr(int32(0))
		case corev1.PodFailed:
			resp.State = StateFailed
			resp.ExitCode = exitCodeFromPod(pod)
			resp.Message = failureMessage(pod)
		case corev1.PodRunning:
			resp.State = StateRunning
		default:
			resp.State = StatePending
		}
		resp.CompletionTime = completionTime(job, pod)
	}

	if pod != nil && pod.Status.StartTime != nil {
		resp.StartTime = metav1TimePtr(pod.Status.StartTime)
	} else if job.Status.StartTime != nil {
		resp.StartTime = metav1TimePtr(job.Status.StartTime)
	}
	return resp
}

// stateForList maps a Job to a task_list state. Pending jobs are reported as
// "running" because task_list only supports running|succeeded|failed.
func stateForList(job *batchv1.Job) TaskState {
	switch {
	case hasCondition(job, batchv1.JobComplete):
		return StateSucceeded
	case hasCondition(job, batchv1.JobFailed):
		return StateFailed
	default:
		return StateRunning
	}
}

func newestPod(pods []corev1.Pod) *corev1.Pod {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	return best
}

func findHermesContainerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	if pod == nil {
		return nil
	}
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == containerName {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// logUnavailableReason reports whether a log-read error is expected because the
// worker container has not started yet, and returns a human-readable reason.
func logUnavailableReason(pod *corev1.Pod, err error) (string, bool) {
	cs := findHermesContainerStatus(pod)
	if cs != nil && cs.State.Waiting != nil {
		if cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason, true
		}
		if cs.State.Waiting.Message != "" {
			return cs.State.Waiting.Message, true
		}
		return err.Error(), true
	}
	// Even without a visible container status the API may report a BadRequest
	// while the pod/container is being created.
	if apierrors.IsBadRequest(err) {
		return err.Error(), true
	}
	return "", false
}

func exitCodeFromPod(pod *corev1.Pod) *int32 {
	cs := findHermesContainerStatus(pod)
	if cs == nil || cs.State.Terminated == nil {
		return nil
	}
	return &cs.State.Terminated.ExitCode
}

func failureMessage(pod *corev1.Pod) string {
	cs := findHermesContainerStatus(pod)
	if cs == nil || cs.State.Terminated == nil {
		return ""
	}
	if cs.State.Terminated.Message != "" {
		return cs.State.Terminated.Message
	}
	if cs.State.Terminated.Reason != "" {
		return cs.State.Terminated.Reason
	}
	return fmt.Sprintf("exit code %d", cs.State.Terminated.ExitCode)
}

func completionTime(job *batchv1.Job, pod *corev1.Pod) *time.Time {
	if t := metav1TimePtr(job.Status.CompletionTime); t != nil {
		return t
	}
	if cs := findHermesContainerStatus(pod); cs != nil && cs.State.Terminated != nil {
		return metav1TimePtr(&cs.State.Terminated.FinishedAt)
	}
	return nil
}

func metav1TimePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	return &t.Time
}
