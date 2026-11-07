// Package httpapi wires the MCP endpoint, health probes, and authentication
// into a single HTTP handler.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// unauthorizedCode is the JSON-RPC server error code used for a missing or
// invalid API key. It is in the implementation-defined server error range.
const unauthorizedCode = -32001

// Auth wraps next with API-key authentication. A key may be supplied either as
// X-API-Key or as an Authorization: Bearer token. Comparison is constant-time.
func Auth(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiKey != "" && matchesAPIKey(r, apiKey) {
			next.ServeHTTP(w, r)
			return
		}
		writeUnauthorized(w)
	})
}

func matchesAPIKey(r *http.Request, apiKey string) bool {
	got := r.Header.Get("X-API-Key")
	if got == "" {
		auth := r.Header.Get("Authorization")
		scheme, token, ok := strings.Cut(auth, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			got = strings.TrimSpace(token)
		}
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(apiKey)) == 1
}

// writeUnauthorized emits a JSON-RPC 2.0 error response, which is what an MCP
// client can surface meaningfully.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Bearer realm="agent-task-runner"`)
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error": map[string]any{
			"code":    unauthorizedCode,
			"message": "unauthorized",
		},
	})
}
