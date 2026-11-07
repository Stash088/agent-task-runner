// Package mcpserver exposes the task lifecycle as an MCP server.
//
// The five tools mirror the contract in section 1 of the implementation plan.
// All task input is validated by the runner package before a Job is created;
// the MCP layer only translates JSON arguments and maps errors to tool errors.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Stash088/agent-task-runner/internal/runner"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server wraps an MCP server and the runner service it exposes.
type Server struct {
	svc *runner.Service
	mcp *mcp.Server
}

// New builds an MCP server with all five task tools registered.
func New(svc *runner.Service, version string, logger *slog.Logger) *Server {
	if version == "" {
		version = "dev"
	}
	var opts *mcp.ServerOptions
	if logger != nil {
		opts = &mcp.ServerOptions{Logger: logger}
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "agent-task-runner",
		Version: version,
	}, opts)

	s := &Server{svc: svc, mcp: srv}
	s.registerTools()
	return s
}

// MCP returns the underlying MCP server, ready to be mounted on an HTTP
// handler.
func (s *Server) MCP() *mcp.Server {
	return s.mcp
}

// taskRunInput mirrors the task_run schema from section 1 of the plan. Prompt
// is required; everything else is optional and filled with service defaults.
type taskRunInput struct {
	Prompt         string `json:"prompt" jsonschema:"task for hermes -z, 1..262144 bytes"`
	Model          string `json:"model,omitempty" jsonschema:"optional model; must match ALLOWED_MODELS"`
	Provider       string `json:"provider,omitempty" jsonschema:"optional provider; must be in ALLOWED_PROVIDERS"`
	Toolsets       string `json:"toolsets,omitempty" jsonschema:"optional comma-separated toolsets; each must be in ALLOWED_TOOLSETS"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"optional timeout in seconds; clamped to [60, MAX_TIMEOUT_SECONDS]"`
	CPU            string `json:"cpu,omitempty" jsonschema:"optional cpu request; clamped to [100m, MAX_CPU]"`
	Memory         string `json:"memory,omitempty" jsonschema:"optional memory request; clamped to [256Mi, MAX_MEMORY]"`
}

type taskStatusInput struct {
	TaskID string `json:"task_id" jsonschema:"task id returned by task_run"`
}

type taskResultInput struct {
	TaskID   string `json:"task_id" jsonschema:"task id returned by task_run"`
	MaxBytes int64  `json:"max_bytes,omitempty" jsonschema:"optional maximum log bytes; capped at MAX_LOG_BYTES"`
}

type taskCancelInput struct {
	TaskID string `json:"task_id" jsonschema:"task id returned by task_run"`
}

type taskListInput struct {
	Limit int    `json:"limit,omitempty" jsonschema:"optional maximum number of tasks; defaults to 20"`
	State string `json:"state,omitempty" jsonschema:"optional state filter: running, succeeded, or failed"`
}

func (s *Server) registerTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "task_run",
		Description: "Start a Hermes agent task in an isolated Kubernetes Job and return its task id.",
	}, s.handleTaskRun)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "task_status",
		Description: "Report the current state, timing, exit code, and message of a task.",
	}, s.handleTaskStatus)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "task_result",
		Description: "Return the worker container log (bounded by max_bytes) and final state of a task.",
	}, s.handleTaskResult)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "task_cancel",
		Description: "Delete the task Job with background propagation.",
	}, s.handleTaskCancel)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "task_list",
		Description: "List managed tasks newest first, optionally filtered by state.",
	}, s.handleTaskList)
}

func (s *Server) handleTaskRun(ctx context.Context, _ *mcp.CallToolRequest, in taskRunInput) (*mcp.CallToolResult, *runner.TaskRunResponse, error) {
	resp, err := s.svc.Run(ctx, &runner.TaskRunRequest{
		Prompt:         in.Prompt,
		Model:          in.Model,
		Provider:       in.Provider,
		Toolsets:       in.Toolsets,
		TimeoutSeconds: in.TimeoutSeconds,
		CPU:            in.CPU,
		Memory:         in.Memory,
	})
	if err != nil {
		return nil, nil, toolError(err)
	}
	return nil, resp, nil
}

func (s *Server) handleTaskStatus(ctx context.Context, _ *mcp.CallToolRequest, in taskStatusInput) (*mcp.CallToolResult, *runner.TaskStatusResponse, error) {
	resp, err := s.svc.Status(ctx, runner.TaskStatusRequest{TaskID: in.TaskID})
	if err != nil {
		return nil, nil, toolError(err)
	}
	return nil, resp, nil
}

func (s *Server) handleTaskResult(ctx context.Context, _ *mcp.CallToolRequest, in taskResultInput) (*mcp.CallToolResult, *runner.TaskResultResponse, error) {
	resp, err := s.svc.Result(ctx, runner.TaskResultRequest{TaskID: in.TaskID, MaxBytes: in.MaxBytes})
	if err != nil {
		return nil, nil, toolError(err)
	}
	return nil, resp, nil
}

func (s *Server) handleTaskCancel(ctx context.Context, _ *mcp.CallToolRequest, in taskCancelInput) (*mcp.CallToolResult, *runner.TaskCancelResponse, error) {
	resp, err := s.svc.Cancel(ctx, runner.TaskCancelRequest{TaskID: in.TaskID})
	if err != nil {
		return nil, nil, toolError(err)
	}
	return nil, resp, nil
}

func (s *Server) handleTaskList(ctx context.Context, _ *mcp.CallToolRequest, in taskListInput) (*mcp.CallToolResult, *runner.TaskListResponse, error) {
	resp, err := s.svc.List(ctx, runner.TaskListRequest{Limit: in.Limit, State: in.State})
	if err != nil {
		return nil, nil, toolError(err)
	}
	return nil, resp, nil
}

// toolError maps runner errors onto the error categories from the MCP
// contract. The returned error is turned into a tool error (IsError=true) by
// the SDK, so the client can distinguish busy/invalid_argument/internal.
func toolError(err error) error {
	var ve *runner.ValidationError
	switch {
	case errors.As(err, &ve):
		return err // already prefixed with "invalid_argument"
	case errors.Is(err, runner.ErrBusy):
		return fmt.Errorf("busy: %w", err)
	default:
		return fmt.Errorf("internal: %w", err)
	}
}
