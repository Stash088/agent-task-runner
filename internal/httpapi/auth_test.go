package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testKey = "correct-key"

func authTestHandler() http.Handler {
	return Auth(testKey, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("authorized"))
	}))
}

func TestAuthValidXAPIKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-API-Key", testKey)
	rec := httptest.NewRecorder()

	authTestHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "authorized" {
		t.Errorf("body = %q, want authorized", rec.Body.String())
	}
}

func TestAuthValidBearerToken(t *testing.T) {
	for _, header := range []string{"Bearer " + testKey, "bearer " + testKey} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()

		authTestHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("header %q: status = %d, want 200", header, rec.Code)
		}
	}
}

func TestAuthInvalidKey(t *testing.T) {
	for _, header := range []string{"wrong-key", ""} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if header != "" {
			req.Header.Set("X-API-Key", header)
		}
		rec := httptest.NewRecorder()

		authTestHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: status = %d, want 401", header, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("header %q: Content-Type = %q, want application/json", header, ct)
		}
		var body struct {
			JSONRPC string `json:"jsonrpc"`
			Error   struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("header %q: response is not JSON-RPC: %v (%s)", header, err, rec.Body.String())
			continue
		}
		if body.JSONRPC != "2.0" || body.Error.Code != unauthorizedCode || body.Error.Message == "" {
			t.Errorf("header %q: body = %+v, want JSON-RPC error %d", header, body, unauthorizedCode)
		}
	}
}

func TestAuthMissingKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()

	authTestHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthEmptyConfiguredKeyDeniesAll(t *testing.T) {
	handler := Auth("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("next handler called with empty configured API key")
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-API-Key", "anything")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
