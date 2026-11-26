package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUsageNamesAWindowByItsLengthAndKeepsNoResetTimeForAnEmptyOne(t *testing.T) {
	now := time.Now()
	header := http.Header{}
	// A plan with only a weekly limit: Codex reports it first, and an empty
	// second window that resets "in 0 seconds".
	header.Set("x-codex-primary-used-percent", "49")
	header.Set("x-codex-primary-window-minutes", "10080")
	header.Set("x-codex-primary-reset-after-seconds", "486000")
	header.Set("x-codex-secondary-used-percent", "0")
	header.Set("x-codex-secondary-reset-after-seconds", "0")
	usage, ok := usageFromHeaders(header, now)
	if !ok || usage.PrimaryWindowMinutes != 10080 || usage.PrimaryResetAt.Before(now.Add(5*24*time.Hour)) {
		t.Fatalf("primary = %#v", usage)
	}
	if !usage.HasSecondary || !usage.SecondaryResetAt.IsZero() || usage.SecondaryWindowMinutes != 0 {
		t.Fatalf("secondary = %#v, want no reset time and no length", usage)
	}

	event := map[string]any{"type": "codex.rate_limits", "rate_limits": map[string]any{
		"primary":   map[string]any{"used_percent": 12.0, "window_minutes": 300.0, "reset_after_seconds": 60.0},
		"secondary": map[string]any{"used_percent": 0.0, "window_minutes": 10080.0, "reset_after_seconds": 0.0},
	}}
	usage, ok = usageFromEvent(event, now)
	if !ok || usage.PrimaryWindowMinutes != 300 || usage.SecondaryWindowMinutes != 10080 || !usage.SecondaryResetAt.IsZero() {
		t.Fatalf("event usage = %#v", usage)
	}
}

func TestStatusCarriesWindowLengths(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID)
	pool := newAccountPool(f.authJSON(), noRefreshClient(t))
	header := http.Header{}
	header.Set("x-codex-primary-used-percent", "49")
	header.Set("x-codex-primary-window-minutes", "10080")
	header.Set("x-codex-primary-reset-after-seconds", "600")
	header.Set("x-codex-secondary-used-percent", "0")
	header.Set("x-codex-secondary-reset-after-seconds", "0")
	pool.observe(pool.accounts[defaultAccountID], &http.Response{StatusCode: 200, Header: header})
	entry := pool.snapshot()[0]
	if entry["primary_window_minutes"] != 10080 || entry["primary_resets_at"] == nil {
		t.Fatalf("primary = %#v", entry)
	}
	if _, has := entry["secondary_resets_at"]; has {
		t.Fatalf("an empty window got a reset time: %#v", entry)
	}
	if _, has := entry["secondary_window_minutes"]; has {
		t.Fatalf("a window of unknown length got one: %#v", entry)
	}
}

func TestActivityCountsTheLastHoursRequestsFailuresAndTokens(t *testing.T) {
	f := newPoolFixture(t, defaultAccountID, "b")
	fail := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case fail:
			w.WriteHeader(http.StatusInternalServerError)
		case callerOf(r) == defaultAccountID:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"limit","resets_in_seconds":3600}}`)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\",\"status\":\"completed\",\"model\":\"gpt-5.6-luna\",\"output\":[],"+
				"\"usage\":{\"input_tokens\":100,\"input_tokens_details\":{\"cached_tokens\":40},\"output_tokens\":7}}}\n\ndata: [DONE]\n\n")
		}
	}))
	defer upstream.Close()
	b := f.backend(upstream)
	now := time.Now()
	b.pool.now = func() time.Time { return now }

	// The first request is refused by one account and answered by the other:
	// one request, one failover, no failure.
	for i := 0; i < 2; i++ {
		if err := streamOnce(b, nil); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	fail = true
	if err := streamOnce(b, nil); err == nil {
		t.Fatal("a 500 from Codex was not an error")
	}

	activity := b.pool.activitySnapshot()
	if activity["requests"] != 3 || activity["failed"] != 1 || activity["failovers"] != 1 || activity["window_seconds"] != 3600 {
		t.Fatalf("activity = %#v", activity)
	}
	models := activity["models"].([]map[string]any)
	if len(models) != 1 || models[0]["model"] != "gpt-5.6-luna" || models[0]["requests"] != int64(3) ||
		models[0]["input_tokens"] != int64(200) || models[0]["cached_input_tokens"] != int64(80) || models[0]["output_tokens"] != int64(14) {
		t.Fatalf("models = %#v", models)
	}

	// An hour on, none of it is recent any more.
	now = now.Add(time.Hour + time.Second)
	later := b.pool.activitySnapshot()
	if later["requests"] != 0 || later["failovers"] != 0 || len(later["models"].([]map[string]any)) != 0 {
		t.Fatalf("activity an hour later = %#v", later)
	}
}
