package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/Stash088/agent-task-runner/internal/config"
	"github.com/Stash088/agent-task-runner/internal/runner"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type noopLogGetter struct{}

func (noopLogGetter) GetLogs(context.Context, string, string, string, int64) ([]byte, error) {
	return nil, nil
}

func testConfig() *config.Config {
	return &config.Config{
		ListenAddr:             ":8080",
		APIKey:                 "test-key",
		WorkerNamespace:        "agent-tasks",
		WorkerImage:            "nousresearch/hermes-agent@sha256:0000000000000000000000000000000000000000000000000000000000000000",
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

func newTestSession(t *testing.T) (*mcp.ClientSession, *fake.Clientset) {
	t.Helper()

	client := fake.NewSimpleClientset()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := runner.NewService(client, noopLogGetter{}, testConfig(), logger)
	srv := New(svc, "test", logger)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.MCP().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connection: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connection: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession, client
}

func TestListTools(t *testing.T) {
	session, _ := newTestSession(t)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(res.Tools) != 5 {
		t.Fatalf("len(Tools) = %d, want 5", len(res.Tools))
	}

	want := map[string]bool{
		"task_run":    false,
		"task_status": false,
		"task_result": false,
		"task_cancel": false,
		"task_list":   false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		want[tool.Name] = true
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q missing from tools/list", name)
		}
	}
}

func TestCallTaskRunCreatesJob(t *testing.T) {
	session, client := newTestSession(t)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_run",
		Arguments: map[string]any{
			"prompt": "Reply with exactly: E2E-OK",
			"model":  "deepseek-v4.1-flash",
			"cpu":    "250m",
			"memory": "512Mi",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() IsError = true, content = %v", res.Content)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out runner.TaskRunResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content %s: %v", raw, err)
	}
	if !strings.HasPrefix(out.TaskID, "atr-") {
		t.Fatalf("TaskID = %q, want atr- prefix", out.TaskID)
	}
	if out.State != runner.StatePending {
		t.Errorf("State = %q, want pending", out.State)
	}

	job, err := client.BatchV1().Jobs("agent-tasks").Get(context.Background(), out.TaskID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created job %q: %v", out.TaskID, err)
	}
	if got := job.Annotations[runner.AnnPromptPreview]; got != "Reply with exactly: E2E-OK" {
		t.Errorf("prompt preview = %q, want prompt", got)
	}
}

func TestCallTaskRunRejectsInvalidPrompt(t *testing.T) {
	session, client := newTestSession(t)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_run",
		Arguments: map[string]any{"prompt": ""},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() IsError = false, want tool error for empty prompt")
	}
	if !contentContains(res.Content, "invalid_argument") {
		t.Errorf("content = %v, want invalid_argument", res.Content)
	}

	jobs, err := client.BatchV1().Jobs("agent-tasks").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("created %d jobs for invalid request, want 0", len(jobs.Items))
	}
}

func contentContains(content []mcp.Content, substr string) bool {
	for _, c := range content {
		if text, ok := c.(*mcp.TextContent); ok && strings.Contains(text.Text, substr) {
			return true
		}
	}
	return false
}
