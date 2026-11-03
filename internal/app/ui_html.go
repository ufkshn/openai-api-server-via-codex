package app

// The console is one self-contained page: no CDN, no build step, no external
// fetches. It is served to a single operator on an internal host, so the whole
// thing staying in one string is worth more than any asset pipeline.

const uiHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Codex Proxy — Console</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #f6f7f9; --panel: #fff; --ink: #16181d; --muted: #666c78;
    --line: #e2e5ea; --accent: #2f6feb; --ok: #1a7f45; --bad: #b3261e;
    --code-bg: #eef1f6;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #14161a; --panel: #1c1f25; --ink: #e8eaee; --muted: #9aa2b1;
      --line: #2b2f37; --accent: #6f9dff; --ok: #4cc38a; --bad: #ff6b60;
      --code-bg: #262a32;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: var(--bg); color: var(--ink);
    font: 15px/1.55 ui-sans-serif, -apple-system, "Segoe UI", Roboto, sans-serif;
    display: flex; justify-content: center; padding: 32px 16px;
  }
  main { width: 100%; max-width: 640px; }
  h1 { font-size: 20px; margin: 0 0 4px; }
  .sub { color: var(--muted); font-size: 13px; margin-bottom: 20px; }
  section {
    background: var(--panel); border: 1px solid var(--line);
    border-radius: 10px; padding: 18px; margin-bottom: 14px;
  }
  h2 { font-size: 14px; margin: 0 0 12px; text-transform: uppercase;
       letter-spacing: .06em; color: var(--muted); }
  label { display: block; font-size: 13px; color: var(--muted); margin-bottom: 6px; }
  input, textarea {
    width: 100%; padding: 9px 11px; border: 1px solid var(--line);
    border-radius: 7px; background: var(--bg); color: var(--ink);
    font: inherit; margin-bottom: 10px;
  }
  textarea { min-height: 130px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
             font-size: 12.5px; resize: vertical; }
  button {
    padding: 9px 15px; border: 1px solid var(--line); border-radius: 7px;
    background: var(--accent); color: #fff; font: inherit; font-weight: 600;
    cursor: pointer; margin-right: 8px;
  }
  button.ghost { background: transparent; color: var(--ink); font-weight: 500; }
  button:disabled { opacity: .5; cursor: not-allowed; }
  .row { display: flex; justify-content: space-between; gap: 12px;
         padding: 7px 0; border-bottom: 1px solid var(--line); font-size: 14px; }
  .row:last-child { border-bottom: 0; }
  .row span:first-child { color: var(--muted); }
  .row span:last-child { text-align: right; word-break: break-all; }
  .ok { color: var(--ok); font-weight: 600; }
  .bad { color: var(--bad); font-weight: 600; }
  .code {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 27px; letter-spacing: .13em; background: var(--code-bg);
    padding: 14px; border-radius: 8px; text-align: center; margin: 10px 0;
    user-select: all;
  }
  .note { font-size: 13px; color: var(--muted); margin: 8px 0 0; }
  .msg { padding: 10px 12px; border-radius: 7px; margin-bottom: 12px;
         font-size: 13.5px; display: none; }
  .msg.show { display: block; }
  .msg.err { background: color-mix(in srgb, var(--bad) 12%, transparent); color: var(--bad); }
  .msg.good { background: color-mix(in srgb, var(--ok) 14%, transparent); color: var(--ok); }
  a { color: var(--accent); }
  .hide { display: none; }
  .acct { border: 1px solid var(--line); border-radius: 8px; padding: 12px 14px; margin-bottom: 10px; }
  .acct.off { border-style: dashed; }
  .acct.off > :not(.acct-actions) { opacity: .55; }
  .acct-head { display: flex; justify-content: space-between; align-items: baseline; gap: 10px; flex-wrap: wrap; }
  .acct-name { font-weight: 600; word-break: break-all; }
  .acct-meta { color: var(--muted); font-size: 12.5px; margin-top: 2px; }
  .pill { font-size: 12px; font-weight: 600; padding: 2px 9px; border-radius: 99px; white-space: nowrap; }
  .pill.ok { background: color-mix(in srgb, var(--ok) 15%, transparent); }
  .pill.bad { background: color-mix(in srgb, var(--bad) 13%, transparent); }
  .pill.warn { color: #a15c00; background: color-mix(in srgb, #e69500 18%, transparent); }
  .pill.muted { color: var(--muted); background: var(--code-bg); }
  .meter { display: grid; grid-template-columns: 92px 1fr 46px; gap: 8px; align-items: center;
           font-size: 12.5px; color: var(--muted); margin-top: 7px; }
  .bar { height: 6px; border-radius: 3px; background: var(--code-bg); overflow: hidden; }
  .bar i { display: block; height: 100%; background: var(--accent); }
  .bar i.hot { background: var(--bad); }
  .acct-actions { margin-top: 10px; }
  .acct-actions button { padding: 5px 11px; font-size: 13px; margin-top: 4px; }
  button.enable { background: color-mix(in srgb, var(--ok) 18%, transparent); border-color: color-mix(in srgb, var(--ok) 60%, var(--line)); color: var(--ok); }
  button.enable::before { content: "\25B6"; font-size: 9px; margin-right: 6px; vertical-align: 1px; }
  button.enable:hover { background: color-mix(in srgb, var(--ok) 28%, transparent); }
  button.danger { background: transparent; color: var(--bad); border-color: color-mix(in srgb, var(--bad) 40%, var(--line)); font-weight: 500; }
  button.danger.solid { background: var(--bad); border-color: var(--bad); color: #fff; font-weight: 600; }
  .modal { position: fixed; inset: 0; display: flex; align-items: center; justify-content: center;
           padding: 16px; background: rgba(8, 10, 14, .55); z-index: 10; }
  .modal.hide { display: none; }
  .modal-box { background: var(--panel); border: 1px solid var(--line); border-radius: 10px;
               max-width: 420px; width: 100%; padding: 20px 22px; box-shadow: 0 18px 50px rgba(0,0,0,.35); }
  .modal-box h3 { margin: 0 0 8px; font-size: 16px; }
  .modal-box p { margin: 0 0 6px; color: var(--muted); font-size: 13.5px; line-height: 1.5; }
  .modal-box strong { color: var(--ink); word-break: break-all; }
  .modal-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 16px; }
  select { padding: 8px 10px; border: 1px solid var(--line); border-radius: 7px;
           background: var(--bg); color: var(--ink); font: inherit; margin-bottom: 10px; width: 100%; }
  .summary { font-size: 13.5px; color: var(--muted); margin-bottom: 12px; }
</style>
</head>
<body>
<main>
  <h1>Codex Proxy</h1>
  <div class="sub">Operator console — the ChatGPT accounts this proxy spreads its requests across.</div>

  <div id="msg" class="msg"></div>

  <section id="gate">
    <h2>Console access</h2>
    <label for="key">API key</label>
    <input id="key" type="password" autocomplete="current-password" placeholder="OPENAI_VIA_CODEX_API_KEY">
    <button id="unlock">Unlock</button>
  </section>

  <div id="app" class="hide">
    <section>
      <h2>Accounts</h2>
      <div class="summary" id="summary"></div>
      <div id="accounts"></div>
      <div style="margin-top:4px">
        <button class="ghost" id="refresh">Refresh</button>
        <button class="ghost" id="lock">Lock console</button>
      </div>
    </section>

    <section>
      <h2 id="device-title">Add an account with a device code</h2>
      <div id="device-idle">
        <p class="note">Starts a sign-in and opens the verification page in a new tab.
          Copy the one-time code, enter it there while signed in to the ChatGPT account
          you want to add, and the proxy starts using it.</p>
        <button id="start">Add account</button>
      </div>
      <div id="device-active" class="hide">
        <div class="code" id="user-code">…</div>
        <button id="copy">Copy code</button>
        <p class="note">Enter it on <a id="verify-url" href="#" target="_blank" rel="noreferrer noopener">the verification page</a>
          — opened in a new tab when sign-in started. It expires in about 15 minutes.</p>
        <p class="note" id="device-msg"></p>
        <button class="ghost" id="cancel">Cancel</button>
      </div>
    </section>

    <section>
      <h2>Paste an auth.json</h2>
      <p class="note" style="margin-top:0">Fallback for when device sign-in is unavailable.
        Run <code>codex login</code> elsewhere, then paste the resulting
        <code>~/.codex/auth.json</code> here. Use a fresh login: a copy of a file
        another process is still using breaks both once either refreshes.</p>
      <label for="paste-target">Save as</label>
      <select id="paste-target"><option value="">A new account</option></select>
      <textarea id="authjson" spellcheck="false" placeholder='{"auth_mode":"chatgpt","tokens":{...}}'></textarea>
      <button id="save">Save auth.json</button>
    </section>

    <section>
      <h2>Proxy</h2>
      <div id="status"></div>
    </section>
  </div>
</main>

<script>
(function () {
  var $ = function (id) { return document.getElementById(id); };
  var poll = null;

  var VERIFY_URL = 'https://auth.openai.com/codex/device';

  function selectText(el) {
    var range = document.createRange();
    range.selectNodeContents(el);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }

  // navigator.clipboard exists only in a secure context. The console is normally
  // reached over plain HTTP on a private address, where it is undefined — so the
  // execCommand path is the one that actually runs in production, not the fallback.
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      var ta = document.createElement('textarea');
      ta.value = text;
      // Keep it off-screen but focusable; display:none would not be selectable.
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.top = '-1000px';
      document.body.appendChild(ta);
      ta.select();
      var ok = false;
      try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
      document.body.removeChild(ta);
      ok ? resolve() : reject(new Error('copy unavailable'));
    });
  }

  function show(text, kind) {
    var box = $('msg');
    box.textContent = text;
    box.className = 'msg show ' + (kind || 'err');
    if (kind === 'good') { setTimeout(function () { box.className = 'msg'; }, 4000); }
  }

  function api(path, body) {
    return fetch('/ui/api/' + path, {
      method: body === undefined ? 'GET' : 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin'
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (data) {
        if (!r.ok) { throw new Error(data.error || ('Request failed (' + r.status + ')')); }
        return data;
      });
    });
  }

  function row(label, value, cls) {
    return '<div class="row"><span>' + label + '</span><span' +
      (cls ? ' class="' + cls + '"' : '') + '>' + value + '</span></div>';
  }

  function esc(v) {
    return String(v === undefined || v === null ? '—' : v).replace(/[&<>"]/g, function (c) {
      return ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c];
    });
  }

  function pct(v) { return Math.round(v * 10) / 10; }

  function until(iso) {
    if (!iso) { return ''; }
    var secs = Math.max(0, Math.round((new Date(iso) - Date.now()) / 1000));
    if (secs < 90) { return secs + 's'; }
    if (secs < 5400) { return Math.round(secs / 60) + ' min'; }
    if (secs < 172800) { return Math.round(secs / 3600) + ' h'; }
    return Math.round(secs / 86400) + ' d';
  }

  function meter(label, percent, resets) {
    if (percent === undefined) { return ''; }
    var width = Math.max(0, Math.min(100, percent));
    return '<div class="meter"><span>' + label + '</span><div class="bar"><i' +
      (width >= 90 ? ' class="hot"' : '') + ' style="width:' + width + '%"></i></div><span>' +
      pct(percent) + '%</span></div>' +
      (resets ? '<div class="acct-meta" style="margin-left:100px">resets in ' + until(resets) + '</div>' : '');
  }

  function accountState(a) {
    if (!a.enabled) { return ['Disabled', 'muted']; }
    if (!a.signed_in) { return ['Signed out', 'bad']; }
    if (a.cooling_until) {
      var why = a.cooling_reason === 'usage_limit' ? 'Usage limit' :
        a.cooling_reason === 'unauthorized' ? 'Rejected' : 'Rate limited';
      return [why + ' · ' + until(a.cooling_until), 'warn'];
    }
    return ['Active', 'ok'];
  }

  function renderAccount(a) {
    var state = accountState(a);
    var name = a.email || a.account_id || a.id;
    var meta = ['id ' + a.id];
    if (a.plan) { meta.push(a.plan); }
    meta.push(a.in_flight + ' in flight', a.served + ' requests');
    if (a.expires_at) { meta.push('token ' + until(a.expires_at)); }
    var html = '<div class="acct' + (a.enabled ? '' : ' off') + '">';
    html += '<div class="acct-head"><span class="acct-name">' + esc(name) + '</span>' +
      '<span class="pill ' + state[1] + '">' + esc(state[0]) + '</span></div>';
    html += '<div class="acct-meta">' + esc(meta.join(' · ')) + '</div>';
    html += meter('5h window', a.primary_used_percent, a.primary_resets_at);
    html += meter('Weekly', a.secondary_used_percent, a.secondary_resets_at);
    var problem = !a.signed_in ? a.error : a.last_error;
    if (problem) { html += '<div class="acct-meta bad" style="margin-top:6px">' + esc(problem) + '</div>'; }
    html += '<div class="acct-actions">' +
      (a.enabled
        ? '<button class="ghost" data-act="disable" data-id="' + esc(a.id) + '">Disable</button>'
        : '<button class="enable" data-act="enable" data-id="' + esc(a.id) + '">Enable</button>') +
      '<button class="ghost" data-act="resign" data-id="' + esc(a.id) + '">Sign in again</button>' +
      '<button class="danger" data-act="remove" data-id="' + esc(a.id) + '" data-name="' + esc(name) + '">Remove</button>' +
      '</div></div>';
    return html;
  }

  function renderStatus(s) {
    var accounts = s.accounts || [];
    $('summary').textContent = accounts.length === 0
      ? 'No account yet — add one below. The proxy rejects every request until then.'
      : s.ready_accounts + ' of ' + accounts.length + ' account' + (accounts.length === 1 ? '' : 's') +
        ' ready. Requests go to the least busy ready account.';
    $('accounts').innerHTML = accounts.map(renderAccount).join('');

    var target = $('paste-target'), chosen = target.value;
    target.innerHTML = '<option value="">A new account</option>' + accounts.map(function (a) {
      return '<option value="' + esc(a.id) + '">Replace ' + esc(a.email || a.id) + '</option>';
    }).join('');
    target.value = chosen;
    if (target.value !== chosen) { target.value = ''; }

    var html = '';
    html += row('Auth directory', esc(s.auth_dir));
    html += row('Default model', esc(s.default_model));
    html += row('Codex CLI', s.codex_cli ? 'available' : 'not installed',
      s.codex_cli ? '' : 'bad');
    $('status').innerHTML = html;

    var d = s.signin || {};
    // Only a live sign-in shows the code panel. Once the CLI stops — completed,
    // cancelled or failed — the code is dead, so keying off user_code alone
    // would leave an unusable code on screen with no way back to the start.
    if (d.running) {
      $('device-idle').className = 'hide';
      $('device-active').className = '';
      $('user-code').textContent = d.user_code || '…';
      $('verify-url').href = d.url || VERIFY_URL;
      $('device-msg').textContent = d.message || '';
    } else {
      $('device-idle').className = '';
      $('device-active').className = 'hide';
    }
    // Surface how a finished attempt ended, once, since the panel it happened
    // in has just been replaced by the idle one.
    if (!d.running && poll && d.message) { show(d.message, d.failed ? 'err' : 'good'); }
    // Stop polling once the login settles, so an idle console is quiet.
    if (!d.running && poll) { clearInterval(poll); poll = null; }
    if (!s.codex_cli) { $('start').disabled = true; }
  }

  function refresh() {
    return api('status').then(renderStatus).catch(function (e) {
      if (/not signed in/i.test(e.message)) { lock(); } else { show(e.message); }
    });
  }

  function unlock() {
    $('gate').className = 'hide';
    $('app').className = '';
    refresh();
  }

  function lock() {
    $('gate').className = '';
    $('app').className = 'hide';
    if (poll) { clearInterval(poll); poll = null; }
  }

  $('unlock').onclick = function () {
    api('login', { key: $('key').value }).then(function () {
      $('key').value = '';
      $('msg').className = 'msg';
      unlock();
    }).catch(function (e) { show(e.message); });
  };

  $('lock').onclick = function () {
    api('logout', {}).then(lock).catch(function (e) { show(e.message); });
  };

  $('refresh').onclick = refresh;

  function startSignIn(account) {
    $('start').disabled = true;
    // Opened synchronously inside the click handler: the code arrives later, over
    // the poll, and a window.open() from that callback is outside the user-gesture
    // window and gets swallowed by popup blockers. The URL is a constant, so there
    // is nothing to wait for anyway.
    var tab = window.open(VERIFY_URL, '_blank', 'noopener');
    api('signin/start', { account: account || '' }).then(function () {
      if (poll) { clearInterval(poll); }
      poll = setInterval(refresh, 3000);
      $('device-active').scrollIntoView({ behavior: 'smooth', block: 'center' });
      return refresh();
    }).catch(function (e) {
      // Nothing will be entered on that page now — don't leave it stranded.
      if (tab) { try { tab.close(); } catch (_) {} }
      show(e.message);
    }).then(function () { $('start').disabled = false; });
  }

  $('start').onclick = function () { startSignIn(''); };

  $('accounts').onclick = function (e) {
    var btn = e.target.closest('button[data-act]');
    if (!btn) { return; }
    var id = btn.getAttribute('data-id'), act = btn.getAttribute('data-act');
    if (act === 'resign') { startSignIn(id); return; }
    if (act === 'remove') {
      askRemove(btn.getAttribute('data-name')).then(function (yes) { if (yes) { runAction(btn, id, act); } });
      return;
    }
    runAction(btn, id, act);
  };

  // An in-page dialog rather than confirm(): embedded browsers may suppress
  // native dialogs, which would make Remove silently do nothing.
  function askRemove(name) {
    $('confirm-name').textContent = name;
    $('confirm').classList.remove('hide');
    $('confirm-cancel').focus();
    return new Promise(function (resolve) {
      function done(yes) {
        $('confirm').classList.add('hide');
        $('confirm-ok').onclick = $('confirm-cancel').onclick = $('confirm').onclick = null;
        document.removeEventListener('keydown', onKey);
        resolve(yes);
      }
      function onKey(e) { if (e.key === 'Escape') { done(false); } }
      $('confirm-ok').onclick = function () { done(true); };
      $('confirm-cancel').onclick = function () { done(false); };
      $('confirm').onclick = function (e) { if (e.target === $('confirm')) { done(false); } };
      document.addEventListener('keydown', onKey);
    });
  }

  function runAction(btn, id, act) {
    btn.disabled = true;
    api('accounts/' + act, { account: id }).then(function () {
      show({ enable: 'Account enabled.', disable: 'Account disabled — it gets no new requests.',
        remove: 'Account removed.' }[act], 'good');
      return refresh();
    }).catch(function (e) { show(e.message); btn.disabled = false; });
  }

  $('copy').onclick = function () {
    var code = $('user-code').textContent.trim();
    if (!code || code === '…') { return; }
    copyText(code).then(function () {
      show('Code copied.', 'good');
    }).catch(function () {
      // Leave it selected so ⌘C / Ctrl-C still works.
      selectText($('user-code'));
      show('Could not copy automatically — the code is selected, press Ctrl/Cmd+C.');
    });
  };

  $('cancel').onclick = function () {
    api('signin/cancel', {}).then(refresh).catch(function (e) { show(e.message); });
  };

  $('save').onclick = function () {
    api('auth/paste', { content: $('authjson').value, account: $('paste-target').value }).then(function (r) {
      $('authjson').value = '';
      show(r.message || 'Saved.', 'good');
      return refresh();
    }).catch(function (e) { show(e.message); });
  };

  $('key').addEventListener('keydown', function (e) {
    if (e.key === 'Enter') { $('unlock').click(); }
  });

  // A live session cookie means the gate can be skipped on reload.
  api('status').then(function (s) { unlock(); renderStatus(s); }).catch(function () { lock(); });
})();
</script>
<div id="confirm" class="modal hide" role="dialog" aria-modal="true" aria-labelledby="confirm-title">
  <div class="modal-box">
    <h3 id="confirm-title">Remove account?</h3>
    <p><strong id="confirm-name"></strong> stops receiving requests and its login is deleted from the proxy.</p>
    <p>Adding it back needs a new sign-in.</p>
    <div class="modal-actions">
      <button class="ghost" id="confirm-cancel">Cancel</button>
      <button class="danger solid" id="confirm-ok">Remove account</button>
    </div>
  </div>
</div>
</body>
</html>
`
