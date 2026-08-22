'use strict';

// Token lives only in this browser's localStorage, scoped to this
// extension's own origin — the proxy in server.js never sees it except
// as a pass-through header on each request. See server.js's doc comment.
const TOKEN_KEY = 'amh_control_plane_token';

function getToken() {
  return localStorage.getItem(TOKEN_KEY) || '';
}

function setToken(value) {
  if (value) localStorage.setItem(TOKEN_KEY, value);
  else localStorage.removeItem(TOKEN_KEY);
}

async function api(path, opts = {}) {
  const headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
  const token = getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const resp = await fetch('/api' + path, Object.assign({}, opts, { headers }));
  let body = null;
  const text = await resp.text();
  try { body = text ? JSON.parse(text) : null; } catch { body = text; }
  if (!resp.ok) {
    const message = (body && body.error) ? body.error : (resp.status + ' ' + resp.statusText);
    throw new Error(message);
  }
  return body;
}

function toast(message, kind) {
  const el = document.createElement('div');
  el.className = 'toast-item ' + (kind || 'ok');
  el.textContent = message;
  document.getElementById('toast').appendChild(el);
  setTimeout(() => el.remove(), 4000);
}

function badge(status) {
  const span = document.createElement('span');
  span.className = 'badge ' + (status || '');
  span.textContent = status || '';
  return span;
}

// ── Tabs ─────────────────────────────────────────────────────────────────

function initTabs() {
  document.querySelectorAll('#tabs button').forEach((btn) => {
    btn.addEventListener('click', () => {
      document.querySelectorAll('#tabs button').forEach((b) => b.classList.remove('active'));
      document.querySelectorAll('.tab').forEach((t) => t.classList.remove('active'));
      btn.classList.add('active');
      document.getElementById('tab-' + btn.dataset.tab).classList.add('active');
      refreshCurrentTab();
    });
  });
}

function currentTab() {
  return document.querySelector('#tabs button.active').dataset.tab;
}

function refreshCurrentTab() {
  const tab = currentTab();
  if (tab === 'extensions') loadExtensions();
  if (tab === 'computers') loadComputers();
  if (tab === 'connectors') loadConnectors();
  if (tab === 'accounts') loadAccounts();
}

// ── Token bar ────────────────────────────────────────────────────────────

function initTokenBar() {
  const input = document.getElementById('token-input');
  const status = document.getElementById('token-status');
  input.value = getToken();
  status.textContent = input.value ? 'token set' : 'no token';
  input.addEventListener('change', () => {
    setToken(input.value.trim());
    status.textContent = input.value ? 'token set' : 'no token';
    refreshCurrentTab();
  });
}

// ── Extensions ───────────────────────────────────────────────────────────

async function loadExtensions() {
  const container = document.getElementById('extensions-table');
  try {
    const list = await api('/v1/extensions');
    if (!list || !list.length) { container.innerHTML = '<div class="empty">No extensions installed yet.</div>'; return; }
    const table = document.createElement('table');
    table.innerHTML = '<thead><tr><th>ID</th><th>Version</th><th>Isolation</th><th>Status</th><th></th></tr></thead>';
    const tbody = document.createElement('tbody');
    list.forEach((e) => {
      const tr = document.createElement('tr');
      const actions = document.createElement('td');
      actions.className = 'row-actions';
      addAction(actions, 'Activate', () => activateExtension(e.id, e.version), e.status !== 'discovered' && e.status !== 'disposed' && e.status !== 'failed');
      addAction(actions, 'Quiesce', () => quiesceExtension(e.id, e.version), e.status !== 'active');
      addAction(actions, 'Dispose', () => disposeExtension(e.id, e.version), e.status !== 'quiescing');
      tr.innerHTML = `<td>${escapeHtml(e.id)}</td><td>${escapeHtml(e.version)}</td><td>${escapeHtml(e.isolation)}</td><td></td>`;
      tr.children[3].appendChild(badge(e.status));
      tr.appendChild(actions);
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    container.innerHTML = '';
    container.appendChild(table);
  } catch (err) {
    container.innerHTML = `<div class="empty">Could not load extensions: ${escapeHtml(err.message)}</div>`;
  }
}

function addAction(container, label, fn, disabled) {
  const btn = document.createElement('button');
  btn.textContent = label;
  btn.disabled = !!disabled;
  btn.addEventListener('click', async () => {
    try { await fn(); toast(label + ' succeeded'); refreshCurrentTab(); }
    catch (err) { toast(label + ' failed: ' + err.message, 'error'); }
  });
  container.appendChild(btn);
}

function activateExtension(id, version) {
  return api('/v1/extensions/activate', { method: 'POST', body: JSON.stringify({ id, version }) });
}
function quiesceExtension(id, version) {
  return api('/v1/extensions/quiesce', { method: 'POST', body: JSON.stringify({ id, version }) });
}
function disposeExtension(id, version) {
  return api('/v1/extensions/dispose', { method: 'POST', body: JSON.stringify({ id, version }) });
}

document.addEventListener('DOMContentLoaded', () => {
  document.getElementById('form-discover-extension').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const raw = document.getElementById('extension-manifest').value;
    let manifest;
    try { manifest = JSON.parse(raw); } catch { toast('Manifest is not valid JSON', 'error'); return; }
    try {
      await api('/v1/extensions', { method: 'POST', body: JSON.stringify(manifest) });
      toast('Extension discovered');
      loadExtensions();
    } catch (err) { toast('Discover failed: ' + err.message, 'error'); }
  });
});

// ── Computers ────────────────────────────────────────────────────────────

async function loadComputers() {
  const container = document.getElementById('computers-table');
  const agentId = document.getElementById('computers-agent-filter').value.trim();
  if (!agentId) { container.innerHTML = '<div class="empty">Enter an agent ID above to list its computers.</div>'; return; }
  try {
    const list = await api('/v1/computers?agent_id=' + encodeURIComponent(agentId));
    if (!list || !list.length) { container.innerHTML = '<div class="empty">No computers for this agent.</div>'; return; }
    const table = document.createElement('table');
    table.innerHTML = '<thead><tr><th>ID</th><th>Isolation</th><th>Image</th><th>Status</th><th></th></tr></thead>';
    const tbody = document.createElement('tbody');
    list.forEach((c) => {
      const tr = document.createElement('tr');
      tr.innerHTML = `<td>${escapeHtml(c.id)}</td><td>${escapeHtml(c.isolation)}</td><td>${escapeHtml(c.image)}</td><td></td>`;
      tr.children[3].appendChild(badge(c.status));
      const actions = document.createElement('td');
      actions.className = 'row-actions';
      addAction(actions, 'Destroy', () => destroyComputer(c.id), c.status === 'destroyed');
      tr.appendChild(actions);
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    container.innerHTML = '';
    container.appendChild(table);
  } catch (err) {
    container.innerHTML = `<div class="empty">Could not load computers: ${escapeHtml(err.message)}</div>`;
  }
}

function destroyComputer(id) {
  return api('/v1/computers/' + encodeURIComponent(id) + '/destroy', { method: 'POST', body: JSON.stringify({ reason: 'destroyed from control plane UI' }) });
}

document.addEventListener('DOMContentLoaded', () => {
  document.getElementById('form-create-computer').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const form = new FormData(ev.target);
    try {
      await api('/v1/computers', {
        method: 'POST',
        body: JSON.stringify({ agent_id: form.get('agent_id'), isolation: form.get('isolation'), image: form.get('image') }),
      });
      toast('Computer created');
      document.getElementById('computers-agent-filter').value = form.get('agent_id');
      loadComputers();
    } catch (err) { toast('Create failed: ' + err.message, 'error'); }
  });
  document.getElementById('computers-agent-filter').addEventListener('change', loadComputers);
});

// ── Connectors ───────────────────────────────────────────────────────────

async function loadConnectors() {
  const container = document.getElementById('connectors-table');
  try {
    const list = await api('/v1/connectors');
    if (!list || !list.length) { container.innerHTML = '<div class="empty">No connectors configured yet.</div>'; return; }
    const table = document.createElement('table');
    table.innerHTML = '<thead><tr><th>ID</th><th>Type</th><th>Auth</th><th>Status</th><th></th></tr></thead>';
    const tbody = document.createElement('tbody');
    list.forEach((c) => {
      const tr = document.createElement('tr');
      tr.innerHTML = `<td>${escapeHtml(c.id)}</td><td>${escapeHtml(c.type)}</td><td>${escapeHtml(c.auth)}</td><td></td>`;
      tr.children[3].appendChild(badge(c.status));
      const actions = document.createElement('td');
      actions.className = 'row-actions';
      addAction(actions, 'Disable', () => disableConnector(c.id), c.status === 'disabled');
      tr.appendChild(actions);
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    container.innerHTML = '';
    container.appendChild(table);
  } catch (err) {
    container.innerHTML = `<div class="empty">Could not load connectors: ${escapeHtml(err.message)}</div>`;
  }
}

function disableConnector(id) {
  return api('/v1/connectors/' + encodeURIComponent(id) + '/disable', { method: 'POST' });
}

document.addEventListener('DOMContentLoaded', () => {
  document.getElementById('form-create-connector').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const form = new FormData(ev.target);
    let config;
    const raw = (form.get('config') || '').trim();
    if (raw) {
      try { config = JSON.parse(raw); } catch { toast('Config is not valid JSON', 'error'); return; }
    }
    try {
      await api('/v1/connectors', {
        method: 'POST',
        body: JSON.stringify({ id: form.get('id'), type: form.get('type'), auth: form.get('auth'), config }),
      });
      toast('Connector added');
      loadConnectors();
    } catch (err) { toast('Add failed: ' + err.message, 'error'); }
  });
});

// ── Accounts ─────────────────────────────────────────────────────────────

// Provider-name presets for the "Register an account" form — purely a
// convenience so the free-text Provider field (which agents match against
// via their own ADAPTER_PROVIDER env var, see agents/context/llm.py) stays
// consistent across operators, not a fixed enum the daemon enforces.
const ACCOUNT_PROVIDER_PRESETS = {
  anthropic: 'anthropic',
  openai: 'openai',
  grok: 'grok',
  glm: 'glm',
  vllm: 'vllm',
  ollama: 'ollama',
};

// Model-provider credential presets — fill in the envelope's kind/base_url
// per daemon/inference/inference.go's verified per-vendor wiring (see that
// package's doc comment). base_url left blank uses that provider kind's
// built-in default (Anthropic direct, or none for openai_compatible, which
// requires one explicitly).
const CREDENTIAL_PRESETS = {
  anthropic: { kind: 'anthropic', baseUrl: '', apiKeyHint: 'sk-ant-...' },
  openai: { kind: 'openai_compatible', baseUrl: 'https://api.openai.com/v1', apiKeyHint: 'sk-...' },
  grok: { kind: 'openai_compatible', baseUrl: 'https://api.x.ai/v1', apiKeyHint: 'xai-... or an OAuth access token below' },
  'glm-anthropic': { kind: 'anthropic', baseUrl: 'https://api.z.ai/api/anthropic', apiKeyHint: 'your Z.ai Coding Plan API key' },
  'glm-openai': { kind: 'openai_compatible', baseUrl: 'https://api.z.ai/api/coding/paas/v4', apiKeyHint: 'your Z.ai Coding Plan API key' },
  vllm: { kind: 'openai_compatible', baseUrl: 'http://localhost:8000/v1', apiKeyHint: 'any non-empty placeholder — vLLM does not validate it locally' },
  ollama: { kind: 'openai_compatible', baseUrl: 'http://localhost:11434/v1', apiKeyHint: "'ollama' (or any placeholder) — Ollama does not validate it" },
};

async function loadAccounts() {
  const container = document.getElementById('accounts-table');
  try {
    const list = await api('/v1/accounts');
    if (!list || !list.length) { container.innerHTML = '<div class="empty">No accounts registered yet.</div>'; return; }
    const table = document.createElement('table');
    table.innerHTML = '<thead><tr><th>ID</th><th>Provider</th><th>Name</th><th>Status</th><th></th></tr></thead>';
    const tbody = document.createElement('tbody');
    list.forEach((a) => {
      const tr = document.createElement('tr');
      tr.innerHTML = `<td>${escapeHtml(a.id)}</td><td>${escapeHtml(a.provider)}</td><td>${escapeHtml(a.display_name || '')}</td><td></td>`;
      tr.children[3].appendChild(badge(a.status));
      const actions = document.createElement('td');
      actions.className = 'row-actions';
      const authBtn = document.createElement('button');
      authBtn.textContent = 'Authenticate…';
      authBtn.disabled = a.status === 'revoked';
      authBtn.addEventListener('click', () => openCredentialDialog(a));
      actions.appendChild(authBtn);
      addAction(actions, 'Revoke', () => revokeAccount(a.id), a.status === 'revoked');
      tr.appendChild(actions);
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    container.innerHTML = '';
    container.appendChild(table);
  } catch (err) {
    container.innerHTML = `<div class="empty">Could not load accounts: ${escapeHtml(err.message)}</div>`;
  }
}

function revokeAccount(id) {
  return api('/v1/accounts/' + encodeURIComponent(id) + '/revoke', { method: 'POST' });
}

document.addEventListener('DOMContentLoaded', () => {
  const accountPreset = document.getElementById('account-preset');
  accountPreset.addEventListener('change', () => {
    const provider = ACCOUNT_PROVIDER_PRESETS[accountPreset.value];
    if (provider) document.querySelector('#form-create-account [name="provider"]').value = provider;
  });

  document.getElementById('form-create-account').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const form = new FormData(ev.target);
    try {
      await api('/v1/accounts', {
        method: 'POST',
        body: JSON.stringify({ provider: form.get('provider'), display_name: form.get('display_name') }),
      });
      toast('Account registered — use Authenticate to store its credential');
      accountPreset.value = '';
      loadAccounts();
    } catch (err) { toast('Register failed: ' + err.message, 'error'); }
  });
});

// ── Credential dialog ───────────────────────────────────────────────────
//
// Two credential shapes travel through the same /v1/accounts/{id}/credential
// endpoint as an opaque {"secret": "<string>"} — the daemon never interprets
// it. For a connector or generic provider that string is just the secret
// itself. For a model-provider account it is instead the JSON-encoded
// credential envelope daemon/inference/inference.go parses
// ({kind, api_key, base_url, oauth}) — this dialog builds that JSON so an
// operator never hand-writes it.

let credentialDialogAccount = null;

function initCredentialDialog() {
  const dialog = document.getElementById('credential-dialog');
  const kindToggle = document.getElementById('credential-kind-toggle');
  const opaqueFields = document.getElementById('credential-opaque-fields');
  const modelFields = document.getElementById('credential-model-fields');
  const preset = document.getElementById('credential-preset');
  const modelKind = document.getElementById('credential-model-kind');
  const baseUrl = document.getElementById('credential-base-url');
  const apiKey = document.getElementById('credential-api-key');

  kindToggle.addEventListener('change', () => {
    const isModel = kindToggle.value === 'model';
    opaqueFields.classList.toggle('hidden', isModel);
    modelFields.classList.toggle('hidden', !isModel);
  });

  preset.addEventListener('change', () => {
    const p = CREDENTIAL_PRESETS[preset.value];
    if (!p) return;
    modelKind.value = p.kind;
    baseUrl.value = p.baseUrl;
    apiKey.placeholder = p.apiKeyHint;
  });

  document.getElementById('credential-cancel').addEventListener('click', () => dialog.close());

  document.getElementById('form-credential').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!credentialDialogAccount) return;
    let secret;
    if (kindToggle.value === 'opaque') {
      secret = document.getElementById('credential-opaque-secret').value;
      if (!secret) { toast('Secret cannot be empty', 'error'); return; }
    } else {
      secret = buildCredentialEnvelope();
      if (secret === null) return; // buildCredentialEnvelope already toasted the error
    }
    try {
      await api('/v1/accounts/' + encodeURIComponent(credentialDialogAccount.id) + '/credential', {
        method: 'POST',
        body: JSON.stringify({ secret }),
      });
      toast('Account authenticated');
      dialog.close();
      loadAccounts();
    } catch (err) {
      toast('Authenticate failed: ' + err.message, 'error');
    }
  });
}

function buildCredentialEnvelope() {
  const kind = document.getElementById('credential-model-kind').value;
  const apiKey = document.getElementById('credential-api-key').value.trim();
  const baseUrl = document.getElementById('credential-base-url').value.trim();
  const accessToken = document.getElementById('credential-oauth-access').value.trim();
  const refreshToken = document.getElementById('credential-oauth-refresh').value.trim();
  const expiresLocal = document.getElementById('credential-oauth-expires').value;
  const refreshUrl = document.getElementById('credential-oauth-refresh-url').value.trim();
  const clientId = document.getElementById('credential-oauth-client-id').value.trim();

  const envelope = { kind };
  if (apiKey) envelope.api_key = apiKey;
  if (baseUrl) envelope.base_url = baseUrl;
  if (accessToken) {
    if (!expiresLocal) { toast('OAuth access token needs an expiry so it can be refreshed on time', 'error'); return null; }
    envelope.oauth = {
      access_token: accessToken,
      refresh_token: refreshToken,
      expires_at: new Date(expiresLocal).toISOString(),
      refresh_url: refreshUrl,
      client_id: clientId,
    };
  }
  if (!envelope.api_key && !envelope.oauth) {
    toast('Provide either an API key or an OAuth access token', 'error');
    return null;
  }
  if (kind === 'openai_compatible' && !envelope.base_url) {
    toast('openai_compatible credentials require a base URL', 'error');
    return null;
  }
  return JSON.stringify(envelope);
}

function resetCredentialDialogFields() {
  document.getElementById('credential-kind-toggle').value = 'opaque';
  document.getElementById('credential-opaque-fields').classList.remove('hidden');
  document.getElementById('credential-model-fields').classList.add('hidden');
  document.getElementById('credential-opaque-secret').value = '';
  document.getElementById('credential-preset').value = '';
  document.getElementById('credential-model-kind').value = 'anthropic';
  document.getElementById('credential-api-key').value = '';
  document.getElementById('credential-api-key').placeholder = 'leave blank if using OAuth below';
  document.getElementById('credential-base-url').value = '';
  document.getElementById('credential-oauth-access').value = '';
  document.getElementById('credential-oauth-refresh').value = '';
  document.getElementById('credential-oauth-expires').value = '';
  document.getElementById('credential-oauth-refresh-url').value = '';
  document.getElementById('credential-oauth-client-id').value = '';
}

function openCredentialDialog(account) {
  credentialDialogAccount = account;
  resetCredentialDialogFields();
  document.getElementById('credential-dialog-title').textContent =
    'Authenticate ' + account.provider + ' (' + account.id + ')';
  document.getElementById('credential-dialog').showModal();
}

function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

document.addEventListener('DOMContentLoaded', () => {
  initTabs();
  initTokenBar();
  initCredentialDialog();
  refreshCurrentTab();
});
