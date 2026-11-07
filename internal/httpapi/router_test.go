package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Stash088/agent-task-runner/internal/config"
	"github.com/Stash088/agent-task-runner/internal/mcpserver"
	"github.com/Stash088/agent-task-runner/internal/runner"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func routerTestConfig() *config.Config {
	return &config.Config{
		ListenAddr:             ":8080",
		APIKey:                 testKey,
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

type nopLogs struct{}

func (nopLogs) GetLogs(context.Context, string, string, string, int64) ([]byte, error) {
	return nil, nil
}

func newTestRouter(t *testing.T, client *fake.Clientset) http.Handler {
	t.Helper()
	cfg := routerTestConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := runner.NewService(client, nopLogs{}, cfg, logger)
	mcpServer := mcpserver.New(svc, "test", logger)
	return NewRouter(svc, mcpServer.MCP(), cfg, logger)
}

func TestHealthzNeedsNoKey(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	newTestRouter(t, fake.NewSimpleClientset()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}

func TestReadyzNeedsNoKey(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	newTestRouter(t, fake.NewSimpleClientset()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReadyzUnavailableWhenAPIErrors(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "jobs", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unreachable")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	newTestRouter(t, client).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestMCPRequiresKey(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")

	newTestRouter(t, fake.NewSimpleClientset()).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMCPWithKeyReachesHandler(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-API-Key", testKey)

	newTestRouter(t, fake.NewSimpleClientset()).ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatal("status = 401, want the authenticated MCP handler to be reached")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}
