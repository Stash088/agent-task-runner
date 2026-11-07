package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeLogGetter implements PodLogGetter with an in-memory map and applies the
// requested limit the way the real pods/log API does.
type fakeLogGetter struct {
	logs      map[string]string
	err       error
	lastLimit int64
}

func (f *fakeLogGetter) GetLogs(_ context.Context, _, pod, _ string, limitBytes int64) ([]byte, error) {
	f.lastLimit = limitBytes
	if f.err != nil {
		return nil, f.err
	}
	s := f.logs[pod]
	if limitBytes > 0 && int64(len(s)) > limitBytes {
		return []byte(s[:limitBytes]), nil
	}
	return []byte(s), nil
}

func newTestService(t *testing.T, objects ...runtime.Object) (*Service, *fakeLogGetter) {
	t.Helper()
	client := fake.NewSimpleClientset(objects...)
	logs := &fakeLogGetter{logs: map[string]string{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewService(client, logs, testConfig(), logger), logs
}

func testJobFor(taskID string, created time.Time) *batchv1.Job {
	job := BuildJob(taskID, &TaskRunRequest{Prompt: "test prompt for " + taskID}, testConfig())
	job.CreationTimestamp = metav1.NewTime(created)
	return job
}

func completeJob(job *batchv1.Job, t time.Time) *batchv1.Job {
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	job.Status.CompletionTime = &metav1.Time{Time: t}
	return job
}

func failJob(job *batchv1.Job, t time.Time) *batchv1.Job {
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	job.Status.CompletionTime = &metav1.Time{Time: t}
	return job
}

func testPod(taskID, name string, phase corev1.PodPhase, exitCode int32) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "agent-tasks",
			Labels:    map[string]string{LabelTaskID: taskID},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  containerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Reason: "Error"}},
		}}
	}
	return p
}

func TestRunCreatesJob(t *testing.T) {
	svc, _ := newTestService(t)
	req := &TaskRunRequest{
		Prompt:         "hello world",
		Model:          "deepseek-v4.1-flash",
		Provider:       "opencode-go",
		TimeoutSeconds: 300,
		CPU:            "500m",
		Memory:         "1Gi",
	}

	resp, err := svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.HasPrefix(resp.TaskID, "atr-") || len(resp.TaskID) != len("atr-")+12 {
		t.Errorf("TaskID = %q, want atr-<12 hex>", resp.TaskID)
	}
	if resp.State != StatePending {
		t.Errorf("State = %q, want pending", resp.State)
	}
	if resp.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	job, err := svc.client.BatchV1().Jobs("agent-tasks").Get(context.Background(), resp.TaskID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created job: %v", err)
	}
	if job.Labels[LabelTaskID] != resp.TaskID {
		t.Errorf("job task-id label = %q, want %q", job.Labels[LabelTaskID], resp.TaskID)
	}
	if job.Annotations[AnnPromptPreview] != "hello world" {
		t.Errorf("prompt preview = %q, want %q", job.Annotations[AnnPromptPreview], "hello world")
	}
}

func TestRunRejectsInvalidRequest(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.Run(context.Background(), &TaskRunRequest{Prompt: ""})
	if err == nil {
		t.Fatal("Run() error = nil, want *ValidationError")
	}
	if _, ok := err.(*ValidationError); !ok {
		t.Fatalf("Run() error type = %T, want *ValidationError", err)
	}
}

func TestRunBusy(t *testing.T) {
	cfg := testConfig()
	now := time.Now()
	var objects []runtime.Object
	for i := 0; i < cfg.MaxConcurrentJobs; i++ {
		objects = append(objects, testJobFor(fmt.Sprintf("atr-%012x", i), now))
	}
	svc, _ := newTestService(t, objects...)

	if _, err := svc.Run(context.Background(), &TaskRunRequest{Prompt: "x"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("Run() error = %v, want ErrBusy", err)
	}
}

func TestRunConcurrentCapacity(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConcurrentJobs = 3
	client := fake.NewSimpleClientset()
	logs := &fakeLogGetter{logs: map[string]string{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewService(client, logs, cfg, logger)

	const goroutines = 10
	var (
		wg        sync.WaitGroup
		successes atomic.Int32
		busy      atomic.Int32
		start     = make(chan struct{})
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.Run(context.Background(), &TaskRunRequest{Prompt: fmt.Sprintf("task %d", i)})
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrBusy):
				busy.Add(1)
			default:
				t.Errorf("Run() error = %v, want nil or ErrBusy", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := successes.Load(); got != int32(cfg.MaxConcurrentJobs) {
		t.Errorf("successful runs = %d, want %d", got, cfg.MaxConcurrentJobs)
	}
	if got := busy.Load(); got != int32(goroutines-cfg.MaxConcurrentJobs) {
		t.Errorf("busy runs = %d, want %d", got, goroutines-cfg.MaxConcurrentJobs)
	}

	jobs, err := client.BatchV1().Jobs(cfg.WorkerNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != cfg.MaxConcurrentJobs {
		t.Errorf("created jobs = %d, want %d", len(jobs.Items), cfg.MaxConcurrentJobs)
	}
}

func TestRunRetriesOnIDCollision(t *testing.T) {
	const colliding = "atr-0000000000aa"
	svc, _ := newTestService(t, testJobFor(colliding, time.Now()))

	calls := 0
	svc.idFunc = func() (string, error) {
		calls++
		if calls == 1 {
			return colliding, nil
		}
		return "atr-0000000000bb", nil
	}

	resp, err := svc.Run(context.Background(), &TaskRunRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if resp.TaskID != "atr-0000000000bb" {
		t.Errorf("TaskID = %q, want retried id", resp.TaskID)
	}
	if calls != 2 {
		t.Errorf("id generator calls = %d, want 2", calls)
	}
}

func TestStatusStates(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		setup     func() (*Service, string)
		wantState TaskState
		wantExit  *int32
	}{
		{
			name: "succeeded",
			setup: func() (*Service, string) {
				id := "atr-0000000000aa"
				j := completeJob(testJobFor(id, now), now)
				p := testPod(id, id+"-abcde", corev1.PodSucceeded, 0)
				svc, _ := newTestService(t, j, p)
				return svc, id
			},
			wantState: StateSucceeded,
			wantExit:  ptr(int32(0)),
		},
		{
			name: "failed",
			setup: func() (*Service, string) {
				id := "atr-0000000000bb"
				j := failJob(testJobFor(id, now), now)
				p := testPod(id, id+"-abcde", corev1.PodFailed, 2)
				svc, _ := newTestService(t, j, p)
				return svc, id
			},
			wantState: StateFailed,
			wantExit:  ptr(int32(2)),
		},
		{
			name: "running",
			setup: func() (*Service, string) {
				id := "atr-0000000000cc"
				j := testJobFor(id, now)
				p := testPod(id, id+"-abcde", corev1.PodRunning, 0)
				svc, _ := newTestService(t, j, p)
				return svc, id
			},
			wantState: StateRunning,
		},
		{
			name: "pending no pod yet",
			setup: func() (*Service, string) {
				id := "atr-0000000000dd"
				j := testJobFor(id, now)
				svc, _ := newTestService(t, j)
				return svc, id
			},
			wantState: StatePending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, id := tt.setup()
			resp, err := svc.Status(context.Background(), TaskStatusRequest{TaskID: id})
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if resp.State != tt.wantState {
				t.Errorf("State = %q, want %q", resp.State, tt.wantState)
			}
			if tt.wantExit != nil {
				if resp.ExitCode == nil || *resp.ExitCode != *tt.wantExit {
					t.Errorf("ExitCode = %v, want %d", resp.ExitCode, *tt.wantExit)
				}
			}
		})
	}
}

func TestStatusUnknown(t *testing.T) {
	svc, _ := newTestService(t)
	resp, err := svc.Status(context.Background(), TaskStatusRequest{TaskID: "atr-doesnotexist"})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if resp.State != StateUnknown {
		t.Errorf("State = %q, want unknown", resp.State)
	}
}

func TestResultSuccess(t *testing.T) {
	now := time.Now()
	id := "atr-0000000000aa"
	pod := id + "-abcde"
	j := completeJob(testJobFor(id, now), now)
	p := testPod(id, pod, corev1.PodSucceeded, 0)
	svc, logs := newTestService(t, j, p)
	logs.logs[pod] = "E2E-OK"

	resp, err := svc.Result(context.Background(), TaskResultRequest{TaskID: id})
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if resp.State != StateSucceeded {
		t.Errorf("State = %q, want succeeded", resp.State)
	}
	if resp.Log != "E2E-OK" {
		t.Errorf("Log = %q, want %q", resp.Log, "E2E-OK")
	}
	if resp.LogBytes != 6 {
		t.Errorf("LogBytes = %d, want 6", resp.LogBytes)
	}
	if resp.Truncated {
		t.Error("Truncated = true, want false")
	}
}

func TestResultTruncated(t *testing.T) {
	now := time.Now()
	id := "atr-0000000000aa"
	pod := id + "-abcde"
	j := completeJob(testJobFor(id, now), now)
	p := testPod(id, pod, corev1.PodSucceeded, 0)
	svc, logs := newTestService(t, j, p)
	logs.logs[pod] = "this is a long log line"

	resp, err := svc.Result(context.Background(), TaskResultRequest{TaskID: id, MaxBytes: 5})
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if !resp.Truncated {
		t.Error("Truncated = false, want true")
	}
	if resp.Log != "this " {
		t.Errorf("Log = %q, want %q (limit 5)", resp.Log, "this ")
	}
	if resp.LogBytes != 5 {
		t.Errorf("LogBytes = %d, want 5", resp.LogBytes)
	}
}

func TestResultMaxBytesClampedToConfig(t *testing.T) {
	now := time.Now()
	id := "atr-0000000000aa"
	pod := id + "-abcde"
	j := completeJob(testJobFor(id, now), now)
	p := testPod(id, pod, corev1.PodSucceeded, 0)
	svc, logs := newTestService(t, j, p)
	logs.logs[pod] = "x"

	_, err := svc.Result(context.Background(), TaskResultRequest{TaskID: id, MaxBytes: 1 << 40})
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if logs.lastLimit != testConfig().MaxLogBytes {
		t.Errorf("log limit = %d, want configured MaxLogBytes %d", logs.lastLimit, testConfig().MaxLogBytes)
	}
}

func TestResultWaitingContainer(t *testing.T) {
	now := time.Now()
	id := "atr-0000000000aa"
	podName := id + "-abcde"
	j := testJobFor(id, now) // Job created, not finished yet
	p := testPod(id, podName, corev1.PodPending, 0)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: containerName,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
		},
	}}
	svc, logs := newTestService(t, j, p)
	logs.err = apierrors.NewBadRequest("container is waiting to start")

	resp, err := svc.Result(context.Background(), TaskResultRequest{TaskID: id})
	if err != nil {
		t.Fatalf("Result() error = %v, want nil for waiting container", err)
	}
	if resp.State != StatePending {
		t.Errorf("State = %q, want pending", resp.State)
	}
	if resp.Log != "" {
		t.Errorf("Log = %q, want empty", resp.Log)
	}
	if !strings.Contains(resp.Message, "ContainerCreating") {
		t.Errorf("Message = %q, want container waiting reason", resp.Message)
	}
}

func TestResultOtherLogErrorFails(t *testing.T) {
	now := time.Now()
	id := "atr-0000000000aa"
	podName := id + "-abcde"
	j := testJobFor(id, now)
	p := testPod(id, podName, corev1.PodRunning, 0)
	svc, logs := newTestService(t, j, p)
	logs.err = errors.New("connection reset")

	if _, err := svc.Result(context.Background(), TaskResultRequest{TaskID: id}); err == nil {
		t.Fatal("Result() error = nil, want non-nil for unexpected log failure")
	}
}

func TestResultUnknownWhenJobGone(t *testing.T) {
	svc, _ := newTestService(t)
	resp, err := svc.Result(context.Background(), TaskResultRequest{TaskID: "atr-doesnotexist"})
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if resp.State != StateUnknown {
		t.Errorf("State = %q, want unknown", resp.State)
	}
	if resp.Log != "" {
		t.Errorf("Log = %q, want empty", resp.Log)
	}
	if resp.Message != "result expired" {
		t.Errorf("Message = %q, want %q", resp.Message, "result expired")
	}
}

func TestResultExpiredWhenPodGone(t *testing.T) {
	now := time.Now()
	id := "atr-0000000000aa"
	j := completeJob(testJobFor(id, now), now)
	svc, _ := newTestService(t, j) // terminal job, no pod

	resp, err := svc.Result(context.Background(), TaskResultRequest{TaskID: id})
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if resp.State != StateUnknown {
		t.Errorf("State = %q, want unknown", resp.State)
	}
	if resp.Message != "result expired" {
		t.Errorf("Message = %q, want %q", resp.Message, "result expired")
	}
}

func TestCancelDeletesJob(t *testing.T) {
	id := "atr-0000000000aa"
	svc, _ := newTestService(t, testJobFor(id, time.Now()))

	resp, err := svc.Cancel(context.Background(), TaskCancelRequest{TaskID: id})
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if !resp.Deleted {
		t.Error("Deleted = false, want true")
	}
	_, err = svc.client.BatchV1().Jobs("agent-tasks").Get(context.Background(), id, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Get after cancel error = %v, want NotFound", err)
	}
}

func TestCancelNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	resp, err := svc.Cancel(context.Background(), TaskCancelRequest{TaskID: "atr-doesnotexist"})
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if resp.Deleted {
		t.Error("Deleted = true, want false for non-existent job")
	}
}

func TestListSortedAndLimited(t *testing.T) {
	now := time.Now()
	var objects []runtime.Object
	for i := 0; i < 5; i++ {
		objects = append(objects, testJobFor(fmt.Sprintf("atr-%012x", i), now.Add(time.Duration(i)*time.Minute)))
	}
	svc, _ := newTestService(t, objects...)

	resp, err := svc.List(context.Background(), TaskListRequest{Limit: 3})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if resp.Count != 5 {
		t.Errorf("Count = %d, want 5 (before limit)", resp.Count)
	}
	if len(resp.Tasks) != 3 {
		t.Errorf("len(Tasks) = %d, want 3", len(resp.Tasks))
	}
	// newest first: i=4 is the most recent
	if resp.Tasks[0].TaskID != "atr-000000000004" {
		t.Errorf("Tasks[0].TaskID = %q, want newest", resp.Tasks[0].TaskID)
	}
	if resp.Tasks[0].PromptPreview == "" {
		t.Error("Tasks[0].PromptPreview is empty, want annotation value")
	}
}

func TestListStateFilter(t *testing.T) {
	now := time.Now()
	var objects []runtime.Object
	// two succeeded, one failed, one running
	objects = append(objects,
		completeJob(testJobFor("atr-0000000000a1", now.Add(time.Minute)), now),
		completeJob(testJobFor("atr-0000000000a2", now.Add(2*time.Minute)), now),
		failJob(testJobFor("atr-0000000000b1", now.Add(3*time.Minute)), now),
		testJobFor("atr-0000000000c1", now.Add(4*time.Minute)),
	)
	svc, _ := newTestService(t, objects...)

	resp, err := svc.List(context.Background(), TaskListRequest{State: "succeeded"})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if resp.Count != 2 {
		t.Errorf("Count = %d, want 2", resp.Count)
	}
	for _, task := range resp.Tasks {
		if task.State != StateSucceeded {
			t.Errorf("task %s state = %q, want succeeded", task.TaskID, task.State)
		}
	}
}

func TestListExcludesForeignJobs(t *testing.T) {
	now := time.Now()
	foreign := testJobFor("atr-ffffffffffff", now)
	foreign.Labels[LabelManagedBy] = "someone-else"
	svc, _ := newTestService(t, foreign, testJobFor("atr-0000000000aa", now))

	resp, err := svc.List(context.Background(), TaskListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if resp.Count != 1 {
		t.Errorf("Count = %d, want 1 (foreign job excluded)", resp.Count)
	}
	if resp.Tasks[0].TaskID != "atr-0000000000aa" {
		t.Errorf("TaskID = %q, want managed job only", resp.Tasks[0].TaskID)
	}
}
