package app

// A small operator console for signing this proxy in to a ChatGPT account.
//
// Why it exists: the server only ever *reads* auth.json. Producing that file
// normally means shell access to run `codex login`, which is exactly what an
// unattended container on a shared host does not offer. The console closes that
// gap with two paths — a device-code sign-in and a paste fallback — so the login
// can be established and re-established from a browser. With several accounts
// in the pool it is also where they are added, disabled and removed.
//
// The device-code path drives the official Codex CLI rather than calling the
// device-auth API directly. That API is undocumented (usercode + an unspecified
// poll step), so the CLI stays the safer dependency: it owns the poll loop, the
// token exchange, and the exact auth.json layout the reader expects.
//
// Access control: the console is gated by the same API key as /v1, exchanged for
// an HttpOnly session cookie because a browser cannot attach a bearer header to
// a navigation. Signing in here grants control of the ChatGPT account, so when
// no API key is configured the console refuses to serve rather than defaulting
// open.

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	uiSessionCookie = "codex_ui_session"
	uiSessionTTL    = 12 * time.Hour
	// The CLI prints this as the page the operator opens to enter the code.
	uiDeviceVerificationURL = "https://auth.openai.com/codex/device"
)

// ansiPattern strips the colour codes the CLI wraps around the URL and code.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// userCodePattern matches the one-time code, e.g. "WXET-EA6X0".
var userCodePattern = regexp.MustCompile(`\b([A-Z0-9]{4}-[A-Z0-9]{4,6})\b`)

type uiSessions struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

func newUISessions() *uiSessions { return &uiSessions{tokens: map[string]time.Time{}} }

func (s *uiSessions) issue() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for existing, expiry := range s.tokens {
		if now.After(expiry) {
			delete(s.tokens, existing)
		}
	}
	s.tokens[token] = now.Add(uiSessionTTL)
	return token, nil
}

func (s *uiSessions) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.tokens[token]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.tokens, token)
		return false
	}
	return true
}

func (s *uiSessions) revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}

// deviceLogin tracks the single in-flight `codex login --device-auth` process.
// Only one may run at a time: concurrent logins would race to write auth.json.
type deviceLogin struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	running  bool
	UserCode string
	URL      string
	Message  string
	Failed   bool
	Account  string
	started  time.Time
	// onDone runs after the CLI exits successfully and decides the outcome:
	// the login lands in a staging directory and only becomes an account here.
	onDone func() (message string, failed bool)
}

func (d *deviceLogin) snapshot() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]any{
		"running":   d.running,
		"user_code": d.UserCode,
		"url":       d.URL,
		"message":   d.Message,
		"failed":    d.Failed,
		"account":   d.Account,
	}
}

func (d *deviceLogin) cancel() {
	d.mu.Lock()
	cmd, running := d.cmd, d.running
	if running {
		d.Message, d.Failed = "Sign-in cancelled.", true
	}
	d.mu.Unlock()
	if running && cmd != nil && cmd.Process != nil {
		// The whole group, not just the direct child — see configureProcessGroup.
		_ = killProcessGroup(cmd.Process.Pid)
	}
}

// start launches the CLI and streams its output into the shared state until it
// exits. Returns an error only if the process could not be started at all.
func (d *deviceLogin) start(codexPath, codexHome, account string, onDone func() (string, bool)) error {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return errors.New("a sign-in is already in progress")
	}
	cmd := exec.Command(codexPath, "login", "--device-auth")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	configureProcessGroup(cmd)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		d.mu.Unlock()
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		d.mu.Unlock()
		return err
	}
	d.cmd, d.running, d.started, d.onDone = cmd, true, time.Now(), onDone
	d.UserCode, d.URL, d.Message, d.Failed, d.Account = "", "", "Starting sign-in…", false, account
	d.mu.Unlock()

	go d.consume(cmd, pipe)
	return nil
}

func (d *deviceLogin) consume(cmd *exec.Cmd, pipe io.ReadCloser) {
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 8192), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(ansiPattern.ReplaceAllString(scanner.Text(), ""))
		if line == "" {
			continue
		}
		d.mu.Lock()
		if match := userCodePattern.FindStringSubmatch(line); match != nil && d.UserCode == "" {
			d.UserCode = match[1]
			if d.URL == "" {
				d.URL = uiDeviceVerificationURL
			}
			// The page renders the code and URL itself, so the CLI's numbered
			// instruction line ("2. Enter this one-time code…") would only
			// repeat what is already on screen.
			d.Message = "Waiting for you to enter the code…"
		}
		if strings.Contains(line, "http") && d.URL == "" {
			if idx := strings.Index(line, "http"); idx >= 0 {
				d.URL = strings.Fields(line[idx:])[0]
			}
		}
		if strings.HasPrefix(strings.ToLower(line), "error") {
			d.Message, d.Failed = line, true
		} else if d.UserCode == "" {
			d.Message = line
		}
		d.mu.Unlock()
	}
	err := cmd.Wait()
	d.mu.Lock()
	failed, onDone := d.Failed, d.onDone
	d.mu.Unlock()
	message := ""
	if err == nil && !failed && onDone != nil {
		// Outside the lock: it writes files and reads the pool.
		message, failed = onDone()
	}
	d.mu.Lock()
	d.running = false
	if err != nil && !d.Failed {
		d.Failed, d.Message = true, "Sign-in did not complete: "+err.Error()
	} else if err == nil && !d.Failed {
		d.Failed, d.Message = failed, message
		if d.Message == "" {
			d.Message = "Signed in."
		}
	}
	d.mu.Unlock()
}

// uiEnabled reports whether the console may serve. Without an API key there is
// nothing to authenticate against, so it stays off rather than open.
func (s *server) uiEnabled() bool { return s.cfg.APIKey != "" }

func (s *server) uiAuthorized(r *http.Request) bool {
	cookie, err := r.Cookie(uiSessionCookie)
	if err != nil {
		return false
	}
	return s.sessions.valid(cookie.Value)
}

func (s *server) serveUI(w http.ResponseWriter, r *http.Request) {
	if !s.uiEnabled() {
		writeJSON(w, 503, map[string]any{
			"error": "The console is disabled because no API key is configured. " +
				"Set OPENAI_VIA_CODEX_API_KEY and restart.",
		})
		return
	}
	// The console must never be cached by an intermediary: it is authenticated.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")

	path := strings.TrimPrefix(r.URL.Path, "/ui")
	path = strings.TrimPrefix(path, "/")

	switch {
	case path == "" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, uiHTML)
	case path == "api/login" && r.Method == http.MethodPost:
		s.uiLogin(w, r)
	case path == "api/logout" && r.Method == http.MethodPost:
		s.uiLogout(w, r)
	case path == "api/status" && r.Method == http.MethodGet:
		s.uiGuard(w, r, s.uiStatus)
	case path == "api/signin/start" && r.Method == http.MethodPost:
		s.uiGuard(w, r, s.uiSignInStart)
	case path == "api/signin/cancel" && r.Method == http.MethodPost:
		s.uiGuard(w, r, s.uiSignInCancel)
	case path == "api/auth/paste" && r.Method == http.MethodPost:
		s.uiGuard(w, r, s.uiPasteAuth)
	case path == "api/accounts/enable" && r.Method == http.MethodPost:
		s.uiGuard(w, r, func(w http.ResponseWriter, r *http.Request) { s.uiSetEnabled(w, r, true) })
	case path == "api/accounts/disable" && r.Method == http.MethodPost:
		s.uiGuard(w, r, func(w http.ResponseWriter, r *http.Request) { s.uiSetEnabled(w, r, false) })
	case path == "api/accounts/remove" && r.Method == http.MethodPost:
		s.uiGuard(w, r, s.uiRemoveAccount)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) uiGuard(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request)) {
	if !s.uiAuthorized(r) {
		writeJSON(w, 401, map[string]any{"error": "Not signed in to the console."})
		return
	}
	next(w, r)
}

func (s *server) uiLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "Malformed request."})
		return
	}
	// Constant-time: the console key is the only thing protecting the account.
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Key)), []byte(s.cfg.APIKey)) != 1 {
		writeJSON(w, 401, map[string]any{"error": "Incorrect key."})
		return
	}
	token, err := s.sessions.issue()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "Could not start a session."})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     uiSessionCookie,
		Value:    token,
		Path:     "/ui",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(uiSessionTTL.Seconds()),
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) uiLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(uiSessionCookie); err == nil {
		s.sessions.revoke(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: uiSessionCookie, Value: "", Path: "/ui", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) uiStatus(w http.ResponseWriter, _ *http.Request) {
	pool := s.accountPool()
	accounts := pool.snapshot()
	ready := 0
	for _, acct := range accounts {
		if acct["signed_in"] == true && acct["enabled"] == true && acct["cooling_until"] == nil {
			ready++
		}
	}
	writeJSON(w, 200, map[string]any{
		"accounts":       accounts,
		"ready_accounts": ready,
		"auth_dir":       pool.root,
		"default_model":  s.cfg.Model,
		"codex_cli":      s.codexCLIPath() != "",
		"signin":         s.device.snapshot(),
	})
}

// codexCLIPath resolves the Codex CLI, which only the device path needs. An
// empty result means the console offers the paste fallback alone.
func (s *server) codexCLIPath() string {
	if override := os.Getenv("OPENAI_VIA_CODEX_CLI_PATH"); override != "" {
		if _, err := os.Stat(override); err == nil {
			return override
		}
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		return ""
	}
	return path
}

// uiSignInStart signs in a new account, or with {"account": id} signs an
// existing one in again. The CLI writes into a staging CODEX_HOME that the pool
// does not read, so a login only joins the pool once it is checked.
func (s *server) uiSignInStart(w http.ResponseWriter, r *http.Request) {
	cli := s.codexCLIPath()
	if cli == "" {
		writeJSON(w, 501, map[string]any{
			"error": "The Codex CLI is not installed in this image, so device sign-in is unavailable. " +
				"Paste an auth.json instead.",
		})
		return
	}
	var body struct {
		Account string `json:"account"`
	}
	if !decodeUIBody(w, r, &body) {
		return
	}
	pool := s.accountPool()
	target, status, err := s.signInTarget(body.Account)
	if err != nil {
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	staging, err := os.MkdirTemp(filepath.Join(pool.root, accountsDirName), ".signin-")
	if err != nil {
		if mkErr := os.MkdirAll(filepath.Join(pool.root, accountsDirName), 0700); mkErr == nil {
			staging, err = os.MkdirTemp(filepath.Join(pool.root, accountsDirName), ".signin-")
		}
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "Could not prepare a sign-in directory: " + err.Error()})
			return
		}
	}
	onDone := func() (string, bool) {
		defer func() { _ = os.RemoveAll(staging) }()
		content, err := os.ReadFile(filepath.Join(staging, "auth.json"))
		if err != nil {
			return "The sign-in finished but wrote no auth.json.", true
		}
		return s.adoptLogin(target, body.Account == "", content)
	}
	if err := s.device.start(cli, staging, target, onDone); err != nil {
		_ = os.RemoveAll(staging)
		writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	// Give the CLI a moment to print the code so the first status carries it.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snap := s.device.snapshot()
		if snap["user_code"] != "" || snap["failed"] == true || snap["running"] == false {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	writeJSON(w, 200, s.device.snapshot())
}

// signInTarget resolves which account a login is for: an existing one when
// named, otherwise a fresh id.
func (s *server) signInTarget(account string) (string, int, error) {
	pool := s.accountPool()
	if account == "" {
		id, err := pool.newAccountID()
		if err != nil {
			return "", 500, err
		}
		return id, 0, nil
	}
	pool.rescan()
	pool.mu.Lock()
	_, ok := pool.accounts[account]
	pool.mu.Unlock()
	if !ok {
		return "", 404, errUnknownAccount
	}
	return account, 0, nil
}

// adoptLogin writes a checked login into an account slot. A login already held
// by another account is refused when adding — two copies of one login rotate
// each other's refresh token until both stop working.
func (s *server) adoptLogin(target string, adding bool, content []byte) (string, bool) {
	if err := validateAuthJSON(content); err != nil {
		return err.Error(), true
	}
	pool := s.accountPool()
	if other := pool.findIdentity(content, target); other != "" {
		if adding {
			return fmt.Sprintf("That ChatGPT login is already account %s, so nothing was added. Sign that account in again instead.", other), true
		}
		return fmt.Sprintf("That ChatGPT login belongs to account %s, not %s; nothing was changed.", other, target), true
	}
	if err := pool.writeAccount(target, content); err != nil {
		return "Could not save the login: " + err.Error(), true
	}
	log.Printf("codex.account.signed_in account=%s added=%t", target, adding)
	if adding {
		return fmt.Sprintf("Signed in. Added as account %s.", target), false
	}
	return fmt.Sprintf("Signed in. Account %s updated.", target), false
}

func (s *server) uiSignInCancel(w http.ResponseWriter, _ *http.Request) {
	s.device.cancel()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// uiPasteAuth adds a pasted login as a new account, or with "account" replaces
// that account's login.
func (s *server) uiPasteAuth(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
		Account string `json:"account"`
	}
	if !decodeUIBody(w, r, &body) {
		return
	}
	if err := validateAuthJSON([]byte(body.Content)); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	target, status, err := s.signInTarget(body.Account)
	if err != nil {
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	message, failed := s.adoptLogin(target, body.Account == "", []byte(body.Content))
	if failed {
		writeJSON(w, 409, map[string]any{"error": message})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "account": target, "message": message})
}

func (s *server) uiSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	var body struct {
		Account string `json:"account"`
	}
	if !decodeUIBody(w, r, &body) {
		return
	}
	if err := s.accountPool().setEnabled(body.Account, enabled); err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) uiRemoveAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
	}
	if !decodeUIBody(w, r, &body) {
		return
	}
	if err := s.accountPool().remove(body.Account); err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func writeAccountError(w http.ResponseWriter, err error) {
	if errors.Is(err, errUnknownAccount) {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 500, map[string]any{"error": err.Error()})
}

// decodeUIBody reads a small JSON body; an empty body decodes as {}.
func decodeUIBody(w http.ResponseWriter, r *http.Request, into any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "Could not read the request."})
		return false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, into); err != nil {
		writeJSON(w, 400, map[string]any{"error": "Malformed request."})
		return false
	}
	return true
}

// validateAuthJSON rejects content the reader would later choke on, so a bad
// paste fails at the point of paste rather than on the next API call.
func validateAuthJSON(content []byte) error {
	if len(strings.TrimSpace(string(content))) == 0 {
		return errors.New("Paste the contents of auth.json.")
	}
	var parsed map[string]any
	if err := json.Unmarshal(content, &parsed); err != nil {
		return fmt.Errorf("That is not valid JSON: %w", err)
	}
	if mode, _ := parsed["auth_mode"].(string); mode != "chatgpt" {
		return fmt.Errorf("Expected auth_mode \"chatgpt\", got %q. Use a ChatGPT login, not an API key.", mode)
	}
	tokens, ok := parsed["tokens"].(map[string]any)
	if !ok {
		return errors.New("Missing the \"tokens\" object.")
	}
	if token, _ := tokens["access_token"].(string); token == "" {
		return errors.New("Missing tokens.access_token.")
	}
	if token, _ := tokens["refresh_token"].(string); token == "" {
		return errors.New("Missing tokens.refresh_token — without it the login expires within the hour and cannot renew.")
	}
	return nil
}
