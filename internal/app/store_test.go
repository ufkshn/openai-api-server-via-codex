package app

import (
	"net/http"
	"testing"
)

// A client that sends `store: false` resends its history itself; keeping its
// responses only filled memory (4.8 GB in production on 2026-11-27). Without
// `store`, a response is kept as before, so previous_response_id still works.
func TestResponsesHonorStoreFalse(t *testing.T) {
	environment := newContractEnvironment(t, nil)

	kept := environment.json(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": "gpt-5.6-luna", "input": "GO-STORE-DEFAULT",
	}, http.StatusOK)
	environment.json(t, http.MethodGet, "/v1/responses/"+stringValue(kept["id"]), nil, http.StatusOK)

	unkept := environment.json(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": "gpt-5.6-luna", "input": "GO-STORE-FALSE", "store": false,
	}, http.StatusOK)
	if stringValue(unkept["id"]) == "" {
		t.Fatalf("response without id: %#v", unkept)
	}
	environment.json(t, http.MethodGet, "/v1/responses/"+stringValue(unkept["id"]), nil, http.StatusNotFound)
	environment.json(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": "gpt-5.6-luna", "previous_response_id": stringValue(unkept["id"]), "input": "next",
	}, http.StatusNotFound)

	response, data := environment.request(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": "gpt-5.6-luna", "input": "GO-STORE-FALSE-STREAM", "store": false, "stream": true,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	var streamedID string
	for _, event := range parseSSE(t, data) {
		if event["type"] == "response.completed" {
			streamedID = stringValue(mapAny(event["response"])["id"])
		}
	}
	if streamedID == "" {
		t.Fatal("stream ended without a completed response")
	}
	environment.json(t, http.MethodGet, "/v1/responses/"+streamedID, nil, http.StatusNotFound)
}

func TestStoreRequested(t *testing.T) {
	cases := []struct {
		body map[string]any
		want bool
	}{
		{map[string]any{}, true},
		{map[string]any{"store": true}, true},
		{map[string]any{"store": false}, false},
		{map[string]any{"store": nil}, true},
	}
	for _, c := range cases {
		if got := storeRequested(c.body); got != c.want {
			t.Errorf("storeRequested(%#v) = %t, want %t", c.body, got, c.want)
		}
	}
}
