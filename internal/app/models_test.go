package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func modelsUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func modelsBackend(t *testing.T, upstream *httptest.Server) *backend {
	t.Helper()
	return retryTestBackend(t, upstream, writeTestAuth(t, authDocument("access", nil)))
}

// serveModels answers GET /v1/models through the handler and returns the data entries.
func serveModels(t *testing.T, b codexBackend) []map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	newImagegenServer(b).models(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var document struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Object != "list" {
		t.Fatalf("object = %q", document.Object)
	}
	return document.Data
}

const catalogWithLimits = `{"models":[
	{"slug":"gpt-6.2-sol","display_name":"GPT-6.2 Sol","supported_in_api":true,"visibility":"list","context_window":500000,"max_context_window":872000},
	{"slug":"gpt-6-luna","supported_in_api":true,"visibility":"list","context_window":272000},
	{"slug":"gpt-odd","supported_in_api":true,"visibility":"list","context_window":"272000","max_context_window":1.5,"display_name":7},
	{"slug":"gpt-negative","supported_in_api":true,"visibility":"list","context_window":-1,"max_context_window":0},
	{"slug":"gpt-hidden","supported_in_api":true,"visibility":"hidden","context_window":500000},
	{"slug":"gpt-unsupported","supported_in_api":false,"visibility":"list","context_window":500000}
]}`

func TestListModelsKeepsUpstreamContextWindows(t *testing.T) {
	models := modelsBackend(t, modelsUpstream(t, http.StatusOK, catalogWithLimits)).listModels(context.Background())
	want := []modelInfo{
		{ID: "gpt-6.2-sol", DisplayName: "GPT-6.2 Sol", ContextWindow: 500000, MaxContextWindow: 872000},
		{ID: "gpt-6-luna", ContextWindow: 272000},
		{ID: "gpt-odd"},
		{ID: "gpt-negative"},
	}
	if len(models) != len(want) {
		t.Fatalf("models = %#v", models)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Errorf("models[%d] = %#v, want %#v", i, models[i], want[i])
		}
	}
}

func TestModelsEndpointCarriesContextWindowsOnlyWhenKnown(t *testing.T) {
	data := serveModels(t, modelsBackend(t, modelsUpstream(t, http.StatusOK, catalogWithLimits)))
	if len(data) != 4 {
		t.Fatalf("data = %#v", data)
	}
	sol := data[0]
	if sol["id"] != "gpt-6.2-sol" || sol["object"] != "model" || sol["owned_by"] != "codex" || sol["created"] != float64(0) {
		t.Fatalf("sol = %#v", sol)
	}
	if sol["context_window"] != float64(500000) || sol["max_context_window"] != float64(872000) || sol["display_name"] != "GPT-6.2 Sol" {
		t.Fatalf("sol limits = %#v", sol)
	}
	luna := data[1]
	if luna["context_window"] != float64(272000) {
		t.Fatalf("luna = %#v", luna)
	}
	if _, ok := luna["max_context_window"]; ok {
		t.Fatalf("luna max_context_window should be absent: %#v", luna)
	}
	for _, entry := range data[2:] {
		for _, field := range []string{"context_window", "max_context_window", "display_name"} {
			if _, ok := entry[field]; ok {
				t.Errorf("%v: %s should be absent for invalid upstream values: %#v", entry["id"], field, entry)
			}
		}
	}
}

func TestModelsFallbackCarriesNoContextWindows(t *testing.T) {
	for name, upstream := range map[string]*httptest.Server{
		"error":  modelsUpstream(t, http.StatusInternalServerError, `{"error":"boom"}`),
		"empty":  modelsUpstream(t, http.StatusOK, `{"models":[]}`),
		"broken": modelsUpstream(t, http.StatusOK, `not json`),
	} {
		t.Run(name, func(t *testing.T) {
			data := serveModels(t, modelsBackend(t, upstream))
			if len(data) != len(defaultModels) {
				t.Fatalf("data = %#v", data)
			}
			for i, entry := range data {
				if entry["id"] != defaultModels[i] {
					t.Errorf("data[%d].id = %v, want %s", i, entry["id"], defaultModels[i])
				}
				for _, field := range []string{"context_window", "max_context_window", "display_name"} {
					if _, ok := entry[field]; ok {
						t.Errorf("fallback %v carries %s: %#v", entry["id"], field, entry)
					}
				}
			}
		})
	}
}
