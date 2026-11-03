package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var defaultModels = []string{"gpt-5.1", "gpt-5.1-codex-max", "gpt-5.1-codex-mini", "gpt-5.2", "gpt-5.2-codex", "gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}

const maxSSEEventBytes = 128 << 20
const replayBodyMemoryLimit = 1 << 20

type backend struct {
	cfg          config
	client       *http.Client
	streamClient *http.Client
	pool         *accountPool
}
type backendError struct {
	Status  int
	Message string
}

func (e *backendError) Error() string { return e.Message }

const (
	retryBaseDelay = 200 * time.Millisecond
	// A longer server-advised wait is left to the client through Retry-After.
	maxRetryAdvice = 30 * time.Second
)

var (
	errSSELimit    = errors.New("Codex SSE size limit exceeded")
	errStreamIdle  = errors.New("Codex stream idle timeout")
	errRetryStream = errors.New("retry Codex stream")
)

// responseFailureError is a failure Codex reported through a response status or
// an event in a 2xx stream.
type responseFailureError struct {
	backendError
	Type, Code, RetryAfter string
	Param                  any
}

func (e *responseFailureError) Unwrap() error { return &e.backendError }

// responseFailure builds and logs the client error for a failure detail. The code
// is logged because a stream may have already reported HTTP 200.
func responseFailure(detail map[string]any) error {
	err := newResponseFailure(detail)
	var failure *responseFailureError
	if errors.As(err, &failure) {
		log.Printf("codex.response.failed code=%s status=%d", failure.Code, failure.Status)
	}
	return err
}

// newResponseFailure builds the client error for a failure detail from Codex.
// Text fields are redacted.
func newResponseFailure(detail map[string]any) error {
	code := redactSensitive(stringValue(detail["code"]))
	upstreamType := redactSensitive(stringValue(detail["type"]))
	if code == "" && responseFailureStatus(upstreamType) == http.StatusTooManyRequests {
		// Usage errors name themselves in type rather than code.
		code = upstreamType
	}
	message := failureFallbackMessages[code]
	if message == "" {
		message = "Codex backend response failed."
	}
	if text := strings.TrimSpace(stringValue(detail["message"])); text != "" {
		message = redactSensitive(text)
	}
	status := responseFailureStatus(code)
	if explicit := intValue(detail["status"]); explicit >= 400 && explicit < 600 {
		status = explicit
	}
	errorType := upstreamType
	if errorType == "" {
		switch status {
		case http.StatusBadRequest, http.StatusNotFound:
			errorType = "invalid_request_error"
		case http.StatusUnauthorized:
			errorType = "authentication_error"
		case http.StatusTooManyRequests:
			errorType = "rate_limit_error"
		default:
			errorType = "api_error"
		}
	}
	var param any
	if text := stringValue(detail["param"]); text != "" {
		param = redactSensitive(text)
	}
	return &responseFailureError{backendError{status, message}, errorType, code, retryAfterAdvice(detail), param}
}

var failureFallbackMessages = map[string]string{
	"invalid_prompt":                "Invalid request.",
	"cyber_policy":                  "This request has been flagged for possible cybersecurity risk.",
	"bio_policy":                    "This content was flagged for possible biological risk.",
	"misalignment_policy_violation": "This request was blocked due to a misalignment policy violation.",
}

func responseFailureStatus(code string) int {
	switch code {
	case "invalid_api_key":
		return http.StatusUnauthorized
	case "model_not_found":
		return http.StatusNotFound
	case "rate_limit_exceeded", "insufficient_quota", "usage_limit_reached", "usage_not_included",
		"credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded",
		"organization_usage_limit_exceeded", "flex_unavailable":
		return http.StatusTooManyRequests
	case "server_is_overloaded", "slow_down", "policy_capacity_unavailable":
		return http.StatusServiceUnavailable
	case "context_length_exceeded", "unsupported_parameter", "content_filter",
		"cyber_policy", "bio_policy", "misalignment_policy_violation",
		"audio_too_large", "audio_too_short", "missing_audio_sample_rate":
		return http.StatusBadRequest
	}
	if strings.HasPrefix(code, "invalid_") {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// retryAfterAdvice returns a valid Retry-After value from a failure's headers.
func retryAfterAdvice(detail map[string]any) string {
	for key, value := range mapAny(detail["headers"]) {
		if strings.EqualFold(key, "retry-after") {
			advice := strings.TrimSpace(stringValue(value))
			if _, ok := parseRetryAfter(advice); ok {
				return advice
			}
		}
	}
	return ""
}

func parseRetryAfter(value string) (time.Duration, bool) {
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		return time.Duration(seconds * float64(time.Second)), seconds >= 0 && seconds <= math.MaxInt32
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0), true
	}
	return 0, false
}

// failureDetail reads an upstream error value, which is an object or a message.
func failureDetail(value any) map[string]any {
	if detail := mapAny(value); detail != nil {
		return detail
	}
	if text := stringValue(value); text != "" {
		return map[string]any{"message": text}
	}
	return nil
}

// errorEventDetail reads an error event, which holds its fields either directly
// or under "error", and may give an HTTP status beside them.
func errorEventDetail(event map[string]any) map[string]any {
	detail := cloneMap(failureDetail(event["error"]))
	if detail == nil {
		detail = cloneMap(event)
		delete(detail, "type")
	}
	if detail["status"] == nil {
		detail["status"] = event["status"]
	}
	return detail
}

// failedResponseError returns the client error for a response that failed.
func failedResponseError(response map[string]any) error {
	if stringValue(response["status"]) != "failed" {
		return nil
	}
	return responseFailure(failureDetail(response["error"]))
}

func newBackend(cfg config) *backend {
	client := &http.Client{Timeout: cfg.Timeout}
	return &backend{
		cfg:    cfg,
		client: client,
		// stream applies cfg.Timeout between events, so a long response can finish.
		streamClient: &http.Client{},
		pool:         newAccountPool(cfg.AuthJSON, &http.Client{Timeout: authRefreshTimeout}),
	}
}

func (b *backend) headers(cred credentials, stream bool, requestID string) http.Header {
	return b.headersWithTier(cred, stream, requestID, "", false)
}

// headersWithTier builds the upstream request headers. serviceTier and fastMode travel as
// X-Service-Tier / X-Fast-Mode rather than only in the body: the payload fields alone did not
// take effect against the Codex backend, so the headers carry them as well. Established for
// service_tier upstream and for fast_mode in the fork this deployment came from, which moved
// fast_mode from the body to a header (and popped it from the body) for exactly this reason.
func (b *backend) headersWithTier(cred credentials, stream bool, requestID, serviceTier string, fastMode bool) http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+cred.AccessToken)
	originator := b.cfg.Originator
	if originator == "" {
		originator = defaultOriginator
	}
	h.Set("originator", originator)
	userAgent := b.cfg.UserAgent
	if userAgent == "" {
		userAgent = fmt.Sprintf("openai-api-server-via-codex/%s (%s; %s)", b.cfg.ClientVersion, runtime.GOOS, runtime.GOARCH)
	}
	h.Set("User-Agent", userAgent)
	if serviceTier != "" {
		h.Set("X-Service-Tier", serviceTier)
	}
	if fastMode {
		h.Set("X-Fast-Mode", "true")
	}
	if cred.AccountID != "" {
		h.Set("ChatGPT-Account-ID", cred.AccountID)
	}
	if stream {
		h.Set("Accept", "text/event-stream")
		h.Set("Content-Type", "application/json")
		h.Set("OpenAI-Beta", "responses=experimental")
	}
	if requestID != "" {
		h.Set("session_id", requestID)
		h.Set("x-client-request-id", requestID)
	}
	return h
}

// doAuthenticated sends one upstream request on an account from the pool. An
// answer that condemns the account rather than the request — 429, or a 401 that
// survives a credential reload — cools that account down and the request moves
// to the next one. Only when no account is left is that refusal returned. The
// returned response's Body releases the account when closed.
func (b *backend) doAuthenticated(hints routeHints, makeRequest func(*accountLease) (*http.Request, error)) (*http.Response, *accountLease, error) {
	return b.doAuthenticatedWith(b.client, hints, makeRequest)
}

// doAuthenticatedWith is doAuthenticated on a chosen client: streams use one
// without a whole-request timeout, because their timeout runs between events.
func (b *backend) doAuthenticatedWith(client *http.Client, hints routeHints, makeRequest func(*accountLease) (*http.Request, error)) (*http.Response, *accountLease, error) {
	exclude := map[string]bool{}
	var refused *http.Response
	var refusedLease *accountLease
	var credentialErr error
	for {
		lease, err := b.pool.acquire(hints, exclude)
		if err != nil {
			if refused != nil {
				return refused, refusedLease, nil
			}
			if credentialErr != nil {
				return nil, nil, &backendError{401, credentialErr.Error()}
			}
			return nil, nil, poolError("request", err)
		}
		resp, err := b.sendOnAccount(client, lease, makeRequest)
		if err != nil {
			lease.release()
			var credErr *accountCredentialError
			if errors.As(err, &credErr) {
				b.pool.coolDown(lease.acct, unauthorizedCooldown, "unauthorized", redactSensitive(credErr.err.Error()))
				exclude[lease.id()] = true
				credentialErr = credErr.err
				continue
			}
			return nil, nil, err
		}
		b.pool.observe(lease.acct, resp)
		if resp.StatusCode == http.StatusBadRequest && len(hints.reasoningKeys) > 0 && !lease.pinMissed {
			// Encrypted reasoning only decrypts on the account that made it. The
			// pool normally routes it home; after a restart it cannot know where
			// home is, so the reasoning is dropped and the request sent again.
			if body := bufferedBody(resp); encryptedContentRejected(body) {
				log.Printf("codex.account.reasoning_rejected account=%s action=resend_without_reasoning", lease.id())
				lease.pinMissed = true
				resp, err = b.sendOnAccount(client, lease, makeRequest)
				if err != nil {
					lease.release()
					return nil, nil, err
				}
				b.pool.observe(lease.acct, resp)
			}
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
			body := bufferedBody(resp)
			if d, reason, ok := cooldownForStatus(resp.StatusCode, resp.Header, body, time.Now()); ok {
				b.pool.coolDown(lease.acct, d, reason, fmt.Sprintf("Codex answered HTTP %d", resp.StatusCode))
				log.Printf("codex.account.failover account=%s status=%d", lease.id(), resp.StatusCode)
				exclude[lease.id()] = true
				lease.release()
				refused, refusedLease = resp, lease
				continue
			}
		}
		resp.Body = &leaseBody{ReadCloser: resp.Body, lease: lease}
		return resp, lease, nil
	}
}

// sendOnAccount keeps the single-account rule: a 401 reloads the credentials
// (someone may have rewritten auth.json) and retries once.
func (b *backend) sendOnAccount(client *http.Client, lease *accountLease, makeRequest func(*accountLease) (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := makeRequest(lease)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}
		if attempt == 1 {
			log.Printf("codex.auth.unauthorized code=upstream_unauthorized stage=upstream attempt=2 action=return_401 account=%s", lease.id())
			return resp, nil
		}
		log.Printf("codex.auth.unauthorized code=upstream_unauthorized stage=upstream attempt=1 action=reload_and_retry account=%s", lease.id())
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		previousAccessToken := lease.cred.AccessToken
		cred, err := lease.reload()
		if err != nil {
			logAuthFailure("reload_after_401", err)
			return nil, &accountCredentialError{err}
		}
		b.debugf("codex.auth.reloaded_after_unauthorized credentials_changed=%t", previousAccessToken != cred.AccessToken)
	}
	panic("unreachable")
}

// accountCredentialError is an account whose credentials stopped loading
// mid-request; the request can still move to another account.
type accountCredentialError struct{ err error }

func (e *accountCredentialError) Error() string { return e.err.Error() }

type leaseBody struct {
	io.ReadCloser
	lease *accountLease
}

func (l *leaseBody) Close() error {
	err := l.ReadCloser.Close()
	l.lease.release()
	return err
}

// poolError turns "no account could be tried" into the client-facing error.
func poolError(stage string, err error) error {
	var unavailable *poolUnavailable
	if errors.As(err, &unavailable) {
		log.Printf("codex.account.unavailable stage=%s message=%q", stage, unavailable.message)
		return &backendError{unavailable.status, unavailable.message}
	}
	logAuthFailure(stage, err)
	return &backendError{401, err.Error()}
}

func encryptedContentRejected(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "encrypted_content") || strings.Contains(text, "encrypted content")
}

// withoutEncryptedReasoning drops replayed reasoning the serving account could
// not decrypt. The model loses that reasoning, not the conversation.
func withoutEncryptedReasoning(payload map[string]any) map[string]any {
	result := cloneMap(payload)
	input, ok := result["input"].([]any)
	if !ok {
		return result
	}
	kept := make([]any, 0, len(input))
	for _, raw := range input {
		if item := mapAny(raw); item != nil && item["type"] == "reasoning" && stringValue(item["encrypted_content"]) != "" {
			continue
		}
		kept = append(kept, raw)
	}
	result["input"] = kept
	return result
}

func logAuthFailure(stage string, err error) {
	log.Printf(
		"codex.auth.error stage=%s code=%s message=%q",
		stage,
		authFailureCode(err),
		redactSensitive(err.Error()),
	)
}

func prepareCodexPayload(payload map[string]any, dropParams []string) map[string]any {
	return normalizeCodexPayload(cloneMap(payload), dropParams)
}

func normalizeCodexPayload(prepared map[string]any, dropParams []string) map[string]any {
	for _, name := range dropParams {
		delete(prepared, name)
	}
	delete(prepared, "max_output_tokens")
	for _, item := range sliceAny(prepared["input"]) {
		for _, part := range sliceAny(mapAny(item)["content"]) {
			delete(mapAny(part), "prompt_cache_breakpoint")
		}
	}
	prepared["stream"], prepared["store"] = true, false
	setDefault(prepared, "tool_choice", "auto")
	setDefault(prepared, "parallel_tool_calls", true)
	text, _ := prepared["text"].(map[string]any)
	if text == nil {
		text = map[string]any{}
	}
	setDefault(text, "verbosity", "low")
	prepared["text"] = text
	include := sliceAny(prepared["include"])
	found := false
	for _, v := range include {
		if v == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found {
		include = append(include, "reasoning.encrypted_content")
	}
	prepared["include"] = include
	return prepared
}

// stream delivers Codex events to fn, retrying transient failures while no output
// has reached fn. Lifecycle events wait until the attempt produces output, so a
// client never sees events from an abandoned attempt.
func (b *backend) stream(ctx context.Context, payload map[string]any, fn func(map[string]any) error) error {
	return b.streamWithRetries(ctx, payload, false, fn)
}

// streamWithRetries holds every event until the terminal one when holdOutput is
// set, for a caller that shows the client nothing until the response is complete.
func (b *backend) streamWithRetries(ctx context.Context, payload map[string]any, holdOutput bool, fn func(map[string]any) error) error {
	for attempt := 0; ; attempt++ {
		var held []map[string]any
		var retryable, pending error
		// The final attempt has nothing to hide, so it streams without delay.
		committed := attempt >= b.cfg.MaxRetries
		commit := func() error {
			committed = true
			for _, event := range held {
				if err := fn(event); err != nil {
					return err
				}
			}
			return nil
		}
		err := b.streamOnce(ctx, payload, func(event map[string]any) error {
			if committed {
				return fn(event)
			}
			typ := stringValue(event["type"])
			switch {
			case typ == "error":
				// A response.failed that follows has the authoritative code.
				pending = eventFailure(event)
				held = append(held, event)
				return nil
			case typ == "response.failed":
				failure := eventFailure(event)
				if _, ok := b.retryDelay(failure, attempt); ok {
					retryable = failure
					return errRetryStream
				}
			case typ == "response.completed" || typ == "response.incomplete":
			case holdOutput || isLifecycleEvent(typ):
				held = append(held, event)
				return nil
			}
			if err := commit(); err != nil {
				return err
			}
			return fn(event)
		})
		if committed {
			return err
		}
		failure := err
		switch {
		case errors.Is(err, errRetryStream):
			failure = retryable
		case err == nil:
			failure = pending
		}
		delay, ok := b.retryDelay(failure, attempt)
		if !ok || ctx.Err() != nil {
			if commitErr := commit(); commitErr != nil {
				return commitErr
			}
			return failure
		}
		log.Printf("codex.stream.retry attempt=%d max_retries=%d delay_ms=%d code=%s", attempt+1, b.cfg.MaxRetries, delay.Milliseconds(), failureCode(failure))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func isLifecycleEvent(eventType string) bool {
	switch eventType {
	case "response.created", "response.queued", "response.in_progress", "response.metadata", "codex.response.metadata":
		return true
	}
	return false
}

func failureCode(err error) string {
	var failure *responseFailureError
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func eventFailure(event map[string]any) error {
	if stringValue(event["type"]) == "error" {
		return newResponseFailure(errorEventDetail(event))
	}
	return newResponseFailure(failureDetail(mapAny(event["response"])["error"]))
}

// retryDelay returns the wait before retrying after err, if err is transient and
// retries remain. Only a backend-reported failure can be transient. A stalled
// backend is not retried, because each attempt would wait out the whole timeout.
func (b *backend) retryDelay(err error, attempt int) (time.Duration, bool) {
	var failure *backendError
	if attempt >= b.cfg.MaxRetries || !errors.As(err, &failure) {
		return 0, false
	}
	var code, advice string
	var reported *responseFailureError
	if errors.As(err, &reported) {
		code, advice = reported.Code, reported.RetryAfter
	}
	var retry bool
	switch {
	case failure.Status == http.StatusTooManyRequests:
		retry = code == "rate_limit_exceeded" || (code == "" && advice != "")
	case code == "server_is_overloaded" || code == "policy_capacity_unavailable":
		retry = advice != ""
	default:
		retry = failure.Status >= 500 && failure.Status != http.StatusGatewayTimeout
	}
	if !retry {
		return 0, false
	}
	if advice != "" {
		delay, _ := parseRetryAfter(advice)
		return delay, delay <= maxRetryAdvice
	}
	delay := min(b.cfg.retryBaseDelay<<min(attempt, 8), maxRetryAdvice)
	return time.Duration(float64(delay) * (0.9 + rand.Float64()*0.2)), true
}

func (b *backend) streamOnce(ctx context.Context, payload map[string]any, fn func(map[string]any) error) error {
	b.debugf("codex.stream.start model=%s endpoint=%s/responses", stringValue(payload["model"]), b.cfg.BackendURL)
	prepared := prepareCodexPayload(payload, b.cfg.DropParams)
	// Same rule as the tier: read from prepared so drop_params still governs it. The body copy is
	// then removed — Codex acts on the header, and leaving an unknown field in the payload risks a
	// 400 from a backend that validates strictly.
	fastMode := boolValue(prepared["fast_mode"])
	delete(prepared, "fast_mode")
	body, _ := json.Marshal(prepared)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := time.AfterFunc(b.cfg.Timeout, func() { cancel(errStreamIdle) })
	defer idle.Stop()
	idleError := func() error {
		if errors.Is(context.Cause(ctx), errStreamIdle) {
			return &backendError{http.StatusGatewayTimeout, "Codex backend stream timed out."}
		}
		return nil
	}
	var stripped []byte
	resp, lease, err := b.doAuthenticatedWith(b.streamClient, hintsForPayload(prepared), func(lease *accountLease) (*http.Request, error) {
		requestBody := body
		if lease.pinMissed {
			if stripped == nil {
				stripped, _ = json.Marshal(withoutEncryptedReasoning(prepared))
			}
			requestBody = stripped
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.BackendURL+"/responses", bytes.NewReader(requestBody))
		if err != nil {
			return nil, err
		}
		// Read the tier from prepared, not payload, so --drop-params service_tier
		// removes it from the header too rather than only from the body.
		req.Header = b.headersWithTier(lease.cred, true, stringValue(prepared["prompt_cache_key"]), stringValue(prepared["service_tier"]), fastMode)
		return req, nil
	})
	if err != nil {
		var backendErr *backendError
		if errors.As(err, &backendErr) {
			b.debugf("codex.stream.auth_error message=%s", redactSensitive(err.Error()))
			return err
		}
		if err := idleError(); err != nil {
			return err
		}
		return &backendError{502, "Codex backend request failed."}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return decodeBackendError(resp)
	}
	account := lease.id()
	b.debugf("codex.stream.account account=%s", account)
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var data strings.Builder
	events := 0
	terminal, finished := false, false
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		raw := data.String()
		data.Reset()
		if raw == "[DONE]" {
			return nil
		}
		var event map[string]any
		if json.Unmarshal([]byte(raw), &event) != nil {
			return nil
		}
		normalizeBackendEvent(event)
		events++
		switch event["type"] {
		case "error":
			terminal = true
		case "response.failed":
			terminal, finished = true, true
		case "response.completed", "response.incomplete":
			if mapAny(event["response"]) != nil {
				terminal, finished = true, true
			}
		case "response.output_item.done":
			b.pool.rememberOutput(account, mapAny(event["item"]))
		case "codex.rate_limits":
			b.pool.observeRateLimitEvent(account, event)
		}
		// A slow client must not count as an idle backend.
		idle.Stop()
		defer idle.Reset(b.cfg.Timeout)
		return fn(event)
	}
	for {
		line, readErr := readSSELine(reader, maxSSEEventBytes)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			b.debugf("codex.stream.error message=%s", redactSensitive(readErr.Error()))
			if terminal {
				return nil
			}
			if err := idleError(); err != nil {
				return err
			}
			if errors.Is(readErr, errSSELimit) {
				return readErr
			}
			return &backendError{502, "Codex backend stream failed."}
		}
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			if finished || errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			part := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data.Len()+len(part)+1 > maxSSEEventBytes {
				return fmt.Errorf("%w: event exceeds %d bytes", errSSELimit, maxSSEEventBytes)
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(part)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if err := flush(); err != nil {
		return err
	}
	b.debugf("codex.stream.end events=%d", events)
	if !terminal {
		return &backendError{502, "Codex stream closed before the response completed."}
	}
	return nil
}

func readSSELine(reader *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return "", fmt.Errorf("%w: line exceeds %d bytes", errSSELimit, limit)
		}
		line = append(line, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		return string(line), err
	}
}

func normalizeBackendEvent(event map[string]any) {
	if event["type"] == "response.done" {
		event["type"] = "response.completed"
	}
	response := mapAny(event["response"])
	if response == nil {
		return
	}
	status := stringValue(response["status"])
	if status != "" && !validResponseStatus(status) {
		delete(response, "status")
	}
}

func validResponseStatus(status string) bool {
	switch status {
	case "completed", "failed", "in_progress", "cancelled", "queued", "incomplete":
		return true
	default:
		return false
	}
}

func (b *backend) collect(ctx context.Context, payload map[string]any) (map[string]any, error) {
	var completed map[string]any
	var output []any
	var failure map[string]any
	err := b.streamWithRetries(ctx, payload, true, func(event map[string]any) error {
		switch event["type"] {
		case "response.output_item.done":
			if item := mapAny(event["item"]); item != nil {
				output = append(output, item)
			}
		case "error":
			failure = errorEventDetail(event)
		case "response.completed", "response.incomplete":
			completed = cloneMap(mapAny(event["response"]))
		case "response.failed":
			completed = cloneMap(mapAny(event["response"]))
			if completed == nil {
				return responseFailure(failure)
			}
			completed["status"] = "failed"
			if completed["error"] == nil && failure != nil {
				completed["error"] = failure
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if completed != nil {
		if len(sliceAny(completed["output"])) == 0 && len(output) > 0 {
			completed["output"] = output
		}
		return completed, nil
	}
	return nil, responseFailure(failure)
}

func (b *backend) listModels(ctx context.Context) []string {
	resp, _, err := b.doAuthenticated(routeHints{}, func(lease *accountLease) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.BackendURL+"/models?client_version="+url.QueryEscape(b.cfg.ClientVersion), nil)
		if err != nil {
			return nil, err
		}
		req.Header = b.headers(lease.cred, false, "")
		return req, nil
	})
	if err != nil {
		b.debugf("codex.models.fallback reason=request_or_auth_error")
		return append([]string(nil), defaultModels...)
	}
	defer resp.Body.Close()
	var doc map[string]any
	if resp.StatusCode/100 != 2 || json.NewDecoder(resp.Body).Decode(&doc) != nil {
		b.debugf("codex.models.fallback reason=invalid_response status=%d", resp.StatusCode)
		return append([]string(nil), defaultModels...)
	}
	var result []string
	for _, raw := range sliceAny(doc["models"]) {
		model := mapAny(raw)
		if boolValue(model["supported_in_api"]) && model["visibility"] == "list" && stringValue(model["slug"]) != "" {
			result = append(result, stringValue(model["slug"]))
		}
	}
	if len(result) == 0 {
		b.debugf("codex.models.fallback reason=empty_model_list")
		return append([]string(nil), defaultModels...)
	}
	b.debugf("codex.models.loaded count=%d", len(result))
	return result
}

func (b *backend) proxy(ctx context.Context, method, path, query string, headers http.Header, body io.Reader) (*http.Response, error) {
	return b.proxyTo(ctx, method, b.cfg.BackendURL, path, query, headers, body)
}

func (b *backend) proxyTo(ctx context.Context, method, baseURL, path, query string, headers http.Header, body io.Reader) (*http.Response, error) {
	target, err := resolveProxyURL(baseURL, path, query)
	if err != nil {
		return nil, &backendError{400, "Invalid proxy path."}
	}
	b.debugf("codex.proxy.start method=%s path=%s", method, redactSensitive(target.EscapedPath()))
	replay, err := newReplayBody(body)
	if err != nil {
		return nil, &backendError{400, "Read proxy request body failed."}
	}
	defer replay.Close()
	resp, _, err := b.doAuthenticated(routeHints{}, func(lease *accountLease) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, method, target.String(), replay.Reader())
		if err != nil {
			return nil, err
		}
		req.Header = b.headers(lease.cred, false, "")
		for _, name := range []string{"Accept", "Content-Type", "Idempotency-Key", "OpenAI-Beta", "OpenAI-Organization", "OpenAI-Project", "OpenAI-Version"} {
			if v := headers.Get(name); v != "" {
				req.Header.Set(name, v)
			}
		}
		return req, nil
	})
	if err != nil {
		var backendErr *backendError
		if errors.As(err, &backendErr) {
			return nil, err
		}
		return nil, &backendError{502, "Codex backend proxy request failed."}
	}
	b.debugf("codex.proxy.headers status=%d", resp.StatusCode)
	return resp, nil
}

type replayBody struct {
	data []byte
	file *os.File
	size int64
}

func newReplayBody(body io.Reader) (*replayBody, error) {
	if body == nil {
		return &replayBody{}, nil
	}
	var memory bytes.Buffer
	if _, err := io.Copy(&memory, io.LimitReader(body, replayBodyMemoryLimit+1)); err != nil {
		return nil, err
	}
	if memory.Len() <= replayBodyMemoryLimit {
		return &replayBody{data: memory.Bytes()}, nil
	}
	file, err := os.CreateTemp("", "openai-via-codex-request-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	if _, err := file.Write(memory.Bytes()); err != nil {
		cleanup()
		return nil, err
	}
	written, err := io.Copy(file, body)
	if err != nil {
		cleanup()
		return nil, err
	}
	return &replayBody{file: file, size: int64(memory.Len()) + written}, nil
}

func (b *replayBody) Reader() io.Reader {
	if b.file == nil {
		return bytes.NewReader(b.data)
	}
	return io.NewSectionReader(b.file, 0, b.size)
}

func (b *replayBody) Close() {
	if b.file != nil {
		name := b.file.Name()
		_ = b.file.Close()
		_ = os.Remove(name)
	}
}

func (b *backend) debugf(format string, args ...any) {
	if b.cfg.Verbose {
		log.Printf(format, args...)
	}
}

func (b *backend) transcribe(ctx context.Context, headers http.Header, body io.Reader) (*http.Response, error) {
	base := strings.TrimSuffix(b.cfg.BackendURL, "/codex")
	return b.proxyTo(ctx, http.MethodPost, base, "transcribe", "", headers, body)
}

func decodeBackendError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var doc map[string]any
	detail := map[string]any{}
	if json.Unmarshal(data, &doc) == nil {
		upstream := failureDetail(doc["error"])
		if upstream == nil {
			upstream = failureDetail(doc["detail"])
		}
		if upstream != nil {
			detail = cloneMap(upstream)
		}
	}
	if stringValue(detail["message"]) == "" && failureFallbackMessages[stringValue(detail["code"])] == "" {
		detail["message"] = resp.Status
	}
	detail["status"] = resp.StatusCode
	detail["headers"] = map[string]any{"retry-after": resp.Header.Get("Retry-After")}
	return responseFailure(detail)
}
func invalidProxyPath(path string) bool {
	_, err := cleanProxyPath(path)
	return err != nil
}

func cleanProxyPath(path string) (string, error) {
	if strings.Contains(path, "\\") {
		return "", errors.New("invalid proxy path")
	}
	for _, r := range path {
		if r == 0 || r < 0x20 || r == 0x7f {
			return "", errors.New("invalid proxy path")
		}
	}
	var segments []string
	for _, p := range strings.Split(path, "/") {
		if p == ".." {
			return "", errors.New("invalid proxy path")
		}
		if p != "" && p != "." {
			segments = append(segments, p)
		}
	}
	return strings.Join(segments, "/"), nil
}

func resolveProxyURL(baseURL, path, query string) (*url.URL, error) {
	cleaned, err := cleanProxyPath(path)
	if err != nil {
		return nil, err
	}
	target, err := url.Parse(baseURL)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("invalid backend base URL")
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/" + cleaned
	target.RawPath = ""
	target.RawQuery = query
	target.Fragment = ""
	return target, nil
}
