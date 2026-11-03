package app

// A pool of ChatGPT accounts the proxy spreads its upstream traffic across.
//
// Why it exists: one ChatGPT subscription has one usage allowance. Several
// accounts behind one proxy let the clients keep a single base URL and key
// while the proxy decides, per request, which subscription carries it. Clients
// never see which account served them.
//
// Layout, chosen so a single-account deployment needs no migration:
//
//	<dir of auth_json>/auth.json              account "default" (the original file)
//	<dir of auth_json>/accounts/<id>/auth.json one directory per added account
//	<dir of auth_json>/accounts.json          which accounts the operator disabled
//
// Each account directory is that account's CODEX_HOME, so the device sign-in
// writes straight into it. Every file has exactly one writer — this process —
// because the refresh token rotates on use: two processes refreshing copies of
// one login invalidate each other. The same rule is why a login that is already
// in the pool is never added twice.
//
// Routing: the account with the fewest requests in flight wins; ties go to the
// lower reported usage, then to the one idle longest. A conversation stays on
// the account that produced its encrypted reasoning, because that content is
// only readable by the account that created it. An account that answers 429 or
// a persistent 401 cools down and the request moves to the next account before
// any byte reaches the client.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultAccountID      = "default"
	accountsDirName       = "accounts"
	accountsStateName     = "accounts.json"
	accountRescanInterval = 10 * time.Second

	// Used when a 429 carries no reset time. Short for a plain rate limit,
	// longer for an exhausted allowance that would only fail again.
	rateLimitFallbackCooldown  = time.Minute
	usageLimitFallbackCooldown = 5 * time.Minute
	maxAccountCooldown         = 8 * 24 * time.Hour
	unauthorizedCooldown       = 10 * time.Minute

	affinityTTL        = 6 * time.Hour
	affinityMaxEntries = 20000
	// A cache-key affinity is a preference, not a pin: it yields once the
	// preferred account is this many requests busier than the idlest one.
	stickySlack = 2
)

var accountIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

type accountUsage struct {
	PrimaryPercent   float64
	SecondaryPercent float64
	HasPrimary       bool
	HasSecondary     bool
	PrimaryResetAt   time.Time
	SecondaryResetAt time.Time
	UpdatedAt        time.Time
}

type poolAccount struct {
	id   string
	path string
	auth *authProvider

	inflight int
	served   int64
	lastUsed time.Time

	coolUntil  time.Time
	coolReason string
	// coolStamp is the auth file's (mtime,size) when the cooldown began. A new
	// login written over the file lifts the cooldown at once.
	coolStamp string

	lastError   string
	lastErrorAt time.Time
	usage       accountUsage
}

type affinityEntry struct {
	account string
	at      time.Time
}

type accountPool struct {
	mu            sync.Mutex
	primaryPath   string
	root          string
	refreshClient *http.Client
	accounts      map[string]*poolAccount
	disabled      map[string]bool
	scannedAt     time.Time
	affinity      map[string]affinityEntry
	now           func() time.Time
}

func newAccountPool(authJSON string, refreshClient *http.Client) *accountPool {
	primary := expandHome(authJSON)
	p := &accountPool{
		primaryPath:   primary,
		root:          filepath.Dir(primary),
		refreshClient: refreshClient,
		accounts:      map[string]*poolAccount{},
		disabled:      map[string]bool{},
		affinity:      map[string]affinityEntry{},
		now:           time.Now,
	}
	p.mu.Lock()
	p.rescanLocked()
	p.mu.Unlock()
	return p
}

func (p *accountPool) accountDir(id string) string {
	if id == defaultAccountID {
		return p.root
	}
	return filepath.Join(p.root, accountsDirName, id)
}

func (p *accountPool) accountPath(id string) string {
	if id == defaultAccountID {
		return p.primaryPath
	}
	return filepath.Join(p.accountDir(id), "auth.json")
}

// rescanLocked reconciles the in-memory accounts with the files on disk. It
// keeps the existing object for an account that is still present, so its
// credential cache, load and cooldown survive the rescan.
func (p *accountPool) rescanLocked() {
	p.scannedAt = p.now()
	present := map[string]bool{}
	if fileExists(p.primaryPath) {
		present[defaultAccountID] = true
	}
	if entries, err := os.ReadDir(filepath.Join(p.root, accountsDirName)); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && accountIDPattern.MatchString(entry.Name()) && entry.Name() != defaultAccountID &&
				fileExists(filepath.Join(p.root, accountsDirName, entry.Name(), "auth.json")) {
				present[entry.Name()] = true
			}
		}
	}
	for id := range p.accounts {
		if !present[id] {
			delete(p.accounts, id)
		}
	}
	for id := range present {
		if p.accounts[id] == nil {
			path := p.accountPath(id)
			p.accounts[id] = &poolAccount{id: id, path: path, auth: &authProvider{path: path, refreshClient: p.refreshClient}}
		}
	}
	for _, acct := range p.accounts {
		if acct.coolStamp != "" && fileStamp(acct.path) != acct.coolStamp {
			acct.coolUntil, acct.coolReason, acct.coolStamp = time.Time{}, "", ""
		}
	}
	p.disabled = readDisabledAccounts(filepath.Join(p.root, accountsStateName))
}

func (p *accountPool) maybeRescanLocked() {
	if p.now().Sub(p.scannedAt) >= accountRescanInterval {
		p.rescanLocked()
	}
}

func (p *accountPool) rescan() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rescanLocked()
}

// orderedLocked lists accounts with "default" first, then by id, which for
// generated ids is the order they were added.
func (p *accountPool) orderedLocked() []*poolAccount {
	list := make([]*poolAccount, 0, len(p.accounts))
	for _, acct := range p.accounts {
		list = append(list, acct)
	}
	sort.Slice(list, func(i, j int) bool {
		if (list[i].id == defaultAccountID) != (list[j].id == defaultAccountID) {
			return list[i].id == defaultAccountID
		}
		return list[i].id < list[j].id
	})
	return list
}

// routeHints carries what a request says about where it belongs.
type routeHints struct {
	// reasoningKeys fingerprint encrypted reasoning the request replays; a hit
	// pins the request to the account that produced it.
	reasoningKeys []string
	// stickyKey is the client's prompt-cache or session key; a hit is only a
	// preference, so prompt caching keeps working without defeating balancing.
	stickyKey string
}

func hintsForPayload(payload map[string]any) routeHints {
	var hints routeHints
	for _, raw := range sliceAny(payload["input"]) {
		item := mapAny(raw)
		if item != nil && item["type"] == "reasoning" {
			if content := stringValue(item["encrypted_content"]); content != "" {
				hints.reasoningKeys = append(hints.reasoningKeys, reasoningKey(content))
			}
		}
	}
	if key := stringValue(payload["prompt_cache_key"]); key != "" {
		hints.stickyKey = "ck:" + key
	}
	return hints
}

func reasoningKey(encrypted string) string {
	sum := sha256.Sum256([]byte(encrypted))
	return "rs:" + hex.EncodeToString(sum[:16])
}

// accountLease is one request's claim on an account. Releasing it ends the
// request's share of that account's load; it is safe to call more than once.
type accountLease struct {
	pool *accountPool
	acct *poolAccount
	cred credentials
	// pinMissed means the request replays reasoning from an account that is not
	// the one serving it, so that reasoning must not be sent.
	pinMissed bool
	once      sync.Once
}

func (l *accountLease) id() string { return l.acct.id }

func (l *accountLease) release() {
	l.once.Do(func() {
		l.pool.mu.Lock()
		l.acct.inflight--
		l.pool.mu.Unlock()
	})
}

func (l *accountLease) reload() (credentials, error) {
	cred, err := l.acct.auth.reload()
	if err == nil {
		l.cred = cred
	}
	return cred, err
}

// errNoAccount is returned when the pool has nothing it could even try.
type poolUnavailable struct {
	status  int
	message string
}

func (e *poolUnavailable) Error() string { return e.message }

// acquire picks an account for one request. Accounts in exclude were already
// tried by this request. When every enabled account is cooling down, the one
// that recovers soonest is tried anyway: a cooldown is a guess, and refusing
// without asking would turn a single-account proxy into an outage.
func (p *accountPool) acquire(hints routeHints, exclude map[string]bool) (*accountLease, error) {
	var credErr error
	for {
		p.mu.Lock()
		p.maybeRescanLocked()
		acct, pinMissed := p.chooseLocked(hints, exclude)
		if acct == nil && credErr == nil && len(exclude) == 0 {
			// Nothing to try may only mean a login appeared since the last scan.
			p.rescanLocked()
			acct, pinMissed = p.chooseLocked(hints, exclude)
		}
		if acct == nil {
			err := p.unavailableLocked(exclude, credErr)
			p.mu.Unlock()
			return nil, err
		}
		acct.inflight++
		acct.served++
		acct.lastUsed = p.now()
		p.mu.Unlock()

		// Outside the lock: borrowing may refresh the token over the network.
		cred, err := acct.auth.borrow()
		if err != nil {
			p.mu.Lock()
			acct.inflight--
			acct.served--
			acct.lastError, acct.lastErrorAt = redactSensitive(err.Error()), p.now()
			p.mu.Unlock()
			logAuthFailure("account="+acct.id, err)
			credErr = err
			exclude[acct.id] = true
			continue
		}
		lease := &accountLease{pool: p, acct: acct, cred: cred, pinMissed: pinMissed}
		if hints.stickyKey != "" {
			p.remember(hints.stickyKey, acct.id)
		}
		return lease, nil
	}
}

func (p *accountPool) chooseLocked(hints routeHints, exclude map[string]bool) (*poolAccount, bool) {
	now := p.now()
	var ready, cooling []*poolAccount
	for _, acct := range p.orderedLocked() {
		if exclude[acct.id] || p.disabled[acct.id] {
			continue
		}
		if now.Before(acct.coolUntil) {
			cooling = append(cooling, acct)
		} else {
			ready = append(ready, acct)
		}
	}
	pinned := ""
	for _, key := range hints.reasoningKeys {
		if entry, ok := p.affinityLocked(key); ok {
			pinned = entry
			break
		}
	}
	if pinned != "" {
		for _, acct := range ready {
			if acct.id == pinned {
				return acct, false
			}
		}
	}
	if len(ready) == 0 {
		if len(cooling) == 0 {
			return nil, false
		}
		sort.SliceStable(cooling, func(i, j int) bool { return cooling[i].coolUntil.Before(cooling[j].coolUntil) })
		return cooling[0], pinned != "" && cooling[0].id != pinned
	}
	best := ready[0]
	for _, acct := range ready[1:] {
		if lessLoaded(acct, best) {
			best = acct
		}
	}
	if pinned == "" && hints.stickyKey != "" {
		if preferred, ok := p.affinityLocked(hints.stickyKey); ok {
			for _, acct := range ready {
				if acct.id == preferred && acct.inflight <= best.inflight+stickySlack {
					return acct, false
				}
			}
		}
	}
	return best, pinned != "" && best.id != pinned
}

func lessLoaded(a, b *poolAccount) bool {
	if a.inflight != b.inflight {
		return a.inflight < b.inflight
	}
	if ua, ub := usageScore(a), usageScore(b); ua != ub {
		return ua < ub
	}
	return a.lastUsed.Before(b.lastUsed)
}

// usageScore is the tighter of the two reported windows; an account that has
// never reported usage counts as unused.
func usageScore(a *poolAccount) float64 {
	score := 0.0
	if a.usage.HasPrimary {
		score = a.usage.PrimaryPercent
	}
	if a.usage.HasSecondary && a.usage.SecondaryPercent > score {
		score = a.usage.SecondaryPercent
	}
	return score
}

func (p *accountPool) unavailableLocked(exclude map[string]bool, credErr error) error {
	if credErr != nil {
		return credErr
	}
	if len(p.accounts) == 0 {
		return authFileStatFailure(p.primaryPath, os.ErrNotExist)
	}
	enabled := 0
	for id := range p.accounts {
		if !p.disabled[id] {
			enabled++
		}
	}
	if enabled == 0 {
		return &poolUnavailable{503, "Every Codex account on this proxy is disabled; enable one in the console at /ui."}
	}
	return &poolUnavailable{503, "No Codex account is available for this request."}
}

func (p *accountPool) affinityLocked(key string) (string, bool) {
	entry, ok := p.affinity[key]
	if !ok {
		return "", false
	}
	if p.now().Sub(entry.at) > affinityTTL || p.accounts[entry.account] == nil {
		delete(p.affinity, key)
		return "", false
	}
	return entry.account, true
}

func (p *accountPool) remember(key, account string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if len(p.affinity) >= affinityMaxEntries {
		for k, entry := range p.affinity {
			if now.Sub(entry.at) > affinityTTL {
				delete(p.affinity, k)
			}
		}
		// Still full of live entries: drop an arbitrary half rather than grow.
		if len(p.affinity) >= affinityMaxEntries {
			dropped := 0
			for k := range p.affinity {
				delete(p.affinity, k)
				if dropped++; dropped >= affinityMaxEntries/2 {
					break
				}
			}
		}
	}
	p.affinity[key] = affinityEntry{account: account, at: now}
}

// rememberOutput pins the encrypted reasoning a response produced to the
// account that produced it.
func (p *accountPool) rememberOutput(account string, item map[string]any) {
	if item == nil || item["type"] != "reasoning" {
		return
	}
	if content := stringValue(item["encrypted_content"]); content != "" {
		p.remember(reasoningKey(content), account)
	}
}

// observe records what an upstream response says about its account.
func (p *accountPool) observe(acct *poolAccount, resp *http.Response) {
	usage, ok := usageFromHeaders(resp.Header, p.now())
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok {
		acct.usage = usage
	}
	if resp.StatusCode/100 == 2 {
		acct.lastError, acct.lastErrorAt = "", time.Time{}
	}
}

// observeRateLimitEvent reads the usage snapshot Codex sends inside a stream.
func (p *accountPool) observeRateLimitEvent(account string, event map[string]any) {
	usage, ok := usageFromEvent(event, p.now())
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if acct := p.accounts[account]; acct != nil {
		acct.usage = usage
	}
}

func (p *accountPool) coolDown(acct *poolAccount, d time.Duration, reason, detail string) {
	if d > maxAccountCooldown {
		d = maxAccountCooldown
	}
	stamp := fileStamp(acct.path)
	p.mu.Lock()
	until := p.now().Add(d)
	acct.coolUntil, acct.coolReason, acct.coolStamp = until, reason, stamp
	acct.lastError, acct.lastErrorAt = detail, p.now()
	p.mu.Unlock()
	log.Printf("codex.account.cooldown account=%s reason=%s until=%s", acct.id, reason, until.UTC().Format(time.RFC3339))
}

func (p *accountPool) clearCooldown(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if acct := p.accounts[id]; acct != nil {
		acct.coolUntil, acct.coolReason, acct.coolStamp = time.Time{}, "", ""
	}
}

// cooldownForStatus decides whether an upstream answer means "this account,
// not this request", and for how long. body is the buffered error body.
func cooldownForStatus(status int, header http.Header, body []byte, now time.Time) (time.Duration, string, bool) {
	switch status {
	case http.StatusTooManyRequests:
		var doc map[string]any
		_ = json.Unmarshal(body, &doc)
		errDoc := mapAny(doc["error"])
		reason := "rate_limited"
		fallback := rateLimitFallbackCooldown
		if kind := stringValue(errDoc["type"]) + " " + stringValue(errDoc["code"]); strings.Contains(kind, "usage_limit") {
			reason, fallback = "usage_limit", usageLimitFallbackCooldown
		}
		if seconds, ok := numberValue(errDoc["resets_in_seconds"]); ok && seconds > 0 {
			return time.Duration(seconds) * time.Second, reason, true
		}
		if at, ok := numberValue(errDoc["resets_at"]); ok && at > float64(now.Unix()) {
			return time.Unix(int64(at), 0).Sub(now), reason, true
		}
		if retry, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After"))); err == nil && retry > 0 {
			return time.Duration(retry) * time.Second, reason, true
		}
		if usage, ok := usageFromHeaders(header, now); ok {
			if usage.HasPrimary && usage.PrimaryPercent >= 100 && usage.PrimaryResetAt.After(now) {
				return usage.PrimaryResetAt.Sub(now), "usage_limit", true
			}
			if usage.HasSecondary && usage.SecondaryPercent >= 100 && usage.SecondaryResetAt.After(now) {
				return usage.SecondaryResetAt.Sub(now), "usage_limit", true
			}
		}
		return fallback, reason, true
	case http.StatusUnauthorized:
		return unauthorizedCooldown, "unauthorized", true
	}
	return 0, "", false
}

// usageFromHeaders reads the x-codex-{primary,secondary}-* headers Codex
// attaches to its responses.
func usageFromHeaders(h http.Header, now time.Time) (accountUsage, bool) {
	var usage accountUsage
	read := func(window string) (float64, time.Time, bool) {
		percent, err := strconv.ParseFloat(strings.TrimSpace(h.Get("x-codex-"+window+"-used-percent")), 64)
		if err != nil {
			return 0, time.Time{}, false
		}
		var reset time.Time
		if at, err := strconv.ParseFloat(strings.TrimSpace(h.Get("x-codex-"+window+"-reset-at")), 64); err == nil && at > 0 {
			reset = time.Unix(int64(at), 0)
		} else if after, err := strconv.ParseFloat(strings.TrimSpace(h.Get("x-codex-"+window+"-reset-after-seconds")), 64); err == nil && after >= 0 {
			reset = now.Add(time.Duration(after) * time.Second)
		}
		return percent, reset, true
	}
	usage.PrimaryPercent, usage.PrimaryResetAt, usage.HasPrimary = read("primary")
	usage.SecondaryPercent, usage.SecondaryResetAt, usage.HasSecondary = read("secondary")
	if !usage.HasPrimary && !usage.HasSecondary {
		return usage, false
	}
	usage.UpdatedAt = now
	return usage, true
}

// usageFromEvent reads a `codex.rate_limits` stream event.
func usageFromEvent(event map[string]any, now time.Time) (accountUsage, bool) {
	if event["type"] != "codex.rate_limits" {
		return accountUsage{}, false
	}
	limits := mapAny(event["rate_limits"])
	if limits == nil {
		limits = event
	}
	var usage accountUsage
	read := func(raw any) (float64, time.Time, bool) {
		window := mapAny(raw)
		percent, ok := numberValue(window["used_percent"])
		if !ok {
			return 0, time.Time{}, false
		}
		var reset time.Time
		if at, ok := numberValue(window["resets_at"]); ok && at > 0 {
			reset = time.Unix(int64(at), 0)
		} else if after, ok := numberValue(window["reset_after_seconds"]); ok {
			reset = now.Add(time.Duration(after) * time.Second)
		}
		return percent, reset, true
	}
	usage.PrimaryPercent, usage.PrimaryResetAt, usage.HasPrimary = read(limits["primary"])
	usage.SecondaryPercent, usage.SecondaryResetAt, usage.HasSecondary = read(limits["secondary"])
	if !usage.HasPrimary && !usage.HasSecondary {
		return usage, false
	}
	usage.UpdatedAt = now
	return usage, true
}

func numberValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

// preflight reports whether at least one enabled account has usable
// credentials, returning the first failure when none does.
func (p *accountPool) preflight() error {
	p.mu.Lock()
	p.rescanLocked()
	list := p.orderedLocked()
	disabled := p.disabled
	p.mu.Unlock()
	var firstErr error
	for _, acct := range list {
		if disabled[acct.id] {
			continue
		}
		if _, err := acct.auth.borrow(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return nil
	}
	if firstErr != nil {
		return firstErr
	}
	if len(list) == 0 {
		return authFileStatFailure(p.primaryPath, os.ErrNotExist)
	}
	return newAuthFailure(authCodeUnknown, "every Codex account on this proxy is disabled")
}

// --- operator actions -------------------------------------------------------

var errUnknownAccount = errors.New("Unknown account.")

func (p *accountPool) setEnabled(id string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rescanLocked()
	if p.accounts[id] == nil {
		return errUnknownAccount
	}
	next := map[string]bool{}
	for k, v := range p.disabled {
		next[k] = v
	}
	if enabled {
		delete(next, id)
	} else {
		next[id] = true
	}
	if err := writeDisabledAccounts(filepath.Join(p.root, accountsStateName), next); err != nil {
		return err
	}
	p.disabled = next
	if enabled {
		// Re-enabling is the operator saying "try it again".
		acct := p.accounts[id]
		acct.coolUntil, acct.coolReason, acct.coolStamp = time.Time{}, "", ""
	}
	log.Printf("codex.account.%s account=%s", map[bool]string{true: "enabled", false: "disabled"}[enabled], id)
	return nil
}

// remove deletes an account's login. Requests already running on it finish
// with the credentials they hold.
func (p *accountPool) remove(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rescanLocked()
	if p.accounts[id] == nil {
		return errUnknownAccount
	}
	if id == defaultAccountID {
		if err := os.Remove(p.primaryPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := os.RemoveAll(p.accountDir(id)); err != nil {
		return err
	}
	if p.disabled[id] {
		next := map[string]bool{}
		for k, v := range p.disabled {
			if k != id {
				next[k] = v
			}
		}
		_ = writeDisabledAccounts(filepath.Join(p.root, accountsStateName), next)
	}
	for key, entry := range p.affinity {
		if entry.account == id {
			delete(p.affinity, key)
		}
	}
	p.rescanLocked()
	log.Printf("codex.account.removed account=%s", id)
	return nil
}

// newAccountID returns an id that sorts by creation time. The first account
// of an empty pool takes the original auth.json slot, so a fresh single-account
// deployment looks exactly like it did before the pool existed.
func (p *accountPool) newAccountID() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rescanLocked()
	if len(p.accounts) == 0 && !fileExists(p.primaryPath) {
		return defaultAccountID, nil
	}
	raw := make([]byte, 2)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return p.now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(raw), nil
}

// identity is what makes two auth files the same login: the same user in the
// same ChatGPT workspace. A Team workspace shares the account id across users,
// so the user id is part of the key.
func authIdentity(content []byte) string {
	var doc map[string]any
	if json.Unmarshal(content, &doc) != nil {
		return ""
	}
	tokens := mapAny(doc["tokens"])
	if tokens == nil {
		return ""
	}
	user := ""
	for _, key := range []string{"id_token", "access_token"} {
		payload := jwtPayload(stringValue(tokens[key]))
		if payload == nil {
			continue
		}
		if nested := mapAny(payload["https://api.openai.com/auth"]); nested != nil {
			if v := stringValue(nested["chatgpt_user_id"]); v != "" {
				user = v
				break
			}
			if v := stringValue(nested["user_id"]); v != "" {
				user = v
				break
			}
		}
		if v := stringValue(payload["sub"]); v != "" {
			user = v
			break
		}
	}
	account := accountID(tokens)
	if user == "" && account == "" {
		return ""
	}
	return user + "|" + account
}

// findIdentity returns the account already holding this login, if any.
func (p *accountPool) findIdentity(content []byte, except string) string {
	identity := authIdentity(content)
	if identity == "" {
		return ""
	}
	p.mu.Lock()
	list := p.orderedLocked()
	p.mu.Unlock()
	for _, acct := range list {
		if acct.id == except {
			continue
		}
		data, err := os.ReadFile(acct.path)
		if err == nil && authIdentity(data) == identity {
			return acct.id
		}
	}
	return ""
}

// writeAccount atomically writes a login into an account slot and makes the
// next request read it.
func (p *accountPool) writeAccount(id string, content []byte) error {
	if !accountIDPattern.MatchString(id) {
		return errUnknownAccount
	}
	path := p.accountPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// A partial write must never replace a working login.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".auth-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rescanLocked()
	if acct := p.accounts[id]; acct != nil {
		acct.auth.invalidate()
		acct.coolUntil, acct.coolReason, acct.coolStamp = time.Time{}, "", ""
		acct.lastError, acct.lastErrorAt = "", time.Time{}
	}
	return nil
}

// snapshot describes every account for the console. It borrows credentials,
// which refreshes them when needed, so "signed in" means they work.
func (p *accountPool) snapshot() []map[string]any {
	p.mu.Lock()
	p.rescanLocked()
	list := p.orderedLocked()
	p.mu.Unlock()
	result := make([]map[string]any, 0, len(list))
	for _, acct := range list {
		cred, credErr := acct.auth.borrow()
		p.mu.Lock()
		now := p.now()
		entry := map[string]any{
			"id":        acct.id,
			"path":      acct.path,
			"enabled":   !p.disabled[acct.id],
			"in_flight": acct.inflight,
			"served":    acct.served,
		}
		if !acct.lastUsed.IsZero() {
			entry["last_used"] = acct.lastUsed.UTC().Format(time.RFC3339)
		}
		if now.Before(acct.coolUntil) {
			entry["cooling_until"] = acct.coolUntil.UTC().Format(time.RFC3339)
			entry["cooling_reason"] = acct.coolReason
		}
		if acct.lastError != "" {
			entry["last_error"] = acct.lastError
			entry["last_error_at"] = acct.lastErrorAt.UTC().Format(time.RFC3339)
		}
		if acct.usage.HasPrimary {
			entry["primary_used_percent"] = acct.usage.PrimaryPercent
			if !acct.usage.PrimaryResetAt.IsZero() {
				entry["primary_resets_at"] = acct.usage.PrimaryResetAt.UTC().Format(time.RFC3339)
			}
		}
		if acct.usage.HasSecondary {
			entry["secondary_used_percent"] = acct.usage.SecondaryPercent
			if !acct.usage.SecondaryResetAt.IsZero() {
				entry["secondary_resets_at"] = acct.usage.SecondaryResetAt.UTC().Format(time.RFC3339)
			}
		}
		p.mu.Unlock()
		if credErr != nil {
			entry["signed_in"] = false
			entry["error"] = redactSensitive(credErr.Error())
		} else {
			entry["signed_in"] = true
			entry["account_id"] = cred.AccountID
			if exp, ok := jwtNumber(cred.AccessToken, "exp"); ok {
				expiry := time.Unix(int64(exp), 0).UTC()
				entry["expires_at"] = expiry.Format(time.RFC3339)
			}
			if data, err := os.ReadFile(acct.path); err == nil {
				for key, value := range loginClaims(data) {
					entry[key] = value
				}
			}
		}
		result = append(result, entry)
	}
	return result
}

// loginClaims pulls the display fields (email, plan) out of an auth file.
func loginClaims(content []byte) map[string]any {
	claims := map[string]any{}
	var doc map[string]any
	if json.Unmarshal(content, &doc) != nil {
		return claims
	}
	tokens := mapAny(doc["tokens"])
	for _, key := range []string{"id_token", "access_token"} {
		payload := jwtPayload(stringValue(tokens[key]))
		if payload == nil {
			continue
		}
		if email := stringValue(payload["email"]); email != "" && claims["email"] == nil {
			claims["email"] = email
		}
		if nested := mapAny(payload["https://api.openai.com/auth"]); nested != nil {
			if plan := stringValue(nested["chatgpt_plan_type"]); plan != "" && claims["plan"] == nil {
				claims["plan"] = plan
			}
		}
	}
	return claims
}

// --- files ------------------------------------------------------------------

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func fileStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

type accountsState struct {
	Disabled []string `json:"disabled"`
}

func readDisabledAccounts(path string) map[string]bool {
	result := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return result
	}
	var state accountsState
	if json.Unmarshal(data, &state) != nil {
		log.Printf("codex.accounts.state_unreadable path=%s action=treat_all_enabled", path)
		return result
	}
	for _, id := range state.Disabled {
		result[id] = true
	}
	return result
}

func writeDisabledAccounts(path string, disabled map[string]bool) error {
	state := accountsState{Disabled: []string{}}
	for id, off := range disabled {
		if off {
			state.Disabled = append(state.Disabled, id)
		}
	}
	sort.Strings(state.Disabled)
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// bufferedBody reads an error body so it can be inspected and still handed to
// the caller unchanged.
func bufferedBody(resp *http.Response) []byte {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(strings.NewReader(string(data)))
	return data
}
