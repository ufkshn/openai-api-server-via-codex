package app

import "time"

// activityWindow is how far back the console's activity summary looks.
const activityWindow = time.Hour

// requestRecord is one client request to the model, as the console counts it.
type requestRecord struct {
	at       time.Time
	model    string
	answered bool
	failed   bool
	input    int64
	cached   int64
	output   int64
}

// observe reads the outcome off the stream's terminal event.
func (r *requestRecord) observe(event map[string]any) {
	switch event["type"] {
	case "response.completed", "response.incomplete":
	default:
		return
	}
	response := mapAny(event["response"])
	if response == nil {
		return
	}
	r.answered = true
	if model := stringValue(response["model"]); model != "" {
		r.model = model
	}
	usage := mapAny(response["usage"])
	count := func(doc map[string]any, key string) int64 {
		n, _ := numberValue(doc[key])
		return int64(n)
	}
	r.input = count(usage, "input_tokens")
	r.output = count(usage, "output_tokens")
	r.cached = count(mapAny(usage["input_tokens_details"]), "cached_tokens")
}

// activityLog holds the last hour in memory: enough for "what is the proxy
// doing right now", and nothing a restart needs to keep.
type activityLog struct {
	requests  []requestRecord
	failovers []time.Time
}

func (l *activityLog) prune(now time.Time) {
	cutoff := now.Add(-activityWindow)
	keep := 0
	for keep < len(l.requests) && !l.requests[keep].at.After(cutoff) {
		keep++
	}
	l.requests = l.requests[keep:]
	keep = 0
	for keep < len(l.failovers) && !l.failovers[keep].After(cutoff) {
		keep++
	}
	l.failovers = l.failovers[keep:]
}

func (p *accountPool) recordRequest(record requestRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	record.at = p.now()
	p.activity.prune(record.at)
	p.activity.requests = append(p.activity.requests, record)
}

// recordFailover counts a request one account refused and the pool moved on.
func (p *accountPool) recordFailover() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.activity.prune(now)
	p.activity.failovers = append(p.activity.failovers, now)
}

// activitySnapshot sums the last hour for the console. Tokens are per model so
// a reader can price them; input_tokens includes the cached ones, as Codex
// reports them.
func (p *accountPool) activitySnapshot() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.activity.prune(p.now())
	type sums struct{ requests, input, cached, output int64 }
	byModel := map[string]*sums{}
	var order []string
	failed := 0
	for _, record := range p.activity.requests {
		if record.failed {
			failed++
		}
		entry := byModel[record.model]
		if entry == nil {
			entry = &sums{}
			byModel[record.model] = entry
			order = append(order, record.model)
		}
		entry.requests++
		entry.input += record.input
		entry.cached += record.cached
		entry.output += record.output
	}
	models := make([]map[string]any, 0, len(order))
	for _, model := range order {
		entry := byModel[model]
		models = append(models, map[string]any{
			"model":               model,
			"requests":            entry.requests,
			"input_tokens":        entry.input,
			"cached_input_tokens": entry.cached,
			"output_tokens":       entry.output,
		})
	}
	return map[string]any{
		"window_seconds": int(activityWindow / time.Second),
		"requests":       len(p.activity.requests),
		"failed":         failed,
		"failovers":      len(p.activity.failovers),
		"models":         models,
	}
}
