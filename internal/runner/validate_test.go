package runner

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	cfg := testConfig()

	tests := []struct {
		name string
		req  TaskRunRequest
		// wantErr asserts rejection as a *ValidationError.
		wantErr bool
		// normalisation expectations (checked only when wantErr is false and
		// the corresponding "want" value is non-zero).
		wantTimeout int
		wantCPU     string
		wantMemory  string
	}{
		{
			name:        "valid full request",
			req:         TaskRunRequest{Prompt: "summarize", Model: "deepseek-v4.1-flash", Provider: "opencode-go", Toolsets: "web,files", TimeoutSeconds: 120, CPU: "500m", Memory: "1Gi"},
			wantTimeout: 120,
			wantCPU:     "500m",
			wantMemory:  "1Gi",
		},
		{name: "valid minimal request", req: TaskRunRequest{Prompt: "hi"}},
		{name: "empty prompt rejected", req: TaskRunRequest{Prompt: ""}, wantErr: true},
		{
			name:    "prompt over max bytes rejected",
			req:     TaskRunRequest{Prompt: strings.Repeat("a", MaxPromptBytes+1)},
			wantErr: true,
		},
		{name: "prompt exactly at max bytes", req: TaskRunRequest{Prompt: strings.Repeat("a", MaxPromptBytes)}},
		{name: "prompt with semicolon injection accepted", req: TaskRunRequest{Prompt: `"; rm -rf /`}},
		{name: "prompt with command substitution accepted", req: TaskRunRequest{Prompt: `$(id)`}},
		{name: "prompt with backtick accepted", req: TaskRunRequest{Prompt: "`touch /tmp/pwned`"}},
		{name: "prompt with newline accepted", req: TaskRunRequest{Prompt: "line one\nline two\nline three"}},
		{name: "prompt with NUL byte rejected", req: TaskRunRequest{Prompt: "a\x00b"}, wantErr: true},
		{name: "prompt with unicode accepted", req: TaskRunRequest{Prompt: "привет мир 🌍"}},
		{name: "model deepseek allowed", req: TaskRunRequest{Prompt: "x", Model: "deepseek-v4.2-flash"}},
		{name: "model glm allowed", req: TaskRunRequest{Prompt: "x", Model: "glm-5.1-flash"}},
		{name: "model qwen allowed", req: TaskRunRequest{Prompt: "x", Model: "qwen3.2-flash"}},
		{name: "model not in regex rejected", req: TaskRunRequest{Prompt: "x", Model: "gpt-4o"}, wantErr: true},
		{name: "model empty accepted", req: TaskRunRequest{Prompt: "x", Model: ""}},
		{name: "provider allowed", req: TaskRunRequest{Prompt: "x", Provider: "zai"}},
		{name: "provider disallowed rejected", req: TaskRunRequest{Prompt: "x", Provider: "anthropic"}, wantErr: true},
		{name: "provider empty accepted", req: TaskRunRequest{Prompt: "x", Provider: ""}},
		{name: "toolsets allowed", req: TaskRunRequest{Prompt: "x", Toolsets: "web,terminal"}},
		{name: "toolsets with whitespace accepted", req: TaskRunRequest{Prompt: "x", Toolsets: " web , files "}},
		{name: "toolsets empty accepted", req: TaskRunRequest{Prompt: "x", Toolsets: ""}},
		{name: "toolset disallowed rejected", req: TaskRunRequest{Prompt: "x", Toolsets: "web,shell"}, wantErr: true},
		{name: "timeout clamped up to min", req: TaskRunRequest{Prompt: "x", TimeoutSeconds: 10}, wantTimeout: 60},
		{name: "timeout clamped down to max", req: TaskRunRequest{Prompt: "x", TimeoutSeconds: 999999}, wantTimeout: 7200},
		{name: "negative timeout clamps to min", req: TaskRunRequest{Prompt: "x", TimeoutSeconds: -5}, wantTimeout: 60},
		{name: "cpu clamped up to min", req: TaskRunRequest{Prompt: "x", CPU: "10m"}, wantCPU: "100m"},
		{name: "cpu clamped down to max", req: TaskRunRequest{Prompt: "x", CPU: "999"}, wantCPU: "2"},
		{name: "cpu unparseable rejected", req: TaskRunRequest{Prompt: "x", CPU: "abc"}, wantErr: true},
		{name: "memory clamped up to min", req: TaskRunRequest{Prompt: "x", Memory: "10Mi"}, wantMemory: "256Mi"},
		{name: "memory clamped down to max", req: TaskRunRequest{Prompt: "x", Memory: "99Gi"}, wantMemory: "4Gi"},
		{name: "memory unparseable rejected", req: TaskRunRequest{Prompt: "x", Memory: "zzz"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			err := Validate(&req, cfg)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Validate() error = nil, want *ValidationError")
				}
				if _, ok := err.(*ValidationError); !ok {
					t.Fatalf("Validate() error type = %T, want *ValidationError", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			if tt.wantTimeout != 0 && req.TimeoutSeconds != tt.wantTimeout {
				t.Errorf("TimeoutSeconds = %d, want %d", req.TimeoutSeconds, tt.wantTimeout)
			}
			if tt.wantCPU != "" && req.CPU != tt.wantCPU {
				t.Errorf("CPU = %q, want %q", req.CPU, tt.wantCPU)
			}
			if tt.wantMemory != "" && req.Memory != tt.wantMemory {
				t.Errorf("Memory = %q, want %q", req.Memory, tt.wantMemory)
			}
		})
	}
}

func TestValidateNilRequest(t *testing.T) {
	if err := Validate(nil, testConfig()); err == nil {
		t.Fatal("Validate(nil) error = nil, want *ValidationError")
	} else if _, ok := err.(*ValidationError); !ok {
		t.Fatalf("Validate(nil) error type = %T, want *ValidationError", err)
	}
}

// TestValidateInjectionPromptNotRejected is the explicit assertion that a
// shell-injection prompt passes validation unchanged: the prompt is a payload
// carried in an environment variable, never an executable fragment.
func TestValidateInjectionPromptNotRejected(t *testing.T) {
	prompt := `"; rm -rf /; $(id); ` + "`cat /etc/passwd`" + "\nsecond line"
	req := TaskRunRequest{Prompt: prompt}
	if err := Validate(&req, testConfig()); err != nil {
		t.Fatalf("Validate() error = %v, want nil (injection prompt must be accepted)", err)
	}
	if req.Prompt != prompt {
		t.Errorf("prompt mutated: %q, want %q", req.Prompt, prompt)
	}
}
