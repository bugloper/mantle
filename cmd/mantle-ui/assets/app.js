/* Mantle web interface.
 *
 * No framework and no build step (§14.3 forbids requiring a Node runtime in
 * production). Roughly 500 lines of plain DOM code is a smaller thing to audit
 * than a bundler config, and this interface is optional — it must never be the
 * reason an upgrade is hard.
 *
 * Two rules hold throughout:
 *   - Every value from the API reaches the page as textContent, never as
 *     innerHTML. Repository names, tags and usernames are user-controlled, and
 *     the Content-Security-Policy that forbids inline script only helps if the
 *     markup cannot be forged in the first place.
 *   - Write actions exist, but the registry authorises every one of them. This
 *     interface decides what to *offer*, never what is permitted: a control
 *     hidden here is a convenience, whereas a control the API refuses is the
 *     actual boundary. Running mantle-ui with --read-only removes the offers.
 */
'use strict';

// ---------------------------------------------------------------- utilities

/** Build an element. Children are appended as text unless already nodes. */
function el(tag, props, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key === 'class') node.className = value;
    else if (key === 'text') node.textContent = value;
    else if (key === 'html') throw new Error('innerHTML is not permitted here');
    else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value === true ? '' : value);
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

/** Render a byte count the way a person reads it. */
function bytes(n) {
  if (n === null || n === undefined) return '—';
  if (n < 1024) return n + ' B';
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let value = n / 1024, i = 0;
  while (value >= 1024 && i < units.length - 1) { value /= 1024; i++; }
  return value.toFixed(value < 10 ? 1 : 0) + ' ' + units[i];
}

/** Render an age at the granularity a person wants: "3h ago", not "3h14m22s". */
function age(iso) {
  if (!iso) return 'never';
  const then = new Date(iso);
  if (Number.isNaN(then.getTime())) return '—';
  const seconds = (Date.now() - then.getTime()) / 1000;

  // A timestamp slightly in the future is normal, not an anomaly: the registry
  // stamps "last used" during the very request that reads it, and the browser's
  // clock is independently skewed from the server's. Treating any negative
  // delta as a far-off date made a credential used one second ago render as a
  // bare calendar date. Only a genuinely distant future time gets that.
  if (seconds < -300) return then.toLocaleString();
  if (seconds < 60) return 'just now';
  if (seconds < 3600) return Math.floor(seconds / 60) + 'm ago';
  if (seconds < 86400) return Math.floor(seconds / 3600) + 'h ago';
  if (seconds < 2592000) return Math.floor(seconds / 86400) + 'd ago';
  return then.toLocaleDateString();
}

function shortDigest(digest) {
  if (!digest) return '—';
  const encoded = digest.includes(':') ? digest.split(':')[1] : digest;
  return encoded.slice(0, 12);
}

function shortCommit(sha) { return sha ? sha.slice(0, 7) : ''; }

function pill(text, kind) { return el('span', { class: 'pill pill-' + kind, text }); }

// ------------------------------------------------------------------- client

const auth = {
  key: 'mantle.credentials',
  get() {
    try { return JSON.parse(sessionStorage.getItem(this.key)) || null; }
    catch { return null; }
  },
  set(username, secret) {
    // sessionStorage, not localStorage: the credential dies with the tab. This
    // is a read-only dashboard, not somewhere to persist a push-capable token
    // across browser restarts.
    sessionStorage.setItem(this.key, JSON.stringify({ username, secret }));
  },
  clear() { sessionStorage.removeItem(this.key); },
  header() {
    const c = this.get();
    if (!c) return null;
    return 'Basic ' + btoa(unescape(encodeURIComponent(c.username + ':' + c.secret)));
  },
};

/** An API failure carrying the structured fields the admin API returns. */
class ApiError extends Error {
  constructor(status, code, message, remedy) {
    super(message || ('HTTP ' + status));
    this.status = status;
    this.code = code;
    this.remedy = remedy;
  }
}

async function api(path, options) {
  options = options || {};
  const header = auth.header();
  const headers = { Accept: 'application/json' };
  if (header) headers.Authorization = header;
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';

  const response = await fetch('/api/v1' + path, {
    method: options.method || 'GET',
    headers,
    body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
  });

  if (response.status === 401) {
    auth.clear();
    showLogin('Your session is no longer valid. Sign in again.');
    throw new ApiError(401, 'unauthorized', 'authentication required');
  }

  let body = null;
  try { body = await response.json(); } catch { /* not JSON */ }

  if (!response.ok) {
    const e = (body && body.error) || {};
    throw new ApiError(response.status, e.code, e.message, e.remedy);
  }
  return body;
}

/** What this deployment permits. Fetched once at boot from mantle-ui itself. */
const capabilities = { readOnly: false, version: 'dev' };

async function loadCapabilities() {
  try {
    const response = await fetch('/ui-config.json', { headers: { Accept: 'application/json' } });
    const config = await response.json();
    capabilities.readOnly = !!config.read_only;
    capabilities.version = config.version || 'dev';
  } catch {
    // Assume writes are permitted and let the proxy be the authority. Guessing
    // read-only would hide controls that actually work.
  }
}

/** Brief confirmation that something happened. */
function toast(message) {
  const node = el('div', { class: 'toast', role: 'status', text: message });
  document.body.append(node);
  setTimeout(() => node.remove(), 2600);
}

// -------------------------------------------------------------------- dialogs

/**
 * Open a form dialog.
 *
 * fields is an array of {name, label, type, value, options, hint, required}.
 * onSubmit receives the collected values and may throw an ApiError, which is
 * shown inline rather than closing the dialog — losing a half-filled form to a
 * validation error is the fastest way to make an interface annoying.
 */
function openDialog({ title, hint, fields, submitLabel, danger, wide, onSubmit, confirmText }) {
  const overlay = el('div', { class: 'overlay' });
  const form = el('form', { class: 'dialog' + (wide ? ' dialog-wide' : '') });
  const errorBox = el('p', { class: 'dialog-error', hidden: true });
  const inputs = {};

  form.append(el('h2', { text: title }));
  if (hint) form.append(el('p', { class: 'dialog-hint', text: hint }));
  form.append(errorBox);

  for (const field of fields || []) {
    if (field.type === 'checkbox') {
      const input = el('input', { type: 'checkbox', id: 'f-' + field.name });
      if (field.value) input.checked = true;
      inputs[field.name] = () => input.checked;
      const wrap = el('div', { class: 'field-check' }, input,
        el('label', { for: 'f-' + field.name, text: field.label }));
      form.append(wrap);
      if (field.hint) form.append(el('p', { class: 'field-hint', text: field.hint }));
      continue;
    }

    let input;
    if (field.type === 'select') {
      input = el('select', { id: 'f-' + field.name });
      for (const option of field.options || []) {
        const node = el('option', { value: option.value, text: option.label });
        if (option.value === field.value) node.selected = true;
        input.append(node);
      }
    } else {
      input = el('input', {
        type: field.type || 'text',
        id: 'f-' + field.name,
        value: field.value || '',
        placeholder: field.placeholder || '',
        autocomplete: field.type === 'password' ? 'new-password' : 'off',
      });
    }
    inputs[field.name] = () => input.value.trim();

    const wrap = el('div', { class: 'field' },
      el('label', { for: 'f-' + field.name, text: field.label }), input);
    if (field.hint) wrap.append(el('p', { class: 'field-hint', text: field.hint }));
    form.append(wrap);
  }

  const submit = el('button', {
    type: 'submit',
    class: 'btn ' + (danger ? 'btn-danger-solid' : 'btn-primary'),
    text: submitLabel || 'Create',
  });
  const cancel = el('button', { type: 'button', class: 'btn', text: 'Cancel' });
  form.append(el('div', { class: 'dialog-actions' }, cancel, submit));

  const close = () => {
    overlay.remove();
    document.removeEventListener('keydown', onKey);
  };
  const onKey = (event) => { if (event.key === 'Escape') close(); };
  document.addEventListener('keydown', onKey);
  cancel.addEventListener('click', close);
  overlay.addEventListener('click', (event) => { if (event.target === overlay) close(); });

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    errorBox.hidden = true;
    const values = {};
    for (const [name, read] of Object.entries(inputs)) values[name] = read();

    for (const field of fields || []) {
      if (field.required && !values[field.name]) {
        errorBox.textContent = field.label + ' is required.';
        errorBox.hidden = false;
        return;
      }
    }
    // A typed confirmation for irreversible actions. A single click is too
    // little friction for something that cannot be undone.
    if (confirmText && values.confirm !== confirmText) {
      errorBox.textContent = 'Type ' + confirmText + ' exactly to confirm.';
      errorBox.hidden = false;
      return;
    }

    submit.disabled = true;
    cancel.disabled = true;
    try {
      await onSubmit(values);
      close();
    } catch (error) {
      errorBox.textContent = (error.message || String(error)) +
        (error.remedy ? '\n' + error.remedy : '');
      errorBox.hidden = false;
      submit.disabled = false;
      cancel.disabled = false;
    }
  });

  overlay.append(form);
  document.body.append(overlay);
  const first = form.querySelector('input:not([type=checkbox]), select');
  if (first) first.focus();
}

/**
 * Show a generated secret. It is displayed exactly once and is not recoverable,
 * so the dialog says so and offers a copy button rather than expecting the
 * reader to select monospace text by hand.
 */
function showSecret(title, secret, footnote) {
  const overlay = el('div', { class: 'overlay' });
  const box = el('div', { class: 'secret-box', text: secret });
  const copy = el('button', { type: 'button', class: 'btn', text: 'Copy' });
  const done = el('button', { type: 'button', class: 'btn btn-primary', text: 'Done' });

  copy.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(secret);
      copy.textContent = 'Copied';
    } catch {
      // Clipboard access can be refused; the text is selectable either way.
      copy.textContent = 'Select and copy';
    }
  });
  done.addEventListener('click', () => overlay.remove());

  const dialog = el('div', { class: 'dialog dialog-wide' },
    el('h2', { text: title }),
    el('p', { class: 'dialog-hint',
      text: 'This secret is shown once and cannot be retrieved again. Store it now.' }),
    box,
    footnote ? el('p', { class: 'field-hint', text: footnote }) : null,
    el('div', { class: 'dialog-actions' }, copy, done));

  overlay.append(dialog);
  document.body.append(overlay);
  done.focus();
}

/** A confirmation dialog for a destructive action. */
function confirmDialog({ title, hint, submitLabel, confirmText, onSubmit }) {
  openDialog({
    title, hint, danger: true,
    submitLabel: submitLabel || 'Delete',
    confirmText,
    fields: confirmText
      ? [{ name: 'confirm', label: 'Type ' + confirmText + ' to confirm', required: true }]
      : [],
    onSubmit,
  });
}

/** An action button, omitted entirely when this deployment is read-only. */
function actionButton(label, kind, onClick) {
  if (capabilities.readOnly) return null;
  const button = el('button', { type: 'button', class: 'btn ' + (kind || ''), text: label });
  button.addEventListener('click', onClick);
  return button;
}

// -------------------------------------------------------------------- views

const view = () => document.getElementById('view');

function setBusy() {
  clear(view());
  view().append(el('div', { class: 'skeleton', text: 'Loading…' }));
}

/** Render an error into the main view, keeping the chrome intact. */
function showError(error) {
  clear(view());
  const kind = error.status === 403 ? 'notice-warn' : 'notice-bad';
  const box = el('div', { class: 'notice ' + kind },
    el('strong', { text: error.status === 403 ? 'Not permitted' : 'Something went wrong' }),
    error.message);
  if (error.remedy) box.append(el('div', { style: 'margin-top:6px', text: error.remedy }));
  view().append(box);
}

/** A panel that degrades to a note when the caller is not an administrator. */
async function guarded(loader, label) {
  try { return await loader(); }
  catch (error) {
    if (error.status === 403) return { forbidden: true, label };
    throw error;
  }
}

function forbiddenCard(label) {
  return el('div', { class: 'card card-pad' },
    el('div', { class: 'empty-inline', text: label + ' requires an instance administrator account.' }));
}

// --- overview -------------------------------------------------------------

async function renderOverview() {
  setBusy();

  const [repos, orgs, gc] = await Promise.all([
    api('/repositories?limit=500'),
    guarded(() => api('/organizations'), 'Organizations'),
    guarded(() => api('/gc/status'), 'Garbage collection'),
  ]);

  const repositories = repos.repositories || [];
  const totalBytes = repositories.reduce((sum, r) => sum + (r.used_bytes || 0), 0);
  const totalTags = repositories.reduce((sum, r) => sum + (r.tags || 0), 0);
  const publicCount = repositories.filter(r => r.visibility === 'public').length;

  clear(view());
  view().append(pageHead('Overview', 'What this registry is holding right now.',
    [actionButton('New repository', 'btn-primary', () => newRepositoryDialog())]));

  view().append(el('div', { class: 'stats' },
    stat('Repositories', String(repositories.length),
      publicCount ? publicCount + ' public' : 'all private'),
    stat('Tags', String(totalTags), null),
    stat('Storage', bytes(totalBytes), 'attributed to repositories'),
    stat('Organizations', orgs.forbidden ? '—' : String((orgs.organizations || []).length), null)));

  // Quarantine is the number an operator most wants next to storage: bytes no
  // longer served but not yet reclaimed, and recoverable until the window ends.
  if (!gc.forbidden) {
    if (gc.stuck_deleting > 0) {
      view().append(el('div', { class: 'notice notice-bad', style: 'margin-top:18px' },
        el('strong', { text: gc.stuck_deleting + ' blob(s) stuck in the deleting state' }),
        gc.alert || 'Storage deletion has been failing. Check the daemon log.'));
    }
    view().append(el('h2', { class: 'section', text: 'Garbage collection' }));
    view().append(gcCard(gc));
  }

  view().append(el('h2', { class: 'section', text: 'Largest repositories' }));
  const largest = [...repositories].sort((a, b) => (b.used_bytes || 0) - (a.used_bytes || 0)).slice(0, 8);
  view().append(largest.length ? repositoryTable(largest) : emptyRepositories());
}

/** A page heading with an optional row of action buttons on the right. */
function pageHead(title, description, actions) {
  const head = el('div', { class: 'page-head' }, el('h1', { text: title }));
  if (description) head.append(el('p', { text: description }));
  const buttons = (actions || []).filter(Boolean);
  if (!buttons.length) return head;
  return el('div', { class: 'page-head-row' }, head, el('div', { class: 'btn-row' }, buttons));
}

function stat(label, value, sub) {
  return el('div', { class: 'stat' },
    el('div', { class: 'stat-label', text: label }),
    el('div', { class: 'stat-value', text: value }),
    sub ? el('div', { class: 'stat-sub', text: sub }) : null);
}

function gcCard(gc) {
  const rows = [];
  if (gc.last_run) {
    const status = gc.last_run.status;
    rows.push(['Last run',
      el('span', {}, pill(status, status === 'succeeded' ? 'good' : status === 'failed' ? 'bad' : 'warn'),
        ' ', el('span', { class: 'stat-sub', text: age(gc.last_run.started_at) }))]);
    if (gc.last_run.stats) {
      const s = gc.last_run.stats;
      rows.push(['Reclaimed', bytes(s.bytes_reclaimed || 0) + ' from ' + (s.blobs_swept || 0) + ' blob(s)']);
    }
    if (gc.last_run.error) rows.push(['Error', gc.last_run.error]);
  } else {
    rows.push(['Last run', el('span', { class: 'empty-inline', text: gc.note || 'has not run yet' })]);
  }
  rows.push(['Quarantined',
    gc.quarantined_blobs + ' blob(s), ' + bytes(gc.quarantined_bytes || 0) +
    ' — no longer served, still recoverable']);

  const table = el('table');
  const body = el('tbody');
  for (const [key, value] of rows) {
    body.append(el('tr',
      {}, el('td', { style: 'width:150px; color:var(--text-muted)', text: key }),
      el('td', {}, value)));
  }
  table.append(body);
  return el('div', { class: 'card' }, table);
}

// --- repositories ---------------------------------------------------------

async function renderRepositories() {
  setBusy();
  const data = await api('/repositories?limit=500');
  const repositories = data.repositories || [];

  clear(view());
  view().append(pageHead('Repositories',
    repositories.length + ' visible to this account.',
    [actionButton('New repository', 'btn-primary', () => newRepositoryDialog())]));

  view().append(repositories.length ? repositoryTable(repositories) : emptyRepositories());
}

function repositoryTable(repositories) {
  const body = el('tbody');
  for (const repo of repositories) {
    body.append(el('tr', {},
      el('td', {}, el('a', { href: '#/repositories/' + repo.name, text: repo.name })),
      el('td', {}, repo.visibility === 'public'
        ? pill('public', 'warn')
        : pill('private', 'neutral')),
      el('td', { class: 'num', text: String(repo.tags ?? 0) }),
      el('td', { class: 'num', text: String(repo.manifests ?? 0) }),
      el('td', { class: 'num', text: bytes(repo.used_bytes || 0) }),
      el('td', { text: age(repo.created_at) })));
  }
  return el('div', { class: 'card table-scroll' },
    el('table', {},
      el('thead', {}, el('tr', {},
        el('th', { text: 'Repository' }),
        el('th', { text: 'Visibility' }),
        el('th', { class: 'num', text: 'Tags' }),
        el('th', { class: 'num', text: 'Manifests' }),
        el('th', { class: 'num', text: 'Size' }),
        el('th', { text: 'Created' }))),
      body));
}

function emptyRepositories() {
  return el('div', { class: 'card empty' },
    el('div', { class: 'empty-title', text: 'Nothing pushed yet' }),
    el('p', { text: 'Create a token and push an image to see it here.' }),
    el('pre', { text: 'mantle setup --repo acme/web' }));
}

// --- organizations --------------------------------------------------------

async function renderOrganizations() {
  setBusy();
  const orgs = await guarded(() => api('/organizations'), 'Organizations');

  clear(view());
  view().append(pageHead('Organizations',
    'Every repository belongs to one. Quotas are set per organization.',
    [actionButton('New organization', 'btn-primary', () => newOrganizationDialog())]));

  if (orgs.forbidden) {
    view().append(forbiddenCard('Organizations'));
    return;
  }

  const list = orgs.organizations || [];
  if (!list.length) {
    view().append(el('div', { class: 'card empty' },
      el('div', { class: 'empty-title', text: 'No organizations' }),
      el('p', { text: 'Create one before pushing an image.' })));
    return;
  }

  const body = el('tbody');
  for (const org of list) {
    body.append(el('tr', {},
      el('td', { text: org.slug }),
      el('td', { text: org.display_name }),
      el('td', { class: 'num', text: String(org.repositories) }),
      el('td', { class: 'num' }, org.quota_bytes === null || org.quota_bytes === undefined
        ? el('span', { class: 'stat-sub', text: 'unlimited' })
        : document.createTextNode(bytes(org.quota_bytes)))));
  }
  view().append(el('div', { class: 'card table-scroll' },
    el('table', {},
      el('thead', {}, el('tr', {},
        el('th', { text: 'Slug' }), el('th', { text: 'Display name' }),
        el('th', { class: 'num', text: 'Repositories' }),
        el('th', { class: 'num', text: 'Quota' }))),
      body)));
}

function newOrganizationDialog() {
  openDialog({
    title: 'New organization',
    hint: 'The slug becomes the first path component of every repository in it.',
    fields: [
      { name: 'slug', label: 'Slug', required: true, placeholder: 'acme',
        hint: 'Lowercase. Repositories will be named acme/<something>.' },
      { name: 'display_name', label: 'Display name', placeholder: 'Acme Corp' },
      { name: 'quota', label: 'Storage quota', placeholder: 'e.g. 100GiB — blank for unlimited',
        hint: 'Counted as distinct blobs plus manifest bytes. Pulls are never blocked by quota.' },
    ],
    submitLabel: 'Create organization',
    async onSubmit(values) {
      const body = { slug: values.slug, display_name: values.display_name };
      if (values.quota) {
        const parsed = parseBytesInput(values.quota);
        if (parsed === null) throw new ApiError(0, 'invalid', 'Quota must look like 100GiB or 500MB.');
        body.quota_bytes = parsed;
      }
      await api('/organizations', { method: 'POST', body });
      toast('Created ' + values.slug);
      await route();
    },
  });
}

/** Parse a human byte size, mirroring what the daemon's config accepts. */
function parseBytesInput(text) {
  const match = /^([0-9.]+)\s*(B|KB|MB|GB|TB|KIB|MIB|GIB|TIB)?$/i.exec(text.trim());
  if (!match) return null;
  const value = parseFloat(match[1]);
  if (Number.isNaN(value)) return null;
  const factors = {
    B: 1, KB: 1e3, MB: 1e6, GB: 1e9, TB: 1e12,
    KIB: 1024, MIB: 1024 ** 2, GIB: 1024 ** 3, TIB: 1024 ** 4,
  };
  return Math.round(value * (factors[(match[2] || 'B').toUpperCase()] || 1));
}

function newRepositoryDialog() {
  openDialog({
    title: 'New repository',
    hint: 'Pushing to a name creates it automatically. Create it here to settle ' +
          'its visibility and tag policy before the first image arrives.',
    fields: [
      { name: 'name', label: 'Name', required: true, placeholder: 'acme/web',
        hint: 'Organization-qualified and lowercase, e.g. acme/web.' },
      { name: 'visibility', label: 'Visibility', type: 'select', value: 'private',
        options: [{ value: 'private', label: 'Private' }, { value: 'public', label: 'Public' }],
        hint: 'Public repositories are readable by anyone who can reach this registry.' },
      { name: 'immutable_tags', label: 'Make tags immutable', type: 'checkbox',
        hint: 'Once a tag points at a digest it cannot be moved or deleted.' },
    ],
    submitLabel: 'Create repository',
    async onSubmit(values) {
      await api('/repositories', { method: 'POST', body: {
        name: values.name,
        visibility: values.visibility,
        immutable_tags: values.immutable_tags,
      }});
      toast('Created ' + values.name);
      location.hash = '#/repositories/' + values.name;
      await route();
    },
  });
}

// --- repository detail, with the ledger as the hero -----------------------

async function renderRepository(name) {
  setBusy();

  const [ledger, deployments] = await Promise.all([
    api('/repositories/' + name + '/ledger'),
    api('/deployments?repository=' + encodeURIComponent(name) + '&limit=25')
      .catch(() => ({ deployments: [] })),
  ]);

  clear(view());
  view().append(el('div', { class: 'crumb' },
    el('a', { href: '#/repositories', text: 'Repositories' }), ' / ', name));

  const repo = await api('/repositories/' + name).catch(() => null);

  const head = el('div', { class: 'page-head' }, el('h1', { text: name }));
  const meta = el('p', {});
  if (repo) {
    meta.append(repo.visibility === 'public' ? pill('public', 'warn') : pill('private', 'neutral'));
    if (repo.immutable_tags) meta.append(' ', pill('immutable tags', 'accent'));
  }
  if (ledger.source_url) {
    meta.append(' ', el('a', {
      href: ledger.source_url, rel: 'noreferrer noopener', target: '_blank',
      text: ledger.source_url,
    }));
  }
  head.append(meta);

  const actions = repo ? [
    actionButton(repo.visibility === 'public' ? 'Make private' : 'Make public', '',
      () => setVisibilityDialog(name, repo.visibility)),
    actionButton(repo.immutable_tags ? 'Allow tag moves' : 'Make tags immutable', '',
      () => setImmutableDialog(name, repo.immutable_tags)),
    actionButton('Delete', 'btn-danger', () => deleteRepositoryDialog(name, repo)),
  ] : [];
  const buttons = actions.filter(Boolean);
  view().append(buttons.length
    ? el('div', { class: 'page-head-row' }, head, el('div', { class: 'btn-row' }, buttons))
    : head);

  view().append(el('h2', { class: 'section', text: 'Deployment ledger' }));
  view().append(ledgerHero(ledger));

  const storage = ledger.storage || {};
  view().append(el('h2', { class: 'section', text: 'Storage' }));
  view().append(el('div', { class: 'stats' },
    stat('Total', bytes(storage.total_bytes || 0), storage.manifests + ' manifests'),
    stat('Reclaimable', bytes(storage.reclaimable_bytes || 0), 'estimate — confirm with a GC dry run'),
    stat('Quarantined', bytes(storage.quarantined_bytes || 0), 'recoverable until the window ends')));

  const history = deployments.deployments || [];
  view().append(el('h2', { class: 'section', text: 'Deployment history' }));
  view().append(history.length ? deploymentTable(history) : el('div', { class: 'card empty' },
    el('div', { class: 'empty-title', text: 'No deployments recorded' }),
    el('p', { text: 'Report one from whatever already deploys, and this repository ' +
                    'becomes pinned against retention and garbage collection.' }),
    el('pre', { text: 'mantle deploy record --repo ' + name + ' \\\n' +
                      '  --digest "$DIGEST" --env production \\\n' +
                      '  --host "$(hostname)" --status active || true' })));
}

function setVisibilityDialog(name, current) {
  const next = current === 'public' ? 'private' : 'public';
  openDialog({
    title: 'Make ' + name + ' ' + next,
    hint: next === 'public'
      ? 'Anyone who can reach this registry will be able to pull from it, ' +
        'without authenticating if anonymous pull is enabled.'
      : 'Only identities with an explicit grant will be able to pull from it.',
    fields: [],
    submitLabel: 'Make ' + next,
    danger: next === 'public',
    async onSubmit() {
      await api('/repositories/' + name, { method: 'PATCH', body: { visibility: next } });
      toast(name + ' is now ' + next);
      await route();
    },
  });
}

function setImmutableDialog(name, current) {
  openDialog({
    title: current ? 'Allow tag moves on ' + name : 'Make tags immutable on ' + name,
    hint: current
      ? 'Tags will be movable and deletable again.'
      : 'Once a tag points at a digest it cannot be moved or deleted. Re-pushing ' +
        'identical content under the same tag still succeeds, so retries are safe.',
    fields: [],
    submitLabel: current ? 'Allow moves' : 'Make immutable',
    async onSubmit() {
      await api('/repositories/' + name, {
        method: 'PATCH', body: { immutable_tags: !current },
      });
      toast('Updated ' + name);
      await route();
    },
  });
}

function deleteRepositoryDialog(name, repo) {
  confirmDialog({
    title: 'Delete ' + name,
    hint: 'This removes ' + repo.tags + ' tag(s) and ' + repo.manifests +
          ' manifest(s). Storage is reclaimed by garbage collection afterwards, ' +
          'not immediately. A manifest that any environment is currently running ' +
          'cannot be deleted, and the registry will refuse rather than break it.',
    submitLabel: 'Delete repository',
    confirmText: name,
    async onSubmit() {
      await api('/repositories/' + name, { method: 'DELETE' });
      toast('Deleted ' + name);
      location.hash = '#/repositories';
      await route();
    },
  });
}

function ledgerHero(ledger) {
  const hero = el('div', { class: 'ledger-hero' });

  // --- now running ---
  const running = ledger.running;
  const runningBody = el('div');
  if (!running) {
    runningBody.append(el('div', { class: 'empty-inline',
      text: 'No deployment recorded for this environment.' }));
    runningBody.append(el('div', { class: 'running-meta',
      text: 'Mantle will also infer one from pull traffic, at lower confidence.' }));
  } else {
    const line = el('div', { class: 'running-line' },
      el('span', { class: 'running-tag', text: running.tag || '(untagged)' }),
      el('span', { class: 'digest', text: shortDigest(running.digest) }));
    if (running.commit_sha) {
      line.append(el('span', { class: 'commit', text: 'commit ' + shortCommit(running.commit_sha) }));
    }
    line.append(el('span', { class: 'stat-sub', text: 'deployed ' + age(running.started_at) }));
    runningBody.append(line);

    // The confidence tier is shown rather than hidden. An inferred deployment
    // is a good guess from pull traffic and a reported one is a fact; showing
    // them identically would make the ledger untrustworthy the first time a
    // guess turned out wrong.
    const meta = el('div', { class: 'running-meta' });
    if (running.performer) meta.append('by ' + running.performer + ' · ');
    const confirmed = (running.hosts || []).filter(h => h.status === 'confirmed').length;
    if ((running.hosts || []).length) {
      meta.append(confirmed + '/' + running.hosts.length + ' hosts confirmed · ');
    }
    meta.append(confidencePill(running.confidence, running.deploy_tool));
    runningBody.append(meta);

    if ((running.hosts || []).length) {
      const hosts = el('div', { class: 'running-hosts' });
      for (const host of running.hosts) {
        hosts.append(el('span', { class: 'tag', text: host.hostname || host.address }));
      }
      runningBody.append(hosts);
    }
  }
  hero.append(ledgerRow('Now running', runningBody));

  // --- rollback candidates ---
  const candidates = ledger.rollback_candidates || [];
  const rollbackBody = el('div');
  if (!candidates.length) {
    rollbackBody.append(el('span', { class: 'empty-inline', text: 'No earlier deployment recorded.' }));
  } else {
    for (const target of candidates) {
      rollbackBody.append(el('div', { class: 'rollback-item' },
        el('span', { text: target.tag || '(untagged)' }),
        el('span', { class: 'digest', text: shortDigest(target.digest) }),
        target.commit_sha ? el('span', { class: 'commit', text: shortCommit(target.commit_sha) }) : null,
        el('span', { class: 'stat-sub', text: age(target.deployed_at) }),
        // The pin is the product's central promise made visible: a pinned image
        // cannot be removed by retention or collection, by construction.
        target.pinned ? pill('pinned', 'good') : pill('unpinned', 'warn')));
    }
  }
  hero.append(ledgerRow('Rollback to', rollbackBody));

  // --- tags ---
  const tags = ledger.tags || [];
  const tagBody = el('div');
  if (!tags.length) {
    tagBody.append(el('span', { class: 'empty-inline', text: 'No tags.' }));
  } else {
    for (const tag of tags.slice(0, 40)) {
      tagBody.append(el('span', { class: 'tag', title: tag.digest, text: tag.name }));
    }
    if (tags.length > 40) tagBody.append(el('span', { class: 'stat-sub', text: ' +' + (tags.length - 40) + ' more' }));
  }
  hero.append(ledgerRow('Tags', tagBody));

  if ((ledger.environments || []).length > 1) {
    const envBody = el('div');
    for (const env of ledger.environments) envBody.append(el('span', { class: 'tag', text: env }));
    hero.append(ledgerRow('Environments', envBody));
  }

  return hero;
}

function ledgerRow(key, body) {
  return el('div', { class: 'ledger-row' },
    el('div', { class: 'ledger-key', text: key }), body);
}

function confidencePill(confidence, tool) {
  if (confidence === 'reported') {
    return pill(tool ? 'reported via ' + tool : 'reported', 'good');
  }
  if (confidence === 'verified') return pill('verified by agent', 'good');
  return pill('inferred from pull traffic', 'warn');
}

function deploymentTable(deployments) {
  const body = el('tbody');
  for (const d of deployments) {
    const statusKind = d.status === 'active' ? 'good'
      : d.status === 'failed' ? 'bad'
      : d.status === 'rolled_back' ? 'warn' : 'neutral';
    body.append(el('tr', {},
      el('td', { text: age(d.started_at) }),
      el('td', { text: d.environment }),
      el('td', {}, pill(d.status, statusKind)),
      el('td', { class: 'mono', text: shortDigest(d.digest) }),
      el('td', { class: 'mono', text: shortCommit(d.commit_sha) || '—' }),
      el('td', {}, pill(d.confidence, d.confidence === 'inferred' ? 'warn' : 'good')),
      el('td', { text: d.performer || '—' })));
  }
  return el('div', { class: 'card table-scroll' },
    el('table', {},
      el('thead', {}, el('tr', {},
        el('th', { text: 'When' }), el('th', { text: 'Env' }), el('th', { text: 'Status' }),
        el('th', { text: 'Digest' }), el('th', { text: 'Commit' }),
        el('th', { text: 'Confidence' }), el('th', { text: 'By' }))),
      body));
}

// --- identities -----------------------------------------------------------

async function renderIdentities() {
  setBusy();
  const [users, tokens] = await Promise.all([
    guarded(() => api('/users'), 'Users'),
    guarded(() => api('/tokens'), 'Tokens'),
  ]);

  clear(view());
  view().append(pageHead('Identities', 'Accounts and machine credentials.', [
    actionButton('New user', '', () => newUserDialog()),
    actionButton('New token', 'btn-primary', () => newTokenDialog()),
  ]));

  view().append(el('h2', { class: 'section', text: 'Users' }));
  view().append(users.forbidden ? forbiddenCard('Users') : identityTable(users.users || [], false));

  view().append(el('h2', { class: 'section', text: 'Machine credentials' }));
  view().append(tokens.forbidden ? forbiddenCard('Tokens') : identityTable(tokens.tokens || [], true));
}

function newUserDialog() {
  openDialog({
    title: 'New user',
    hint: 'A human account. Machine access should use a token instead.',
    fields: [
      { name: 'name', label: 'Username', required: true },
      { name: 'email', label: 'Email', placeholder: 'optional' },
      { name: 'password', label: 'Initial password', type: 'password', required: true },
      { name: 'instance_admin', label: 'Instance administrator', type: 'checkbox',
        hint: 'Full access to every organization, plus instance settings.' },
    ],
    submitLabel: 'Create user',
    async onSubmit(values) {
      await api('/users', { method: 'POST', body: {
        name: values.name, email: values.email,
        password: values.password, instance_admin: values.instance_admin,
      }});
      toast('Created ' + values.name);
      await route();
    },
  });
}

async function newTokenDialog() {
  // The organization list drives the picker, so a token cannot be created
  // against an organization that does not exist.
  let organizations = [];
  try {
    const response = await api('/organizations');
    organizations = (response.organizations || []).map((o) => ({ value: o.slug, label: o.slug }));
  } catch {
    // Falls back to a free-text field below.
  }

  openDialog({
    title: 'New token',
    hint: 'The secret is shown once, when it is created.',
    wide: true,
    fields: [
      { name: 'name', label: 'Name', required: true, placeholder: 'acme/web builder' },
      organizations.length
        ? { name: 'organization', label: 'Organization', type: 'select',
            value: organizations[0].value, options: organizations }
        : { name: 'organization', label: 'Organization', required: true },
      { name: 'namespace', label: 'Repository prefix',
        placeholder: 'blank for the whole organization',
        hint: 'e.g. acme/web restricts the token to that repository and anything under it.' },
      { name: 'role', label: 'Role', type: 'select', value: 'reader', options: [
        { value: 'reader', label: 'reader — pull only' },
        { value: 'contributor', label: 'contributor — pull and push' },
        { value: 'maintainer', label: 'maintainer — also delete tags and set policy' },
        { value: 'owner', label: 'owner — also delete the repository' },
      ], hint: 'Servers want reader. A build machine wants contributor.' },
      { name: 'kind', label: 'Kind', type: 'select', value: 'deploy_token', options: [
        { value: 'deploy_token', label: 'deploy token — servers and deploy tooling' },
        { value: 'robot', label: 'robot — CI' },
        { value: 'pat', label: 'personal access token — a human scripting' },
      ]},
      { name: 'expires_in_days', label: 'Expires in (days)', type: 'number',
        placeholder: '0 or blank — never expires' },
    ],
    submitLabel: 'Create token',
    async onSubmit(values) {
      const created = await api('/tokens', { method: 'POST', body: {
        name: values.name,
        organization: values.organization,
        namespace: values.namespace,
        role: values.role,
        kind: values.kind,
        expires_in_days: parseInt(values.expires_in_days, 10) || 0,
      }});
      await route();
      showSecret('Token ' + values.name,
        created.secret,
        'Use it as the password: docker login ' + location.host +
        ' -u ' + values.name + ' -p <secret>');
    },
  });
}

function revokeTokenDialog(token) {
  confirmDialog({
    title: 'Revoke ' + token.name,
    hint: 'The credential stops working immediately — permissions are re-evaluated ' +
          'on every request, so it does not keep working until a token expires. ' +
          'It is disabled rather than deleted, so audit records that reference it ' +
          'stay meaningful.',
    submitLabel: 'Revoke',
    async onSubmit() {
      await api('/tokens/' + token.uuid, { method: 'DELETE' });
      toast('Revoked ' + token.name);
      await route();
    },
  });
}

function identityTable(identities, showKind) {
  if (!identities.length) {
    return el('div', { class: 'card empty' }, el('p', { text: 'None.' }));
  }
  const body = el('tbody');
  for (const identity of identities) {
    const row = el('tr', {}, el('td', { text: identity.name }));
    if (showKind) row.append(el('td', {}, pill(identity.kind, 'neutral')));
    row.append(
      el('td', {}, identity.disabled ? pill('disabled', 'bad') : pill('active', 'good')),
      el('td', {}, identity.instance_admin ? pill('admin', 'accent') : ''),
      el('td', { text: identity.expires_at ? age(identity.expires_at) : 'never' }),
      el('td', { text: age(identity.last_used_at) }));

    if (showKind) {
      const revoke = identity.disabled ? null
        : actionButton('Revoke', 'btn-small btn-danger', () => revokeTokenDialog(identity));
      row.append(el('td', { class: 'row-actions' }, revoke));
    }
    body.append(row);
  }
  const headings = [el('th', { text: 'Name' })];
  if (showKind) headings.push(el('th', { text: 'Kind' }));
  headings.push(el('th', { text: 'Status' }), el('th', { text: 'Role' }),
    el('th', { text: 'Expires' }), el('th', { text: 'Last used' }));
  if (showKind) headings.push(el('th', {}));

  return el('div', { class: 'card table-scroll' },
    el('table', {}, el('thead', {}, el('tr', {}, headings)), body));
}

// --- storage --------------------------------------------------------------

async function renderStorage() {
  setBusy();
  const [repos, gc] = await Promise.all([
    api('/repositories?limit=500'),
    guarded(() => api('/gc/status'), 'Garbage collection'),
  ]);

  clear(view());
  view().append(pageHead('Storage',
    'Blobs are deduplicated globally, so repository sizes overlap and will not ' +
    'sum to the total on disk.',
    gc.forbidden ? [] : [
      actionButton('Dry run', '', () => runGC(true)),
      actionButton('Reconcile', '', () => reconcileDialog()),
      actionButton('Collect now', 'btn-danger', () => collectDialog()),
    ]));

  if (gc.forbidden) {
    view().append(forbiddenCard('Garbage collection'));
  } else {
    view().append(gcCard(gc));
  }

  const repositories = (repos.repositories || [])
    .sort((a, b) => (b.used_bytes || 0) - (a.used_bytes || 0));
  view().append(el('h2', { class: 'section', text: 'By repository' }));
  view().append(repositories.length ? repositoryTable(repositories) : emptyRepositories());
}

// --- garbage collection actions -------------------------------------------

/** Run a collection cycle and show what it did. */
async function runGC(dryRun) {
  const overlay = el('div', { class: 'overlay' });
  const dialog = el('div', { class: 'dialog dialog-wide' },
    el('h2', { text: dryRun ? 'Dry run' : 'Garbage collection' }),
    el('p', { class: 'dialog-hint', text: 'Running…' }));
  overlay.append(dialog);
  document.body.append(overlay);

  let stats;
  try {
    stats = await api('/gc/run' + (dryRun ? '?dry_run=true' : ''), { method: 'POST', body: {} });
  } catch (error) {
    clear(dialog);
    dialog.append(
      el('h2', { text: 'Collection failed' }),
      el('p', { class: 'dialog-error', text: error.message || String(error) }),
      el('div', { class: 'dialog-actions' },
        el('button', { type: 'button', class: 'btn btn-primary', text: 'Close',
          onclick: () => overlay.remove() })));
    return;
  }

  clear(dialog);
  dialog.append(el('h2', { text: dryRun ? 'Dry run — nothing was changed' : 'Collection finished' }));

  const rows = el('tbody');
  const add = (label, value) => rows.append(el('tr', {},
    el('td', { style: 'color:var(--text-muted)', text: label }),
    el('td', { text: String(value) })));
  add('Sessions cleaned', stats.sessions_cleaned || 0);
  add(dryRun ? 'Manifests that would be quarantined' : 'Manifests quarantined',
    stats.manifests_quarantined || 0);
  add(dryRun ? 'Blobs that would be quarantined' : 'Blobs quarantined',
    stats.blobs_quarantined || 0);
  if (!dryRun) {
    add('Restored', stats.unquarantined || 0);
    add('Blobs swept', stats.blobs_swept || 0);
  }
  add(dryRun ? 'Reclaimable' : 'Reclaimed', bytes(stats.bytes_reclaimed || 0));
  add('Duration', stats.duration || '—');
  dialog.append(el('div', { class: 'card', style: 'margin:14px 0' }, el('table', {}, rows)));

  if ((stats.candidates || []).length) {
    dialog.append(el('p', { class: 'field-hint',
      text: 'Quarantined objects stop being served but remain recoverable until the ' +
            'quarantine window expires. Nothing that is deployed is ever a candidate.' }));
  }
  if ((stats.errors || []).length) {
    dialog.append(el('p', { class: 'dialog-error', text: stats.errors.join('\n') }));
  }

  dialog.append(el('div', { class: 'dialog-actions' },
    el('button', { type: 'button', class: 'btn btn-primary', text: 'Close',
      onclick: () => { overlay.remove(); route(); } })));
}

function collectDialog() {
  openDialog({
    title: 'Run garbage collection',
    hint: 'Unreferenced objects are quarantined, and objects already quarantined ' +
          'past the window have their bytes removed. Pulls and pushes continue ' +
          'throughout. Anything deployed, pinned, or newer than the grace period ' +
          'is never touched. Try a dry run first if you want to see the list.',
    fields: [],
    submitLabel: 'Collect now',
    danger: true,
    async onSubmit() { await runGC(false); },
  });
}

function reconcileDialog() {
  openDialog({
    title: 'Reconcile storage against the catalog',
    hint: 'Walks the blob store and compares it with the database. It reports and ' +
          'never deletes: dangling rows mean images that will fail to pull, while ' +
          'orphan bytes are only wasted cost.',
    fields: [],
    submitLabel: 'Reconcile',
    async onSubmit() {
      const report = await api('/gc/reconcile', { method: 'POST', body: {} });
      const overlay = el('div', { class: 'overlay' });
      const dangling = (report.dangling_rows || []).length;
      const orphans = (report.orphan_bytes || []).length;
      overlay.append(el('div', { class: 'dialog dialog-wide' },
        el('h2', { text: 'Reconcile complete' }),
        el('p', { class: 'dialog-hint',
          text: report.blobs_in_storage + ' blob(s) in storage, ' +
                report.blobs_in_catalog + ' in the catalog.' }),
        dangling
          ? el('div', { class: 'notice notice-bad' },
              el('strong', { text: dangling + ' catalog row(s) have no stored content' }),
              'Images referencing these will fail to pull.')
          : el('div', { class: 'notice notice-info' },
              el('strong', { text: 'No dangling rows' }),
              'Every catalogued blob has its content.'),
        orphans
          ? el('div', { class: 'notice notice-warn' },
              el('strong', { text: orphans + ' stored file(s) with no catalog row' }),
              bytes(report.orphan_byte_count || 0) + ' wasted. No image is affected.')
          : el('div', { class: 'notice notice-info' },
              el('strong', { text: 'No orphan bytes' }),
              'Nothing stored that the catalog does not know about.'),
        el('div', { class: 'dialog-actions' },
          el('button', { type: 'button', class: 'btn btn-primary', text: 'Close',
            onclick: () => overlay.remove() }))));
      document.body.append(overlay);
    },
  });
}

// -------------------------------------------------------------------- router

const routes = [
  { pattern: /^\/?$/, render: renderOverview, nav: 'overview' },
  { pattern: /^\/repositories\/(.+)$/, render: (m) => renderRepository(m[1]), nav: 'repositories' },
  { pattern: /^\/repositories\/?$/, render: renderRepositories, nav: 'repositories' },
  { pattern: /^\/organizations\/?$/, render: renderOrganizations, nav: 'organizations' },
  { pattern: /^\/identities\/?$/, render: renderIdentities, nav: 'identities' },
  { pattern: /^\/storage\/?$/, render: renderStorage, nav: 'storage' },
];

async function route() {
  if (!auth.get()) { showLogin(); return; }

  const path = decodeURIComponent(location.hash.replace(/^#/, '')) || '/';
  for (const candidate of routes) {
    const match = candidate.pattern.exec(path);
    if (!match) continue;

    for (const link of document.querySelectorAll('#nav a')) {
      link.classList.toggle('active', link.dataset.route === candidate.nav);
    }
    try {
      await candidate.render(match);
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) return;
      showError(error instanceof ApiError ? error
        : new ApiError(0, 'client_error', String(error && error.message || error)));
    }
    return;
  }

  clear(view());
  view().append(el('div', { class: 'card empty' },
    el('div', { class: 'empty-title', text: 'No such page' }),
    el('p', {}, el('a', { href: '#/', text: 'Back to the overview' }))));
}

// ---------------------------------------------------------------------- boot

function showLogin(message) {
  document.getElementById('app').hidden = true;
  const login = document.getElementById('login');
  login.hidden = false;
  const error = document.getElementById('login-error');
  if (message) { error.textContent = message; error.hidden = false; }
  else { error.hidden = true; }
}

async function showApp() {
  document.getElementById('login').hidden = true;
  document.getElementById('app').hidden = false;

  // Before rendering anything: whether this deployment permits writes decides
  // which controls exist at all.
  await loadCapabilities();
  if (capabilities.readOnly) {
    document.getElementById('read-only-note').hidden = false;
  }

  try {
    const version = await api('/version');
    document.getElementById('version-line').textContent =
      'Mantle ' + version.version + ' · API ' + version.api;
  } catch { /* the route below will surface anything real */ }
  document.getElementById('registry-name').textContent = location.host;
  await route();
}

document.getElementById('login-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const button = document.getElementById('login-submit');
  const error = document.getElementById('login-error');
  button.disabled = true;
  error.hidden = true;

  const username = document.getElementById('login-username').value.trim();
  const secret = document.getElementById('login-secret').value;
  auth.set(username || 'mantle', secret);

  try {
    // Verify before accepting, so a bad credential fails here rather than as a
    // confusing empty dashboard.
    await api('/version');
    document.getElementById('login-secret').value = '';
    await showApp();
  } catch (e) {
    auth.clear();
    error.textContent = e.status === 401
      ? 'Invalid username or credential.'
      : (e.message || 'Could not reach the registry.');
    error.hidden = false;
  } finally {
    button.disabled = false;
  }
});

document.getElementById('sign-out').addEventListener('click', () => {
  auth.clear();
  location.hash = '#/';
  showLogin();
});

window.addEventListener('hashchange', route);

if (auth.get()) showApp(); else showLogin();
