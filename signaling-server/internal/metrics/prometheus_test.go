package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQueryAccountBytes_ReturnsBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []map[string]any{
					{
						"metric": map[string]string{},
						"value":  []any{1700000000, "12345678"},
					},
				},
			},
		})
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	got, err := client.QueryAccountBytes(context.Background(), "acc001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 12345678 {
		t.Errorf("QueryAccountBytes = %d, want 12345678", got)
	}
}

func TestQueryAccountBytes_NoDataReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result":     []any{},
			},
		})
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	got, err := client.QueryAccountBytes(context.Background(), "acc001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("QueryAccountBytes = %d, want 0", got)
	}
}

func TestQueryAccountBytes_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	_, err := client.QueryAccountBytes(context.Background(), "acc001")
	if err == nil {
		t.Error("expected error from 500 response, got nil")
	}
}

func TestQueryAccountBytes_IncludesAccountIDInQuery(t *testing.T) {
	var capturedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": []any{}},
		})
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	client.QueryAccountBytes(context.Background(), "myaccount123")

	if capturedQuery == "" {
		t.Fatal("no query sent to Prometheus")
	}
	if !contains(capturedQuery, "myaccount123") {
		t.Errorf("query %q does not contain account ID", capturedQuery)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsHelper(s, sub))
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
