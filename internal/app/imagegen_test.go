package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// scriptedBackend answers each /codex/responses round with the next scripted
// event list and every Images-endpoint call with one fixed picture, recording
// what it was sent.
type scriptedBackend struct {
	mu       sync.Mutex
	rounds   [][]map[string]any
	payloads []map[string]any
	images   []map[string]any
	edits    []bool
}

func (b *scriptedBackend) stream(_ context.Context, payload map[string]any, fn func(map[string]any) error) error {
	b.mu.Lock()
	b.payloads = append(b.payloads, cloneMap(payload))
	events := b.rounds[0]
	b.rounds = b.rounds[1:]
	b.mu.Unlock()
	for _, event := range events {
		if err := fn(cloneMap(event)); err != nil {
			return err
		}
	}
	return nil
}
func (b *scriptedBackend) collect(context.Context, map[string]any) (map[string]any, error) {
	panic("collect is not used by the imagegen path")
}
func (b *scriptedBackend) listModels(context.Context) []string { return nil }
func (b *scriptedBackend) proxy(context.Context, string, string, string, http.Header, io.Reader) (*http.Response, error) {
	panic("proxy is not used by the imagegen path")
}
func (b *scriptedBackend) transcribe(context.Context, http.Header, io.Reader) (*http.Response, error) {
	panic("transcribe is not used by the imagegen path")
}
func (b *scriptedBackend) imagesCall(_ context.Context, edit bool, body map[string]any) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.images = append(b.images, cloneMap(body))
	b.edits = append(b.edits, edit)
	return map[string]any{
		"created": float64(1700000000), "background": body["background"], "output_format": "png", "quality": "low", "size": "1536x1024",
		"data":  []any{map[string]any{"b64_json": "UElDVFVSRQ=="}},
		"usage": map[string]any{"input_tokens": float64(10), "output_tokens": float64(343), "total_tokens": float64(353)},
	}, nil
}

func roundEvents(id string, items ...map[string]any) []map[string]any {
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": id, "created_at": float64(1), "status": "in_progress"}},
	}
	for index, item := range items {
		events = append(events, map[string]any{"type": "response.output_item.added", "output_index": float64(index), "item": map[string]any{"id": item["id"], "type": item["type"], "name": item["name"]}})
		if item["type"] == "function_call" {
			events = append(events, map[string]any{"type": "response.function_call_arguments.delta", "output_index": float64(index), "item_id": item["id"], "delta": "{}"})
		}
		events = append(events, map[string]any{"type": "response.output_item.done", "output_index": float64(index), "item": item})
	}
	return append(events, map[string]any{"type": "response.completed", "response": map[string]any{
		"id": id, "status": "completed", "usage": map[string]any{"input_tokens": float64(100), "output_tokens": float64(20), "total_tokens": float64(120)},
	}})
}

func imagegenCall(id, arguments string) map[string]any {
	return map[string]any{"id": id, "type": "function_call", "call_id": "call_" + id, "name": imagegenToolName, "arguments": arguments, "status": "completed"}
}

func assistantText(id, text string) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func newImagegenServer(backend codexBackend) *server {
	return &server{cfg: config{Model: "gpt-6-astra"}, backend: backend, responses: newResponseStore(8), chats: newChatStore(1)}
}

func postJSON(t *testing.T, s *server, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(data)))
	request.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(recorder, request)
	return recorder
}

func TestImagegenResponseDrawsOnImagesEndpointAndShowsImageItem(t *testing.T) {
	backend := &scriptedBackend{rounds: [][]map[string]any{
		roundEvents("resp_1", imagegenCall("fc_1", `{"prompt":"a glass cube","transparent_background":true,"num_last_images_to_include":1}`)),
		roundEvents("resp_2", assistantText("msg_1", "Here it is.")),
	}}
	s := newImagegenServer(backend)
	recorder := postJSON(t, s, "/imagegen/v1/responses", map[string]any{
		"input": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "draw it in glass"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,UkVG"},
		}}},
		"tools": []any{map[string]any{"type": "image_generation", "size": "auto"}}, "tool_choice": map[string]any{"type": "image_generation"}, "max_tool_calls": 1,
	})
	if recorder.Code != 200 {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)

	// The model was offered the imagegen function, forced on round one only, and never saw max_tool_calls.
	first, second := backend.payloads[0], backend.payloads[1]
	if tool := mapAny(sliceAny(first["tools"])[0]); tool["type"] != "function" || tool["name"] != imagegenToolName {
		t.Fatalf("round one tools = %v", first["tools"])
	}
	if choice := mapAny(first["tool_choice"]); choice["name"] != imagegenToolName {
		t.Fatalf("round one tool_choice = %v", first["tool_choice"])
	}
	if second["tool_choice"] != "none" {
		t.Fatalf("round two tool_choice = %v, want none once max_tool_calls is spent", second["tool_choice"])
	}
	if _, ok := first["max_tool_calls"]; ok {
		t.Fatal("max_tool_calls reached Codex")
	}
	// Round two carries the call and its picture back to the model.
	input := sliceAny(second["input"])
	call, output := mapAny(input[len(input)-2]), mapAny(input[len(input)-1])
	if call["type"] != "function_call" || output["type"] != "function_call_output" || output["call_id"] != "call_fc_1" {
		t.Fatalf("round two tail = %v / %v", call, output)
	}
	if part := mapAny(sliceAny(output["output"])[0]); part["type"] != "input_image" || part["image_url"] != "data:image/png;base64,UElDVFVSRQ==" {
		t.Fatalf("function output = %v", output["output"])
	}
	// The Images endpoint got the CLI's request, editing the conversation's picture.
	if len(backend.images) != 1 || !backend.edits[0] {
		t.Fatalf("images calls = %v edits = %v", backend.images, backend.edits)
	}
	image := backend.images[0]
	if image["model"] != imagegenImageModel || image["quality"] != "auto" || image["size"] != "auto" || image["background"] != "transparent" || image["prompt"] != "a glass cube" {
		t.Fatalf("images request = %v", image)
	}
	if ref := mapAny(sliceAny(image["images"])[0]); ref["image_url"] != "data:image/png;base64,UkVG" {
		t.Fatalf("images request refs = %v", image["images"])
	}
	// The client sees one response: the first round's id, the image item, the closing text, summed usage, its own tools.
	if response["id"] != "resp_1" {
		t.Fatalf("response id = %v", response["id"])
	}
	output0, output1 := mapAny(sliceAny(response["output"])[0]), mapAny(sliceAny(response["output"])[1])
	if output0["type"] != "image_generation_call" || output0["status"] != "completed" || output0["result"] != "UElDVFVSRQ==" || output0["quality"] != "low" {
		t.Fatalf("output[0] = %v", output0)
	}
	if output1["type"] != "message" || len(sliceAny(response["output"])) != 2 {
		t.Fatalf("output = %v", response["output"])
	}
	if usage := mapAny(response["usage"]); intValue(usage["total_tokens"]) != 240 {
		t.Fatalf("usage = %v", usage)
	}
	if tool := mapAny(sliceAny(response["tools"])[0]); tool["type"] != "image_generation" {
		t.Fatalf("response tools = %v", response["tools"])
	}
}

func TestImagegenResponseStreamRenumbersAndHidesFunctionCall(t *testing.T) {
	backend := &scriptedBackend{rounds: [][]map[string]any{
		roundEvents("resp_1", assistantText("msg_0", "Drawing."), imagegenCall("fc_1", `{"prompt":"a red cube"}`)),
		roundEvents("resp_2", assistantText("msg_1", "Done.")),
	}}
	s := newImagegenServer(backend)
	recorder := postJSON(t, s, "/imagegen/v1/responses", map[string]any{
		"input": "draw a red cube", "stream": true, "tools": []any{map[string]any{"type": "image_generation"}},
	})
	var events []map[string]any
	scanner := bufio.NewScanner(recorder.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			var event map[string]any
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
			events = append(events, event)
		}
	}
	var types []string
	for i, event := range events {
		if intValue(event["sequence_number"]) != i {
			t.Fatalf("event %d sequence_number = %v", i, event["sequence_number"])
		}
		if strings.Contains(stringValue(event["type"]), "function_call") || mapAny(event["item"])["type"] == "function_call" {
			t.Fatalf("function call leaked to the client: %v", event)
		}
		types = append(types, stringValue(event["type"]))
	}
	want := []string{
		"response.created",
		"response.output_item.added", "response.output_item.done", // msg_0, index 0
		"response.output_item.added", "response.output_item.done", // image, index 1
		"response.output_item.added", "response.output_item.done", // msg_1, index 2
		"response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v", types)
	}
	for i, wantIndex := range []int{0, 0, 1, 1, 2, 2} {
		if got := intValue(events[i+1]["output_index"]); got != wantIndex {
			t.Fatalf("event %d output_index = %d, want %d", i+1, got, wantIndex)
		}
	}
	// A call without num_last_images_to_include draws a new picture.
	if backend.edits[0] || backend.images[0]["images"] != nil {
		t.Fatal("a new picture was sent as an edit")
	}
	final := mapAny(events[len(events)-1]["response"])
	if len(sliceAny(final["output"])) != 3 || final["id"] != "resp_1" {
		t.Fatalf("final response = %v", final)
	}
	// A follow-up continues from the model's real context: its call and the call's output.
	stored := s.responses.get("resp_1")
	var sawCall, sawOutput bool
	for _, raw := range stored.Context {
		item := mapAny(raw)
		sawCall = sawCall || item["type"] == "function_call"
		sawOutput = sawOutput || item["type"] == "function_call_output"
	}
	if !sawCall || !sawOutput {
		t.Fatalf("stored context = %v", stored.Context)
	}
}

// unansweredCalls lists the function calls in a stored context that have no
// function_call_output; Codex rejects a follow-up that carries one.
func unansweredCalls(context []any) []string {
	answered := map[string]bool{}
	for _, raw := range context {
		if item := mapAny(raw); item["type"] == "function_call_output" {
			answered[stringValue(item["call_id"])] = true
		}
	}
	var missing []string
	for _, raw := range context {
		if item := mapAny(raw); item["type"] == "function_call" && !answered[stringValue(item["call_id"])] {
			missing = append(missing, stringValue(item["call_id"]))
		}
	}
	return missing
}

func TestImagegenResponseStopsForTheCallersOwnToolCall(t *testing.T) {
	lookup := map[string]any{"id": "fc_2", "type": "function_call", "call_id": "call_fc_2", "name": "lookup", "arguments": `{"q":"cube"}`, "status": "completed"}
	backend := &scriptedBackend{rounds: [][]map[string]any{
		roundEvents("resp_1", imagegenCall("fc_1", `{"prompt":"a red cube"}`), lookup),
	}}
	s := newImagegenServer(backend)
	recorder := postJSON(t, s, "/imagegen/v1/responses", map[string]any{
		"input": "draw a cube and look it up",
		"tools": []any{map[string]any{"type": "image_generation"}, map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
	})
	if recorder.Code != 200 {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	// The run ends after the round, without asking Codex to continue past the caller's call.
	if len(backend.payloads) != 1 || len(backend.images) != 1 {
		t.Fatalf("rounds = %d draws = %d", len(backend.payloads), len(backend.images))
	}
	var response map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)
	var types []string
	for _, raw := range sliceAny(response["output"]) {
		types = append(types, stringValue(mapAny(raw)["type"])+":"+stringValue(mapAny(raw)["name"]))
	}
	if strings.Join(types, ",") != "function_call:lookup,image_generation_call:" {
		t.Fatalf("output = %v", types)
	}
	// Only the caller's own call waits for an output; the drawing is answered.
	if missing := unansweredCalls(s.responses.get("resp_1").Context); strings.Join(missing, ",") != "call_fc_2" {
		t.Fatalf("unanswered calls = %v", missing)
	}
}

func TestImagegenResponseAnswersCallsMadeAtTheRoundLimit(t *testing.T) {
	backend := &scriptedBackend{rounds: [][]map[string]any{
		roundEvents("resp_1", imagegenCall("fc_1", `{"prompt":"a red cube"}`)),
		// The last round forbids tools, yet the model calls imagegen again.
		roundEvents("resp_2", imagegenCall("fc_2", `{"prompt":"a blue cube"}`)),
	}}
	s := newImagegenServer(backend)
	recorder := postJSON(t, s, "/imagegen/v1/responses", map[string]any{
		"input": "draw cubes", "tools": []any{map[string]any{"type": "image_generation"}}, "max_tool_calls": 1,
	})
	if recorder.Code != 200 {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	if len(backend.payloads) != 2 || len(backend.images) != 1 || backend.payloads[1]["tool_choice"] != "none" {
		t.Fatalf("rounds = %d draws = %d last tool_choice = %v", len(backend.payloads), len(backend.images), backend.payloads[1]["tool_choice"])
	}
	if missing := unansweredCalls(s.responses.get("resp_1").Context); len(missing) != 0 {
		t.Fatalf("unanswered calls = %v", missing)
	}
}

func TestImagegenResponseDefaultsToEightDraws(t *testing.T) {
	var rounds [][]map[string]any
	for i := 0; i < imagegenMaxDraws; i++ {
		rounds = append(rounds, roundEvents("resp_"+string(rune('a'+i)), imagegenCall("fc_"+string(rune('a'+i)), `{"prompt":"a cube"}`)))
	}
	backend := &scriptedBackend{rounds: append(rounds, roundEvents("resp_z", assistantText("msg_1", "Done.")))}
	s := newImagegenServer(backend)
	recorder := postJSON(t, s, "/imagegen/v1/responses", map[string]any{"input": "draw cubes", "tools": []any{map[string]any{"type": "image_generation"}}})
	if recorder.Code != 200 || len(backend.images) != imagegenMaxDraws || len(backend.payloads) != imagegenMaxDraws+1 {
		t.Fatalf("status = %d draws = %d rounds = %d", recorder.Code, len(backend.images), len(backend.payloads))
	}
}

func TestImagegenResponseWithoutImageToolUsesTheUsualPath(t *testing.T) {
	backend := &scriptedBackend{rounds: [][]map[string]any{roundEvents("resp_1", assistantText("msg_1", "hi"))}}
	s := newImagegenServer(backend)
	recorder := postJSON(t, s, "/imagegen/v1/responses", map[string]any{"input": "hello", "stream": true})
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"hi"`) || len(backend.images) != 0 {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestImagegenImagesEditSendsCLIRequest(t *testing.T) {
	backend := &scriptedBackend{}
	s := newImagegenServer(backend)
	recorder := postJSON(t, s, "/imagegen/v1/images/edits", map[string]any{
		"model": "gpt-image-2", "prompt": "make it granite", "image": []any{"data:image/png;base64,QQ==", "data:image/png;base64,Qg=="}, "background": "transparent",
	})
	if recorder.Code != 200 {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	var result map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &result)
	if mapAny(sliceAny(result["data"])[0])["b64_json"] != "UElDVFVSRQ==" || result["quality"] != "low" || result["size"] != "1536x1024" {
		t.Fatalf("result = %v", result)
	}
	request := backend.images[0]
	if !backend.edits[0] || len(sliceAny(request["images"])) != 2 || request["background"] != "transparent" || request["model"] != imagegenImageModel {
		t.Fatalf("images request = %v", request)
	}
}

func TestImagegenRouteLeavesV1Alone(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/imagegen/v1/images/generations", nil)
	if !imagegenRoute(request) || request.URL.Path != "/v1/images/generations" {
		t.Fatalf("imagegen route = %s", request.URL.Path)
	}
	for _, path := range []string{"/v1/images/generations", "/imagegenx/v1/responses", "/imagegen/v2/responses"} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		if imagegenRoute(request) || request.URL.Path != path {
			t.Fatalf("%s was taken as an imagegen route", path)
		}
	}
}

func TestRecentImagesCountsNewestFirstAndReturnsOldestFirst(t *testing.T) {
	items := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_image", "image_url": "a"},
			map[string]any{"type": "input_text", "text": "x"},
			map[string]any{"type": "input_image", "image_url": map[string]any{"url": "b"}},
		}},
		map[string]any{"type": "image_generation_call", "result": "QQ=="},
		map[string]any{"type": "function_call_output", "output": []any{map[string]any{"type": "input_image", "image_url": "c"}}},
	}
	if got := recentImages(items, 3); strings.Join(got, ",") != "b,data:image/png;base64,QQ==,c" {
		t.Fatalf("recent 3 = %v", got)
	}
	if got := recentImages(items, 9); len(got) != 4 || got[0] != "a" {
		t.Fatalf("recent 9 = %v", got)
	}
}

func imagesUpstreamBackend(t *testing.T, data []any) *backend {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1700000000, "output_format": "png", "data": data})
	}))
	t.Cleanup(upstream.Close)
	cfg := defaultConfig()
	cfg.BackendURL = upstream.URL
	cfg.AuthJSON = writeTestAuth(t, authDocument("access", map[string]any{"account_id": "acct_imagegen"}))
	return newBackend(cfg)
}

func TestImagegenImagesStreamWithoutBase64FailsInsteadOfPanicking(t *testing.T) {
	s := newImagegenServer(imagesUpstreamBackend(t, []any{map[string]any{"url": "https://example.test/picture.png"}}))
	recorder := postJSON(t, s, "/imagegen/v1/images/generations", map[string]any{"prompt": "a cube", "stream": true})
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "carried no image") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("status = %d body = %s", recorder.Code, body)
	}
}

func TestImagegenImagesKeepOnlyBase64Pictures(t *testing.T) {
	s := newImagegenServer(imagesUpstreamBackend(t, []any{
		map[string]any{"url": "https://example.test/picture.png"}, map[string]any{"b64_json": "QQ=="},
	}))
	recorder := postJSON(t, s, "/imagegen/v1/images/generations", map[string]any{"prompt": "a cube"})
	var result map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &result)
	if data := sliceAny(result["data"]); recorder.Code != 200 || len(data) != 1 || mapAny(data[0])["b64_json"] != "QQ==" {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestImagegenRefusesOutputFormatItCannotHonour(t *testing.T) {
	backend := &scriptedBackend{}
	s := newImagegenServer(backend)
	for _, request := range []struct {
		path string
		body map[string]any
	}{
		{"/imagegen/v1/images/generations", map[string]any{"prompt": "a cube", "output_format": "jpeg"}},
		{"/imagegen/v1/responses", map[string]any{"input": "a cube", "stream": true, "tools": []any{map[string]any{"type": "image_generation", "output_format": "webp"}}}},
	} {
		recorder := postJSON(t, s, request.path, request.body)
		if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "use /v1 for jpeg or webp") {
			t.Fatalf("%s: status = %d body = %s", request.path, recorder.Code, recorder.Body.String())
		}
	}
	if len(backend.payloads) != 0 || len(backend.images) != 0 {
		t.Fatal("a refused request reached Codex")
	}
	// png, the only format the endpoint is known to return, is still accepted.
	if recorder := postJSON(t, s, "/imagegen/v1/images/generations", map[string]any{"prompt": "a cube", "output_format": "png"}); recorder.Code != 200 {
		t.Fatalf("png: status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestImagegenCallWithoutItemIDHidesOnlyItsOwnEvents(t *testing.T) {
	backend := &scriptedBackend{rounds: [][]map[string]any{
		roundEvents("resp_1", imagegenCall("", `{"prompt":"a red cube"}`), assistantText("msg_0", "Drawing.")),
		roundEvents("resp_2", assistantText("msg_1", "Done.")),
	}}
	s := newImagegenServer(backend)
	recorder := postJSON(t, s, "/imagegen/v1/responses", map[string]any{"input": "draw a red cube", "tools": []any{map[string]any{"type": "image_generation"}}})
	var response map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)
	var types []string
	for _, raw := range sliceAny(response["output"]) {
		types = append(types, stringValue(mapAny(raw)["type"]))
	}
	if strings.Join(types, ",") != "message,image_generation_call,message" {
		t.Fatalf("status = %d output = %v", recorder.Code, types)
	}
}

func TestImageDataURLUsesTheRealFormat(t *testing.T) {
	items := []any{map[string]any{"type": "image_generation_call", "result": "QQ==", "output_format": "jpeg"}}
	if got := recentImages(items, 1); got[0] != "data:image/jpeg;base64,QQ==" {
		t.Fatalf("recent = %v", got)
	}
	if imageDataURL(nil, "QQ==") != "data:image/png;base64,QQ==" || imageDataURL("jpg", "QQ==") != "data:image/jpeg;base64,QQ==" {
		t.Fatal("imageDataURL defaults")
	}
}
