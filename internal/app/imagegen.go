package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// The /imagegen/v1 routes draw the way the Codex CLI's own image_gen tool does
// (codex-rs ext/image-generation, 0.166+): not with the hosted image_generation
// tool inside /codex/responses, but with Codex's standalone Images endpoint,
// /codex/images/generations and /codex/images/edits. Every other /imagegen/v1
// route behaves exactly like its /v1 twin, so a client switches paths by
// changing nothing but its base URL.
//
//   - POST /imagegen/v1/images/generations and /images/edits send the request
//     straight to that endpoint.
//   - POST /imagegen/v1/responses with an image_generation tool runs the CLI's
//     loop: the chat model gets an `imagegen` function instead of the hosted
//     tool; when it calls it, the proxy draws on the Images endpoint, hands the
//     picture back to the model as the function's output, and lets it finish.
//     The client still sees image_generation_call items, as on /v1.
const imagegenPrefix = "/imagegen"

const (
	// imagegenImageModel is what the CLI sends. The endpoint has been seen to
	// ignore it (and quality and size) and draw at its own settings; the response
	// reports which ones it used.
	imagegenImageModel = "gpt-image-2"
	// imagegenToolName is the function offered to the chat model. The CLI's own
	// name, image_gen.imagegen, is reserved by Codex for its exact schema.
	imagegenToolName = "imagegen"
	// imagegenMaxEditImages is the CLI's limit on the pictures one call edits.
	imagegenMaxEditImages = 5
	// imagegenMaxRounds bounds the model → draw → model loop of one response.
	imagegenMaxRounds = 8
)

// imagegenRoute strips the /imagegen prefix from a request path. It reports
// whether the request asked for the Images-endpoint drawing path.
func imagegenRoute(r *http.Request) bool {
	path := r.URL.Path
	if path != imagegenPrefix+"/v1" && !strings.HasPrefix(path, imagegenPrefix+"/v1/") {
		return false
	}
	r.URL.Path = strings.TrimPrefix(path, imagegenPrefix)
	if r.URL.RawPath != "" {
		r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, imagegenPrefix)
	}
	return true
}

// imagesCall posts one request to Codex's Images endpoint, the way the CLI's
// CodexImagesBackend does: JSON body, the usual Codex identity headers, and an
// x-codex-image-turn-id.
func (b *backend) imagesCall(ctx context.Context, edit bool, body map[string]any) (map[string]any, error) {
	path := "/images/generations"
	if edit {
		path = "/images/edits"
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, &backendError{400, "Encode image request failed."}
	}
	turnID := newUUID()
	started := time.Now()
	resp, lease, err := b.doAuthenticated(routeHints{}, func(lease *accountLease) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.BackendURL+path, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header = b.headers(lease.cred, false, "")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("x-codex-image-turn-id", turnID)
		return req, nil
	})
	if err != nil {
		var backendErr *backendError
		if errors.As(err, &backendErr) {
			return nil, err
		}
		return nil, &backendError{502, "Codex images request failed."}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("codex.imagegen.error path=%s status=%d account=%s seconds=%.1f", path, resp.StatusCode, lease.id(), time.Since(started).Seconds())
		return nil, decodeBackendError(resp)
	}
	var doc map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 128<<20)).Decode(&doc); err != nil {
		return nil, &backendError{502, "Codex images response was not valid JSON."}
	}
	if len(sliceAny(doc["data"])) == 0 {
		return nil, &backendError{502, "Codex images response carried no image."}
	}
	log.Printf(
		"codex.imagegen.done path=%s account=%s images=%d quality=%s size=%s background=%s seconds=%.1f request_id=%s",
		path, lease.id(), len(sliceAny(doc["data"])), stringValue(doc["quality"]), stringValue(doc["size"]),
		stringValue(doc["background"]), time.Since(started).Seconds(), resp.Header.Get("x-codex-imagegen-request-id"),
	)
	return doc, nil
}

// imagegenRequest builds the Images-endpoint body the CLI sends: its model,
// quality and size "auto" and an opaque background unless transparency is asked
// for. A quality or size the caller names is forwarded in their place.
func imagegenRequest(prompt string, settings map[string]any, images []string) map[string]any {
	request := map[string]any{
		"prompt": prompt, "model": imagegenImageModel,
		"background": "opaque", "quality": "auto", "size": "auto",
	}
	if stringValue(settings["background"]) == "transparent" {
		request["background"] = "transparent"
	}
	for _, key := range []string{"quality", "size"} {
		if value := stringValue(settings[key]); value != "" {
			request[key] = value
		}
	}
	if len(images) > 0 {
		refs := make([]any, 0, len(images))
		for _, image := range images {
			refs = append(refs, map[string]any{"image_url": image})
		}
		request["images"] = refs
	}
	return request
}

// imagegenImages serves /imagegen/v1/images/generations and /images/edits. A
// masked edit stays on the hosted tool: the Images endpoint takes no mask.
func (s *server) imagegenImages(w http.ResponseWriter, r *http.Request, edit bool) {
	body, err := decodeImageRequest(r)
	if err != nil {
		writeError(w, 400, "Invalid image request body.", "invalid_request_error", nil, nil)
		return
	}
	if err := validateImage(body, edit); err != nil {
		writeError(w, 400, err.Message, "invalid_request_error", strPtr(err.Param), nil)
		return
	}
	if len(dataURLList(body["mask"])) > 0 {
		log.Printf("codex.imagegen.mask_fallback path=hosted_tool")
		s.serveImages(w, r, body, edit)
		return
	}
	count := intValue(body["n"])
	if count == 0 {
		count = 1
	}
	stream := imageStreamRequested(body)
	if stream {
		if _, ok := w.(http.Flusher); !ok {
			writeError(w, 500, "Streaming unsupported.", "api_error", nil, nil)
			return
		}
	}
	if !s.acquireRequest(w, r) {
		return
	}
	defer s.release()
	request := imagegenRequest(strings.TrimSpace(stringValue(body["prompt"])), body, dataURLList(body["image"]))
	data := make([]any, 0, count)
	var last, usage map[string]any
	for i := 0; i < count; i++ {
		doc, err := s.backend.imagesCall(r.Context(), edit, request)
		if err != nil {
			if stream {
				setSSE(w)
				failure := publicErrorObject(err)
				failure["type"] = "error"
				writeSSE(w, failure)
				io.WriteString(w, "data: [DONE]\n\n")
				w.(http.Flusher).Flush()
				return
			}
			writeBackendError(w, err)
			return
		}
		last = doc
		usage = addUsage(usage, mapAny(doc["usage"]))
		for _, raw := range sliceAny(doc["data"]) {
			if b64 := stringValue(mapAny(raw)["b64_json"]); b64 != "" {
				data = append(data, map[string]any{"b64_json": b64})
			}
		}
	}
	created := intValue(last["created"])
	if created == 0 {
		created = int(time.Now().Unix())
	}
	metadata := map[string]any{"output_format": valueOr(last["output_format"], "png")}
	for _, key := range []string{"background", "quality", "size"} {
		if value := stringValue(last[key]); value != "" {
			metadata[key] = value
		}
	}
	if !stream {
		result := map[string]any{"created": created, "data": data, "usage": usage}
		for key, value := range metadata {
			result[key] = value
		}
		writeJSON(w, 200, result)
		return
	}
	prefix := "image_generation"
	if edit {
		prefix = "image_edit"
	}
	setSSE(w)
	event := imageStreamEvent(prefix+".completed", metadata, created, stringValue(mapAny(data[0])["b64_json"]), nil)
	if usage != nil {
		event["usage"] = usage
	}
	writeSSE(w, event)
	io.WriteString(w, "data: [DONE]\n\n")
	w.(http.Flusher).Flush()
}

// imagegenFunctionTool is the function the chat model calls to draw: the CLI's
// image_gen arguments, minus referenced_image_paths (the proxy has no files —
// pictures come from the conversation, as num_last_images_to_include).
func imagegenFunctionTool() map[string]any {
	return map[string]any{
		"type": "function", "name": imagegenToolName, "strict": false,
		"description": "Generate an image from a description, or edit images from this conversation. Use it when the user asks for a " +
			"picture of any kind (scene, character, icon, diagram, comic...) or wants an attached or previously generated image " +
			"changed (adding or removing elements, colors, style, quality).\n\nGuidelines:\n" +
			"- Write the full prompt the image model needs; it sees only the prompt and the images you include.\n" +
			"- Set `transparent_background` to true when the request calls for a transparent background, a cutout or background " +
			"removal; false otherwise. For edits, preserve existing transparency unless the user asks to change it.\n" +
			"- Omit `num_last_images_to_include` for a brand new image.\n" +
			"- For edits or images drawn from references, set `num_last_images_to_include` to the smallest number of the most " +
			"recent conversation images that includes every image to use, up to 5.\n" +
			"- Directly generate the image without reconfirmation or clarification.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{"type": "string"},
				"transparent_background": map[string]any{
					"type": "boolean", "default": false,
					"description": "Whether the output should have a transparent background. Defaults to false.",
				},
				"num_last_images_to_include": map[string]any{
					"type": []any{"integer", "null"}, "minimum": 1, "maximum": imagegenMaxEditImages,
				},
			},
			"required": []any{"prompt"}, "additionalProperties": false,
		},
	}
}

// imageGenerationTool returns the hosted image_generation tool a request
// offers, or nil.
func imageGenerationTool(payload map[string]any) map[string]any {
	for _, raw := range sliceAny(payload["tools"]) {
		if tool := mapAny(raw); tool != nil && tool["type"] == "image_generation" {
			return tool
		}
	}
	return nil
}

func isImagegenCall(item map[string]any) bool {
	return item != nil && item["type"] == "function_call" && item["name"] == imagegenToolName
}

// imagegenRun is one /imagegen/v1/responses request: its rounds against
// /codex/responses and the pictures drawn between them.
type imagegenRun struct {
	s        *server
	prepared map[string]any
	previous string
	settings map[string]any // the caller's image_generation tool
	base     []any          // the request's input
	context  []any          // what the rounds added after it, as upstream input
	outputs  []any          // what the client sees as the response's output
	emit     func(map[string]any)
	seq      int
	next     int // the client's next output_index
	usage    map[string]any
	first    map[string]any // the first round's response: its id stands for the whole run
	drawn    int
	limit    int
}

// imagegenResponse runs the CLI's drawing loop for a /responses request that
// offers the image_generation tool.
func (s *server) imagegenResponse(w http.ResponseWriter, r *http.Request, prepared, downstream map[string]any, previous string, stream bool) {
	run := &imagegenRun{
		s: s, prepared: prepared, previous: previous, settings: imageGenerationTool(downstream),
		base: sliceAny(downstream["input"]), limit: intValue(downstream["max_tool_calls"]),
	}
	if run.limit <= 0 {
		run.limit = imagegenMaxRounds
	}
	var flusher http.Flusher
	if stream {
		setSSE(w)
		var ok bool
		if flusher, ok = w.(http.Flusher); !ok {
			writeError(w, 500, "Streaming unsupported.", "api_error", nil, nil)
			return
		}
		run.emit = func(event map[string]any) {
			event["sequence_number"] = run.seq
			run.seq++
			writeSSE(w, event)
			flusher.Flush()
		}
	}
	if !s.acquireRequest(w, r) {
		return
	}
	defer s.release()
	response, err := run.do(r.Context(), downstream)
	if !stream {
		if err != nil {
			writeBackendError(w, err)
			return
		}
		writeJSON(w, 200, response)
		return
	}
	if err != nil {
		if run.seq == 0 {
			clearSSE(w)
			writeBackendError(w, err)
			return
		}
		failure := publicErrorObject(err)
		run.emit(map[string]any{"type": "error", "code": failure["code"], "message": failure["message"], "param": failure["param"]})
	}
	io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (run *imagegenRun) forward(event map[string]any) {
	if run.emit != nil {
		run.emit(event)
	}
}

func (run *imagegenRun) do(ctx context.Context, downstream map[string]any) (map[string]any, error) {
	tools := make([]any, 0, len(sliceAny(downstream["tools"])))
	for _, raw := range sliceAny(downstream["tools"]) {
		if tool := mapAny(raw); tool != nil && tool["type"] == "image_generation" {
			tools = append(tools, imagegenFunctionTool())
			continue
		}
		tools = append(tools, raw)
	}
	choice := downstream["tool_choice"]
	if mapAny(choice)["type"] == "image_generation" {
		choice = map[string]any{"type": "function", "name": imagegenToolName}
	}
	for round := 0; ; round++ {
		payload := cloneMap(downstream)
		// Codex refuses max_tool_calls; the run keeps that limit itself.
		delete(payload, "max_tool_calls")
		payload["tools"] = tools
		payload["tool_choice"] = choice
		payload["input"] = append(cloneSlice(run.base), run.context...)
		final, calls, err := run.round(ctx, payload, round)
		if err != nil {
			return nil, err
		}
		if len(calls) == 0 || round+1 >= imagegenMaxRounds {
			return run.finish(final), nil
		}
		for _, call := range calls {
			run.draw(ctx, call)
		}
		// A forced call is answered; the model now finishes in its own words.
		choice = "auto"
		if run.drawn >= run.limit {
			choice = "none"
		}
	}
}

// round streams one /codex/responses call. Ordinary output reaches the client
// as it arrives, renumbered into the run's output; the model's imagegen calls
// are held back and returned.
func (run *imagegenRun) round(ctx context.Context, payload map[string]any, round int) (map[string]any, []map[string]any, error) {
	var final map[string]any
	var calls []map[string]any
	held := map[string]bool{}
	indexes := map[int]int{}
	err := run.s.backend.stream(ctx, payload, func(event map[string]any) error {
		eventType := stringValue(event["type"])
		switch eventType {
		case "response.created", "response.in_progress", "response.queued":
			if response := mapAny(event["response"]); response != nil && run.first == nil {
				run.first = cloneMap(response)
			}
			if round == 0 {
				if response := mapAny(event["response"]); response != nil {
					response["tools"] = sliceAny(run.prepared["tools"])
				}
				run.forward(event)
			}
			return nil
		case "response.completed", "response.incomplete":
			final = cloneMap(mapAny(event["response"]))
			return nil
		case "response.failed":
			return responseFailure(failureDetail(mapAny(event["response"])["error"]))
		case "error":
			return responseFailure(errorEventDetail(event))
		}
		item := mapAny(event["item"])
		if isImagegenCall(item) {
			held[stringValue(item["id"])] = true
			if eventType == "response.output_item.done" {
				calls = append(calls, cloneMap(item))
				run.context = append(run.context, normalizeInputItem(item))
			}
			return nil
		}
		if held[stringValue(event["item_id"])] {
			return nil
		}
		if eventType == "response.output_item.done" && item != nil {
			if normalized := normalizeInputItem(item); normalized != nil {
				run.context = append(run.context, normalized)
			}
			run.outputs = append(run.outputs, item)
		}
		if raw, ok := event["output_index"]; ok && raw != nil {
			upstream := intValue(raw)
			index, seen := indexes[upstream]
			if !seen {
				index = run.next
				run.next++
				indexes[upstream] = index
			}
			event["output_index"] = index
		}
		run.forward(event)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if final == nil {
		return nil, nil, &backendError{502, "Codex stream closed before the response completed."}
	}
	run.usage = addUsage(run.usage, mapAny(final["usage"]))
	return final, calls, nil
}

// draw answers one imagegen call: the picture goes to the client as an
// image_generation_call item, and back to the model as the call's output.
func (run *imagegenRun) draw(ctx context.Context, call map[string]any) {
	callID := stringValue(valueOr(call["call_id"], call["id"]))
	answer := func(output any) {
		run.context = append(run.context, map[string]any{"type": "function_call_output", "call_id": callID, "output": output})
	}
	var args struct {
		Prompt                 string `json:"prompt"`
		TransparentBackground  bool   `json:"transparent_background"`
		NumLastImagesToInclude *int   `json:"num_last_images_to_include"`
	}
	if err := json.Unmarshal([]byte(stringValue(call["arguments"])), &args); err != nil || strings.TrimSpace(args.Prompt) == "" {
		answer("Not run: imagegen needs a JSON object with a non-empty `prompt`.")
		return
	}
	if run.drawn >= run.limit {
		answer(fmt.Sprintf("Not run: this response may draw at most %d image(s).", run.limit))
		return
	}
	var images []string
	if n := args.NumLastImagesToInclude; n != nil {
		if *n < 1 || *n > imagegenMaxEditImages {
			answer(fmt.Sprintf("Not run: `num_last_images_to_include` must be between 1 and %d.", imagegenMaxEditImages))
			return
		}
		images = recentImages(append(cloneSlice(run.base), run.context...), *n)
		if len(images) != *n {
			answer(fmt.Sprintf("Not run: asked for the last %d conversation images, but only %d are available.", *n, len(images)))
			return
		}
	}
	run.drawn++
	// As in the CLI, the model decides transparency per call.
	settings := cloneMap(run.settings)
	settings["background"] = "opaque"
	if args.TransparentBackground {
		settings["background"] = "transparent"
	}
	item := map[string]any{"id": newID("ig"), "type": "image_generation_call", "status": "in_progress"}
	index := run.next
	run.next++
	run.forward(map[string]any{"type": "response.output_item.added", "output_index": index, "item": cloneMap(item)})
	doc, err := run.s.backend.imagesCall(ctx, len(images) > 0, imagegenRequest(args.Prompt, settings, images))
	if err != nil {
		message := publicErrorObject(err)["message"]
		item["status"] = "failed"
		item["error"] = map[string]any{"message": message}
		answer(fmt.Sprintf("image generation failed: %v", message))
	} else {
		b64 := stringValue(mapAny(sliceAny(doc["data"])[0])["b64_json"])
		item["status"] = "completed"
		item["result"] = b64
		item["revised_prompt"] = args.Prompt
		item["output_format"] = valueOr(doc["output_format"], "png")
		for _, key := range []string{"background", "quality", "size"} {
			if value := stringValue(doc[key]); value != "" {
				item[key] = value
			}
		}
		answer([]any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + b64, "detail": "high"}})
	}
	run.outputs = append(run.outputs, item)
	run.forward(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
}

// finish shapes the run as one response: the first round's id, every round's
// visible output and summed usage, and the caller's own tools.
func (run *imagegenRun) finish(final map[string]any) map[string]any {
	response := cloneMap(final)
	if run.first != nil {
		for _, key := range []string{"id", "created_at"} {
			if run.first[key] != nil {
				response[key] = run.first[key]
			}
		}
	}
	response["output"] = run.outputs
	response["usage"] = run.usage
	response["tools"] = sliceAny(run.prepared["tools"])
	response["tool_choice"] = valueOr(run.prepared["tool_choice"], "auto")
	response = ensureResponse(response, run.prepared)
	if run.previous != "" {
		response["previous_response_id"] = run.previous
	}
	id := stringValue(response["id"])
	run.s.responses.remember(id, sliceAny(run.prepared["input"]), response)
	// A follow-up continues from what the model really saw — its imagegen calls
	// and their outputs — rather than from the image items the client was shown.
	run.s.responses.setContext(id, append(cloneSlice(sliceAny(run.prepared["input"])), run.context...))
	eventType := "response.completed"
	if stringValue(response["status"]) == "incomplete" {
		eventType = "response.incomplete"
	}
	run.forward(map[string]any{"type": eventType, "response": response})
	return response
}

func (s *responseStore) setContext(id string, context []any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stored, ok := s.values[id]; ok {
		stored.Context = context
	}
}

// recentImages returns the last n pictures in a conversation, oldest first:
// input images in messages, images in function outputs, and drawn
// image_generation_call results — what the CLI's num_last_images_to_include
// counts.
func recentImages(items []any, n int) []string {
	var found []string
	for i := len(items) - 1; i >= 0 && len(found) < n; i-- {
		item := mapAny(items[i])
		if item == nil {
			continue
		}
		var parts []any
		switch {
		case item["type"] == "image_generation_call":
			if result := stringValue(item["result"]); result != "" {
				found = append(found, "data:image/png;base64,"+result)
			}
			continue
		case item["type"] == "function_call_output":
			parts = sliceAny(item["output"])
		default:
			parts = sliceAny(item["content"])
		}
		for j := len(parts) - 1; j >= 0 && len(found) < n; j-- {
			part := mapAny(parts[j])
			if part == nil || part["type"] != "input_image" {
				continue
			}
			url, _ := part["image_url"].(string)
			if object := mapAny(part["image_url"]); object != nil {
				url, _ = object["url"].(string)
			}
			if url != "" {
				found = append(found, url)
			}
		}
	}
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	return found
}

// addUsage sums two usage objects field by field, nested details included.
func addUsage(total, more map[string]any) map[string]any {
	if more == nil {
		return total
	}
	if total == nil {
		return cloneMap(more)
	}
	result := cloneMap(total)
	for key, value := range more {
		switch typed := value.(type) {
		case map[string]any:
			result[key] = addUsage(mapAny(result[key]), typed)
		case float64, int, json.Number:
			result[key] = intValue(result[key]) + intValue(typed)
		default:
			if result[key] == nil {
				result[key] = value
			}
		}
	}
	return result
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
