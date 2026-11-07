// Package runner implements the core task lifecycle: validating task_run
// requests against the allowlist, building the worker Job specification, and
// orchestrating tasks against the Kubernetes API.
package runner

import "time"

// TaskState is the lifecycle state of a task, as reported to clients.
type TaskState string

const (
	StatePending   TaskState = "pending"
	StateRunning   TaskState = "running"
	StateSucceeded TaskState = "succeeded"
	StateFailed    TaskState = "failed"
	StateUnknown   TaskState = "unknown"
)

// TaskRunRequest is the input for task_run. Prompt and parameters reach the
// worker pod exclusively via environment variables; none of these fields are
// ever interpolated into a shell command.
type TaskRunRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Toolsets       string `json:"toolsets,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	CPU            string `json:"cpu,omitempty"`
	Memory         string `json:"memory,omitempty"`
}

// TaskRunResponse is the output of task_run.
type TaskRunResponse struct {
	TaskID    string    `json:"task_id"`
	State     TaskState `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	Pod       string    `json:"pod,omitempty"`
}

// TaskStatusRequest is the input for task_status.
type TaskStatusRequest struct {
	TaskID string `json:"task_id"`
}

// TaskStatusResponse is the output of task_status.
type TaskStatusResponse struct {
	TaskID         string     `json:"task_id"`
	State          TaskState  `json:"state"`
	StartTime      *time.Time `json:"start_time,omitempty"`
	CompletionTime *time.Time `json:"completion_time,omitempty"`
	ExitCode       *int32     `json:"exit_code,omitempty"`
	Message        string     `json:"message,omitempty"`
}

// TaskResultRequest is the input for task_result.
type TaskResultRequest struct {
	TaskID   string `json:"task_id"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
}

// TaskResultResponse is the output of task_result.
type TaskResultResponse struct {
	TaskID    string    `json:"task_id"`
	State     TaskState `json:"state"`
	ExitCode  *int32    `json:"exit_code,omitempty"`
	Log       string    `json:"log"`
	Truncated bool      `json:"truncated"`
	LogBytes  int64     `json:"log_bytes"`
	Message   string    `json:"message,omitempty"`
}

// TaskCancelRequest is the input for task_cancel.
type TaskCancelRequest struct {
	TaskID string `json:"task_id"`
}

// TaskCancelResponse is the output of task_cancel.
type TaskCancelResponse struct {
	TaskID  string `json:"task_id"`
	Deleted bool   `json:"deleted"`
}

// TaskListRequest is the input for task_list.
type TaskListRequest struct {
	Limit int    `json:"limit,omitempty"`
	State string `json:"state,omitempty"`
}

// TaskListItem is a single entry returned by task_list.
type TaskListItem struct {
	TaskID        string    `json:"task_id"`
	State         TaskState `json:"state"`
	CreatedAt     time.Time `json:"created_at"`
	PromptPreview string    `json:"prompt_preview"`
}

// TaskListResponse is the output of task_list.
type TaskListResponse struct {
	Tasks []TaskListItem `json:"tasks"`
	Count int            `json:"count"`
}
