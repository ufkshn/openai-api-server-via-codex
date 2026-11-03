package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// poolFixture is an auth directory with one login per account id. Each
// account's access token is "tok-<id>", so the upstream can tell who called.
type poolFixture struct {
	t    *testing.T
	root string
}

func newPoolFixture(t *testing.T, ids ...string) *poolFixture {
	t.Helper()
	f := &poolFixture{t: t, root: t.TempDir()}
	for _, id := range ids {
		f.write(id, authDocument("tok-"+id, map[string]any{"account_id": "acct-" + id}))
	}
	return f
}

func (f *poolFixture) authJSON() string { return filepath.Join(f.root, "auth.json") }

func (f *poolFixture) path(id string) string {
	if id == defaultAccountID {
		return f.authJSON()
	}
	return filepath.Join(f.root, accountsDirName, id, "auth.json")
}

func (f *poolFixture) write(id string, doc map[string]any) {
	f.t.Helper()
	data, _ := json.Marshal(doc)
	if err := os.MkdirAll(filepath.Dir(f.path(id)), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.path(id), data, 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *poolFixture) backend(upstream *httptest.Server) *backend {
	cfg := defaultConfig()
	cfg.BackendURL = upstream.URL
	cfg.AuthJSON = f.authJSON()
	return newBackend(cfg)
}

func callerOf(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
}

const completedSSE = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n"

func streamOnce(b *backend, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{"model": "gpt-5.6-luna", "input": "hello"}
	}
	return b.stream(context.Background(), payload, func(map[string]any) error { return nil })
}

func TestPoolSendsConcurrentRequestsToTheLeastBusyAccount(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID, "b")
	var mu sync.Mutex
	var callers []string
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callers = append(callers, callerOf(r))
		first := len(callers) == 1
		mu.Unlock()
		if first {
			<-release // hold the first request open so its account stays busy
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedSSE)
	}))
	defer upstream.Close()
	b := f.backend(upstream)

	done := make(chan error, 1)
	go func() { done <- streamOnce(b, nil) }()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(callers) == 1 })
	if err := streamOnce(b, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if callers[0] == callers[1] {
		t.Fatalf("both requests went to %q while one was busy", callers[0])
	}
	b.pool.mu.Lock()
	defer b.pool.mu.Unlock()
	for id, acct := range b.pool.accounts {
		if acct.inflight != 0 {
			t.Fatalf("account %s still holds %d in-flight requests", id, acct.inflight)
		}
	}
}

func TestPoolMovesAnExhaustedAccountsRequestToAnotherAndCoolsItDown(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID, "b")
	var mu sync.Mutex
	var callers []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callers = append(callers, callerOf(r))
		mu.Unlock()
		if callerOf(r) == defaultAccountID {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"limit","resets_in_seconds":3600}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedSSE)
	}))
	defer upstream.Close()
	b := f.backend(upstream)

	for i := 0; i < 3; i++ {
		if err := streamOnce(b, nil); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	// The exhausted account is asked at most once; afterwards it is cooling.
	hits := 0
	for _, caller := range callers {
		if caller == defaultAccountID {
			hits++
		}
	}
	if hits > 1 {
		t.Fatalf("cooling account called %d times: %v", hits, callers)
	}
	b.pool.mu.Lock()
	acct := b.pool.accounts[defaultAccountID]
	cooling, reason := time.Until(acct.coolUntil), acct.coolReason
	b.pool.mu.Unlock()
	if reason != "usage_limit" || cooling < 50*time.Minute {
		t.Fatalf("cooldown = %s reason=%q, want ~1h usage_limit", cooling, reason)
	}
}

func TestPoolReturnsTheRefusalWhenEveryAccountIsExhausted(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID, "b")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit."}}`)
	}))
	defer upstream.Close()
	err := streamOnce(f.backend(upstream), nil)
	var backendErr *backendError
	if !errors.As(err, &backendErr) || backendErr.Status != http.StatusTooManyRequests || !strings.Contains(backendErr.Message, "usage limit") {
		t.Fatalf("error = %#v", err)
	}
}

// One account cooling down must not turn a single-account proxy into an
// outage: the next request still asks it.
func TestPoolStillTriesACoolingAccountWhenItIsTheOnlyOne(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedSSE)
	}))
	defer upstream.Close()
	b := f.backend(upstream)
	if err := streamOnce(b, nil); err == nil {
		t.Fatal("first request should carry the 429")
	}
	if err := streamOnce(b, nil); err != nil {
		t.Fatalf("second request = %v", err)
	}
}

func TestPoolSkipsDisabledAccountsAndRemembersAcrossRestarts(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID, "b")
	var callers []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callers = append(callers, callerOf(r))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedSSE)
	}))
	defer upstream.Close()
	b := f.backend(upstream)
	if err := b.pool.setEnabled("b", false); err != nil {
		t.Fatal(err)
	}
	restarted := f.backend(upstream)
	for i := 0; i < 4; i++ {
		if err := streamOnce(restarted, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, caller := range callers {
		if caller == "b" {
			t.Fatalf("disabled account served a request: %v", callers)
		}
	}
	if err := restarted.pool.setEnabled(defaultAccountID, false); err != nil {
		t.Fatal(err)
	}
	err := streamOnce(restarted, nil)
	var backendErr *backendError
	if !errors.As(err, &backendErr) || backendErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("all disabled: error = %#v", err)
	}
	if err := restarted.pool.setEnabled("b", true); err != nil {
		t.Fatal(err)
	}
	callers = nil
	if err := streamOnce(restarted, nil); err != nil || len(callers) != 1 || callers[0] != "b" {
		t.Fatalf("re-enabled: err=%v callers=%v", err, callers)
	}
}

func TestPoolRemoveDeletesTheLoginFromDisk(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID, "b")
	pool := newAccountPool(f.authJSON(), noRefreshClient(t))
	if err := pool.remove("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(f.path("b"))); !os.IsNotExist(err) {
		t.Fatalf("account directory still present: %v", err)
	}
	if err := pool.remove(defaultAccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.authJSON()); !os.IsNotExist(err) {
		t.Fatalf("auth.json still present: %v", err)
	}
	if err := pool.remove("b"); !errors.Is(err, errUnknownAccount) {
		t.Fatalf("second remove = %v", err)
	}
}

// Encrypted reasoning only decrypts on the account that produced it.
func TestPoolRoutesReplayedReasoningToTheAccountThatProducedIt(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID, "b")
	var mu sync.Mutex
	var callers []string
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		callers = append(callers, callerOf(r))
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		item := `{"type":"reasoning","id":"rs_1","encrypted_content":"secret-of-` + callerOf(r) + `","summary":[]}`
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":"+item+"}\n\n"+completedSSE)
	}))
	defer upstream.Close()
	b := f.backend(upstream)
	if err := streamOnce(b, nil); err != nil {
		t.Fatal(err)
	}
	producer := callers[0]
	followUp := map[string]any{"model": "gpt-5.6-luna", "input": []any{
		map[string]any{"type": "reasoning", "encrypted_content": "secret-of-" + producer, "summary": []any{}},
		map[string]any{"role": "user", "content": "and then?"},
	}}
	// Several times: the idle-longest tie-break alone would alternate.
	for i := 0; i < 3; i++ {
		if err := streamOnce(b, followUp); err != nil {
			t.Fatal(err)
		}
	}
	for _, caller := range callers[1:] {
		if caller != producer {
			t.Fatalf("follow-up went to %q, reasoning came from %q (%v)", caller, producer, callers)
		}
	}

	// With its home account disabled the request moves on without the reasoning.
	if err := b.pool.setEnabled(producer, false); err != nil {
		t.Fatal(err)
	}
	if err := streamOnce(b, followUp); err != nil {
		t.Fatal(err)
	}
	last := len(callers) - 1
	if callers[last] == producer {
		t.Fatal("disabled account served the follow-up")
	}
	for _, raw := range sliceAny(bodies[last]["input"]) {
		if mapAny(raw)["type"] == "reasoning" {
			t.Fatalf("reasoning from %q was sent to %q: %v", producer, callers[last], bodies[last]["input"])
		}
	}
}

func TestPoolResendsWithoutReasoningTheAccountCannotDecrypt(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID)
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		for _, raw := range sliceAny(body["input"]) {
			if mapAny(raw)["type"] == "reasoning" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"The encrypted content for item rs_x could not be verified.","code":"invalid_encrypted_content"}}`)
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedSSE)
	}))
	defer upstream.Close()
	// Reasoning from before a restart: the pool has no record of its account.
	err := streamOnce(f.backend(upstream), map[string]any{"model": "gpt-5.6-luna", "input": []any{
		map[string]any{"type": "reasoning", "encrypted_content": "from-elsewhere", "summary": []any{}},
		map[string]any{"role": "user", "content": "hi"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(bodies))
	}
}

func TestPoolCountsAWebSocketAsLoadUntilItCloses(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID)
	pool := newAccountPool(f.authJSON(), noRefreshClient(t))
	lease, err := pool.acquire(routeHints{}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if pool.accounts[defaultAccountID].inflight != 1 {
		t.Fatal("lease not counted")
	}
	lease.release()
	lease.release()
	if got := pool.accounts[defaultAccountID].inflight; got != 0 {
		t.Fatalf("in-flight after double release = %d", got)
	}
}

func TestPoolReadsUsageHeadersAndPrefersTheLessUsedAccount(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID, "b")
	pool := newAccountPool(f.authJSON(), noRefreshClient(t))
	now := time.Now()
	header := http.Header{}
	header.Set("x-codex-primary-used-percent", "90")
	header.Set("x-codex-primary-reset-after-seconds", "600")
	pool.observe(pool.accounts[defaultAccountID], &http.Response{StatusCode: 200, Header: header})
	usage := pool.accounts[defaultAccountID].usage
	if !usage.HasPrimary || usage.PrimaryPercent != 90 || usage.PrimaryResetAt.Before(now.Add(9*time.Minute)) {
		t.Fatalf("usage = %#v", usage)
	}
	lease, err := pool.acquire(routeHints{}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	if lease.id() != "b" {
		t.Fatalf("picked %q, want the less used account", lease.id())
	}
}

func TestUIAddsAccountsAndRefusesALoginAlreadyInThePool(t *testing.T) {
	s, _ := newUITestServer(t, "secret")
	session := uiRequest(s, http.MethodPost, "/ui/api/login", `{"key":"secret"}`, nil).Result().Cookies()[0]
	login := func(user string) string {
		token := testJWT(t, map[string]any{"sub": user, "email": user + "@example.com",
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "ws", "chatgpt_plan_type": "pro"}})
		doc, _ := json.Marshal(authDocument(token, map[string]any{"refresh_token": "r-" + user}))
		payload, _ := json.Marshal(map[string]string{"content": string(doc)})
		return string(payload)
	}
	paste := func(body string) *httptest.ResponseRecorder {
		return uiRequest(s, http.MethodPost, "/ui/api/auth/paste", body, session)
	}
	if rec := paste(login("alice")); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"account":"default"`) {
		t.Fatalf("first paste = %d %s", rec.Code, rec.Body.String())
	}
	second := paste(login("bob"))
	if second.Code != 200 || strings.Contains(second.Body.String(), `"account":"default"`) {
		t.Fatalf("second paste = %d %s", second.Code, second.Body.String())
	}
	if rec := paste(login("alice")); rec.Code != 409 || !strings.Contains(rec.Body.String(), "already account default") {
		t.Fatalf("duplicate paste = %d %s", rec.Code, rec.Body.String())
	}

	rec := uiRequest(s, http.MethodGet, "/ui/api/status", "", session)
	var status struct {
		Accounts []map[string]any `json:"accounts"`
		Ready    int              `json:"ready_accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Accounts) != 2 || status.Ready != 2 {
		t.Fatalf("status = %s", rec.Body.String())
	}
	if status.Accounts[0]["email"] != "alice@example.com" || status.Accounts[0]["plan"] != "pro" {
		t.Fatalf("first account = %#v", status.Accounts[0])
	}

	bobID := status.Accounts[1]["id"].(string)
	if rec := uiRequest(s, http.MethodPost, "/ui/api/accounts/disable", `{"account":"`+bobID+`"}`, session); rec.Code != 200 {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	rec = uiRequest(s, http.MethodGet, "/ui/api/status", "", session)
	_ = json.Unmarshal(rec.Body.Bytes(), &status)
	if status.Ready != 1 || status.Accounts[1]["enabled"] != false {
		t.Fatalf("after disable = %s", rec.Body.String())
	}
	if rec := uiRequest(s, http.MethodPost, "/ui/api/accounts/remove", `{"account":"nope"}`, session); rec.Code != 404 {
		t.Fatalf("remove unknown = %d", rec.Code)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
