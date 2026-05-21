'use strict';

// --- State ---

const TAB_STORAGE_KEY = 'webshare.activeTab';

const state = {
  tab: (() => {
    try {
      const saved = localStorage.getItem(TAB_STORAGE_KEY);
      return saved === 'users' || saved === 'sources' ? saved : 'sources';
    } catch (_) { return 'sources'; }
  })(),
  keys: [],
  upstreams: [],
  users: [],
  settings: null,
  proxy: { running: false, http_addr: '', socks_addr: '' },
  revealedPasswords: {}, // username -> {plaintext, timerId}
  listenerError: '',
};

// --- API helpers ---

async function api(method, path, body) {
  const opts = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(path, opts);
  if (r.status === 401) {
    location.href = '/login';
    throw new Error('unauthorized');
  }
  const text = await r.text();
  let data;
  try { data = text ? JSON.parse(text) : null; } catch (_) { data = text; }
  if (!r.ok) {
    const err = new Error((data && data.error) || ('HTTP ' + r.status));
    err.status = r.status;
    err.data = data;
    throw err;
  }
  return data;
}

const apiGET = (p) => api('GET', p);
const apiPOST = (p, b) => api('POST', p, b ?? {});
const apiPATCH = (p, b) => api('PATCH', p, b);
const apiPUT = (p, b) => api('PUT', p, b);
const apiDELETE = (p) => api('DELETE', p);

// --- Refresh ---

async function refreshAll() {
  try {
    const [keys, upstreams, users, settings, proxy] = await Promise.all([
      apiGET('/api/v1/keys').catch(() => []),
      apiGET('/api/v1/upstreams').catch(() => []),
      apiGET('/api/v1/users').catch(() => []),
      apiGET('/api/v1/settings').catch(() => null),
      apiGET('/api/v1/proxy/status').catch(() => ({ running: false })),
    ]);
    state.keys = keys || [];
    state.upstreams = upstreams || [];
    state.users = users || [];
    if (settings) state.settings = settings;
    state.proxy = proxy || { running: false };
    render();
  } catch (e) {
    // If the very first call returned 401 we already redirected.
    console.error('refresh failed', e);
  }
}

// --- Render ---

const $app = document.getElementById('app');

function render() {
  // Update tab active state
  document.querySelectorAll('#tabs .tab').forEach((b) => {
    b.classList.toggle('active', b.dataset.tab === state.tab);
  });
  if (state.tab === 'sources') renderSources();
  else renderUsers();
}

function renderSources() {
  $app.innerHTML = '';
  $app.appendChild(systemSection());
  $app.appendChild(webshareSection());
}

function systemSection() {
  const s = state.settings || {
    sync_interval_minutes: 60,
    http_listener_port: 8080,
    http_listener_bind: '127.0.0.1',
    socks5_listener_port: 1080,
    socks5_listener_bind: '127.0.0.1',
    proxy_enabled: false,
  };
  const section = el('section', {},
    el('h2', {},
      'System',
      el('span', { class: 'status-pill' },
        el('span', { class: 'status-dot ' + (state.proxy.running ? 'running' : 'stopped') }),
        ' ',
        state.proxy.running ? 'Running' : 'Stopped',
      ),
      el('span', { style: 'flex:1' }),
      el('button', {
        class: 'primary',
        onclick: () => state.proxy.running ? stopProxy() : startProxy(),
      }, state.proxy.running ? 'Stop proxy' : 'Start proxy'),
    ),
    card(
      el('div', { class: 'row' },
        field('Listen addr', selectEl(
          { onchange: (e) => { s.http_listener_bind = e.target.value; s.socks5_listener_bind = e.target.value; } },
          [
            { value: '127.0.0.1', label: '127.0.0.1' },
            { value: '0.0.0.0', label: '0.0.0.0' },
            { value: '[::1]', label: '[::1]' },
          ],
          s.http_listener_bind,
        )),
        field('SOCKS5 port', inputEl({
          type: 'number', value: s.socks5_listener_port, style: 'width:90px',
          oninput: (e) => { s.socks5_listener_port = parseInt(e.target.value || '0', 10); },
        })),
        field('HTTP port', inputEl({
          type: 'number', value: s.http_listener_port, style: 'width:90px',
          oninput: (e) => { s.http_listener_port = parseInt(e.target.value || '0', 10); },
        })),
        field('Sync (min)', inputEl({
          type: 'number', value: s.sync_interval_minutes, style: 'width:80px',
          oninput: (e) => { s.sync_interval_minutes = parseInt(e.target.value || '0', 10); },
        })),
        el('div', { class: 'grow' }),
        el('button', { class: 'primary', onclick: () => applySettings(s) }, 'Apply'),
      ),
      state.listenerError ? el('div', { class: 'banner error' }, state.listenerError) : null,
    ),
  );
  return section;
}

function webshareSection() {
  return el('section', {},
    el('h2', {},
      'Webshare',
      el('span', { style: 'flex:1' }),
      el('button', { class: 'icon', title: 'Add API key', onclick: () => openAddKeyModal() }, '+'),
    ),
    state.keys.length === 0
      ? el('div', { class: 'card empty' }, 'No API keys configured. Click + to add one.')
      : el('div', {}, ...state.keys.map(renderKeyCard)),
  );
}

function renderKeyCard(key) {
  const owned = state.upstreams.filter((u) => u.source_api_key_id === key.id);
  return el('div', { class: 'key-card' },
    el('div', { class: 'header' },
      el('span', { class: 'label' }, key.label),
      key.last_sync_error
        ? el('span', { class: 'err-dot', title: key.last_sync_error }, '●')
        : null,
      key.last_synced_at
        ? el('span', { class: 'timestamp' }, formatRelative(key.last_synced_at))
        : el('span', { class: 'timestamp' }, 'never synced'),
      el('button', { class: 'icon', title: 'Sync', onclick: () => syncKey(key.id) }, '↻'),
      el('button', { class: 'icon danger-icon', title: 'Delete', onclick: () => deleteKey(key.id) }, '✕'),
    ),
    owned.length === 0
      ? el('div', { class: 'empty', style: 'padding:8px' }, 'No upstreams synced yet')
      : renderUpstreams(owned),
  );
}

function renderUpstreams(rows) {
  return el('div', { class: 'upstreams' },
    el('div', { class: 'upstream-row head' },
      el('div', {}, 'Country'),
      el('div', {}, 'Display Name'),
      el('div', { class: 'col-host' }, 'Node Address'),
      el('div', {}, 'Alive'),
    ),
    ...rows.map((u) =>
      el('div', { class: 'upstream-row' },
        el('div', {}, u.country_code || '—'),
        el('div', {}, u.display_name),
        el('div', { class: 'mono col-host' }, `${u.host}:${u.port}`),
        u.alive
          ? el('div', { class: 'alive-yes' }, '✓')
          : el('div', { class: 'alive-no' }, '✗'),
      ),
    ),
  );
}

function renderUsers() {
  $app.innerHTML = '';
  const section = el('section', {},
    el('h2', {},
      'Users',
      el('span', { style: 'flex:1' }),
      el('button', { class: 'icon', title: 'Add user', onclick: () => openAddUserModal() }, '+'),
    ),
    state.users.length === 0
      ? el('div', { class: 'card empty' }, 'No users yet. Click + to add one.')
      : el('div', { class: 'card', style: 'padding:0' }, renderUsersTable()),
  );
  $app.appendChild(section);
}

function renderUsersTable() {
  const upstreamOptions = [{ value: '', label: '— (unmapped)' }]
    .concat(state.upstreams.map((u) => ({ value: u.id, label: u.display_name })));

  const tbody = el('tbody', {}, ...state.users.map((user, idx) => {
    const revealed = state.revealedPasswords[user.username];
    return el('tr', {},
      el('td', {}, user.username),
      el('td', {},
        selectEl(
          { onchange: (e) => setMapping(user.username, e.target.value || null) },
          upstreamOptions,
          user.upstream_proxy_id || '',
        ),
      ),
      el('td', {},
        el('div', { class: 'row', style: 'gap:6px' },
          revealed ? el('span', { class: 'mono' }, revealed.value) : el('span', { class: 'muted' }, '••••••'),
          el('button', {
            class: 'icon', title: revealed ? 'Hide' : 'Reveal',
            onclick: () => peekPassword(user.username),
          }, revealed ? '🙈' : '👁'),
        ),
      ),
      el('td', {},
        user.broken
          ? el('span', { class: 'broken', title: 'Mapping broken — upstream missing or stale' }, '⚠')
          : el('span', { class: 'ok' }, '✓'),
      ),
      el('td', { class: 'actions' },
        el('div', { class: 'action-group' },
          el('button', {
            class: 'icon', title: 'Move up',
            disabled: idx === 0 ? '' : null,
            onclick: () => moveUser(idx, -1),
          }, '↑'),
          el('button', {
            class: 'icon', title: 'Move down',
            disabled: idx === state.users.length - 1 ? '' : null,
            onclick: () => moveUser(idx, +1),
          }, '↓'),
          el('button', {
            class: 'icon danger-icon', title: 'Delete',
            onclick: () => deleteUser(user.username),
          }, '✕'),
        ),
      ),
    );
  }));

  return el('table', { class: 'user-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Username'),
      el('th', {}, 'Mapped Proxy'),
      el('th', {}, 'Password'),
      el('th', {}, 'Status'),
      el('th', { class: 'actions' }, ''),
    )),
    tbody,
  );
}

// --- Actions ---

async function applySettings(s) {
  try {
    state.listenerError = '';
    await apiPUT('/api/v1/settings', {
      sync_interval_minutes: s.sync_interval_minutes,
      http_listener_port: s.http_listener_port,
      http_listener_bind: s.http_listener_bind,
      socks5_listener_port: s.socks5_listener_port,
      socks5_listener_bind: s.socks5_listener_bind,
      proxy_enabled: s.proxy_enabled,
    });
  } catch (e) {
    state.listenerError = e.message;
  }
  await refreshAll();
}

async function startProxy() {
  try {
    state.listenerError = '';
    await apiPOST('/api/v1/proxy/start');
  } catch (e) {
    state.listenerError = e.message;
  }
  await refreshAll();
}

async function stopProxy() {
  try {
    state.listenerError = '';
    await apiPOST('/api/v1/proxy/stop');
  } catch (e) {
    state.listenerError = e.message;
  }
  await refreshAll();
}

async function syncKey(id) {
  try { await apiPOST(`/api/v1/keys/${id}/sync`); }
  catch (e) { alert('Sync failed: ' + e.message); }
  await refreshAll();
}

async function deleteKey(id) {
  if (!confirm('Delete this API key? Synced upstreams will be removed.')) return;
  try { await apiDELETE(`/api/v1/keys/${id}`); }
  catch (e) {
    if (e.status === 409 && e.data && e.data.referencing_users) {
      const lines = e.data.referencing_users
        .map((r) => `• ${r.username} → ${r.display_name}`).join('\n');
      alert('Key is in use by:\n' + lines);
    } else {
      alert('Delete failed: ' + e.message);
    }
  }
  await refreshAll();
}

async function setMapping(username, upstreamId) {
  try { await apiPATCH(`/api/v1/users/${encodeURIComponent(username)}`, { upstream_proxy_id: upstreamId }); }
  catch (e) { alert('Set mapping failed: ' + e.message); }
  await refreshAll();
}

async function peekPassword(username) {
  if (state.revealedPasswords[username]) {
    clearTimeout(state.revealedPasswords[username].timerId);
    delete state.revealedPasswords[username];
    render();
    return;
  }
  try {
    const r = await apiGET(`/api/v1/users/${encodeURIComponent(username)}/password`);
    const timerId = setTimeout(() => {
      delete state.revealedPasswords[username];
      render();
    }, 5000);
    state.revealedPasswords[username] = { value: r.password, timerId };
    render();
  } catch (e) {
    alert('Peek failed: ' + e.message);
  }
}

async function deleteUser(username) {
  if (!confirm(`Delete user "${username}"?`)) return;
  try { await apiDELETE(`/api/v1/users/${encodeURIComponent(username)}`); }
  catch (e) { alert('Delete failed: ' + e.message); }
  await refreshAll();
}

async function moveUser(idx, delta) {
  const j = idx + delta;
  if (j < 0 || j >= state.users.length) return;
  const next = state.users.slice();
  [next[idx], next[j]] = [next[j], next[idx]];
  const usernames = next.map((u) => u.username);
  try { await apiPOST('/api/v1/users/reorder', usernames); }
  catch (e) { alert('Reorder failed: ' + e.message); }
  await refreshAll();
}

// --- Modals ---

function openAddKeyModal() {
  const root = document.getElementById('modal-root');
  root.innerHTML = '';

  const labelInput = inputEl({ autofocus: '' });
  const keyInput = inputEl({ type: 'password' });
  const errEl = el('div', { class: 'banner error', style: 'display:none' });
  const submitBtn = el('button', { class: 'primary' }, 'Add');

  const close = () => { root.innerHTML = ''; };
  const submit = async () => {
    if (!labelInput.value || !keyInput.value) {
      errEl.textContent = 'Label and key are required'; errEl.style.display = '';
      return;
    }
    submitBtn.disabled = true; submitBtn.textContent = 'Adding…';
    errEl.style.display = 'none';
    try {
      await apiPOST('/api/v1/keys', { Label: labelInput.value, APIKey: keyInput.value });
      close();
      await refreshAll();
    } catch (e) {
      errEl.textContent = e.message; errEl.style.display = '';
      submitBtn.disabled = false; submitBtn.textContent = 'Add';
    }
  };
  submitBtn.addEventListener('click', submit);

  root.appendChild(
    el('div', { class: 'modal-backdrop', onclick: (e) => { if (e.target.classList.contains('modal-backdrop')) close(); } },
      el('div', { class: 'modal' },
        el('h3', {}, 'Add API key'),
        el('div', { class: 'field' }, el('label', {}, 'Label'), labelInput),
        el('div', { class: 'field' }, el('label', {}, 'API key (sk_…)'), keyInput),
        errEl,
        el('div', { class: 'buttons' },
          el('button', { onclick: close }, 'Cancel'),
          submitBtn,
        ),
      ),
    ),
  );
}

function openAddUserModal() {
  const root = document.getElementById('modal-root');
  root.innerHTML = '';

  const usernameInput = inputEl({ autofocus: '' });
  const passwordInput = inputEl({ type: 'password' });
  const upstreamOptions = [{ value: '', label: '— (no mapping)' }]
    .concat(state.upstreams.map((u) => ({ value: u.id, label: u.display_name })));
  const upstreamSelect = selectEl({}, upstreamOptions, '');
  const errEl = el('div', { class: 'banner error', style: 'display:none' });
  const submitBtn = el('button', { class: 'primary' }, 'Add');

  const close = () => { root.innerHTML = ''; };
  const submit = async () => {
    const username = usernameInput.value;
    const password = passwordInput.value;
    if (!username || !password) {
      errEl.textContent = 'Username and password are required'; errEl.style.display = '';
      return;
    }
    submitBtn.disabled = true; submitBtn.textContent = 'Adding…';
    errEl.style.display = 'none';
    try {
      await apiPOST('/api/v1/users', { Username: username, Password: password });
      if (upstreamSelect.value) {
        await apiPATCH(`/api/v1/users/${encodeURIComponent(username)}`, { upstream_proxy_id: upstreamSelect.value });
      }
      close();
      await refreshAll();
    } catch (e) {
      errEl.textContent = e.message; errEl.style.display = '';
      submitBtn.disabled = false; submitBtn.textContent = 'Add';
    }
  };
  submitBtn.addEventListener('click', submit);

  root.appendChild(
    el('div', { class: 'modal-backdrop', onclick: (e) => { if (e.target.classList.contains('modal-backdrop')) close(); } },
      el('div', { class: 'modal' },
        el('h3', {}, 'Add user'),
        el('div', { class: 'field' }, el('label', {}, 'Username'), usernameInput),
        el('div', { class: 'field' }, el('label', {}, 'Password'), passwordInput),
        el('div', { class: 'field' }, el('label', {}, 'Mapped Proxy'), upstreamSelect),
        errEl,
        el('div', { class: 'buttons' },
          el('button', { onclick: close }, 'Cancel'),
          submitBtn,
        ),
      ),
    ),
  );
}

// --- DOM helpers ---

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined) continue;
      if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
      else if (k === 'class') node.className = v;
      else if (k === 'disabled' || k === 'autofocus' || k === 'checked' || k === 'readonly') {
        if (v !== null && v !== false) node.setAttribute(k, '');
      } else node.setAttribute(k, v);
    }
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.appendChild(typeof c === 'string' || typeof c === 'number' ? document.createTextNode(String(c)) : c);
  }
  return node;
}

function card(...children) {
  return el('div', { class: 'card' }, ...children);
}

function field(label, input) {
  return el('div', { class: 'field' }, el('label', {}, label), input);
}

function inputEl(attrs) {
  return el('input', attrs);
}

function selectEl(attrs, options, currentValue) {
  const select = el('select', attrs);
  for (const opt of options) {
    const o = document.createElement('option');
    o.value = opt.value;
    o.textContent = opt.label;
    if (String(opt.value) === String(currentValue)) o.selected = true;
    select.appendChild(o);
  }
  return select;
}

function formatRelative(iso) {
  const t = new Date(iso).getTime();
  const diff = Date.now() - t;
  const s = Math.floor(diff / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return new Date(iso).toLocaleString();
}

// --- Boot ---

document.getElementById('tabs').addEventListener('click', (e) => {
  const t = e.target.closest('.tab');
  if (!t) return;
  state.tab = t.dataset.tab;
  try { localStorage.setItem(TAB_STORAGE_KEY, state.tab); } catch (_) {}
  render();
});

document.getElementById('refresh-btn').addEventListener('click', () => refreshAll());

document.getElementById('logout-btn').addEventListener('click', async () => {
  try { await apiPOST('/web/api/logout'); } catch (_) {}
  location.href = '/login';
});

refreshAll();
setInterval(refreshAll, 30000);
