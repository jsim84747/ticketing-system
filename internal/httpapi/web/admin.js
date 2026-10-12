'use strict';
const $ = id => document.getElementById(id);
let token = sessionStorage.getItem('admin_token') || '', events = [], timer, shownEvent = '';
const num = n => Number(n).toLocaleString('en-US');
const ms = n => n < 1000 ? `${(n / 1000).toFixed(1)} sec` : n < 60000 ? `${Math.round(n / 1000)} sec` : `${Math.round(n / 60000)} min`;
async function api(path, method = 'GET', body) {
  const response = await fetch(path, {method, headers: {Authorization: `Bearer ${token}`, ...(body ? {'Content-Type': 'application/json'} : {})}, body: body && JSON.stringify(body), signal: AbortSignal.timeout(7000)});
  const data = response.status === 204 ? {} : await response.json();
  if (response.status === 401) { logout('Incorrect password.'); throw new Error('unauthorized'); }
  if (response.status === 404 && data.error === 'admin disabled') { logout('The admin console is off because ADMIN_TOKEN is not set on the server.'); throw new Error('disabled'); }
  if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
  return data;
}
function logout(message = '') {
  clearTimeout(timer); token = ''; sessionStorage.removeItem('admin_token');
  $('dashboard').hidden = true; $('login').hidden = false; $('login-message').textContent = message;
}
function selected() { return $('event').value || (events[0] && events[0].id); }
function current() { return events.find(x => x.id === selected()); }
function flash(id, text, ok = true) { $(id).textContent = text; $(id).classList.toggle('ok', ok); }
// Fill the settings form from the server, but never while the admin is editing it.
function fillSettings(e, force) {
  if (!force && $('settings').contains(document.activeElement)) return;
  if (!force && shownEvent === e.id) return;
  shownEvent = e.id;
  $('set-name').value = e.name; $('set-limit').value = e.active_limit; $('set-remaining').value = e.remaining;
}
function renderMode(mode) {
  const manual = mode === 'manual';
  $('mode-auto').setAttribute('aria-checked', String(!manual)); $('mode-manual').setAttribute('aria-checked', String(manual));
  $('mode-badge').textContent = manual ? 'MANUAL' : 'AUTO'; $('mode-badge').classList.toggle('manual', manual);
  $('admit').hidden = !manual;
  $('mode-help').textContent = manual
    ? 'Nobody is let in automatically. Use the button below to admit people from the front of the line.'
    : 'The next person in line is let in automatically whenever a spot opens.';
}
function renderEvents(list) {
  events = list;
  const before = selected();
  if ($('event').options.length !== list.length) {
    $('event').replaceChildren(...list.map(e => new Option(e.id, e.id)));
    if (before) $('event').value = before;
  }
  const e = current();
  if (!e) return;
  const usage = e.active_limit ? Math.min(100, e.active / e.active_limit * 100) : 0;
  $('active').textContent = num(e.active); $('limit').textContent = `/ ${num(e.active_limit)} people`;
  $('usage').value = usage; $('usage-text').textContent = `${Math.floor(usage)}%`;
  $('waiting').textContent = num(e.waiting); $('remaining').textContent = num(e.remaining);
  $('reserved').textContent = num(e.reserved); $('admissions').textContent = num(e.admissions);
  $('peak').textContent = num(e.peak_active); $('violations').textContent = num(e.order_violations);
  $('violations').parentElement.classList.toggle('alert', e.order_violations > 0);
  $('wait-avg').textContent = e.admissions ? ms(e.wait_total_ms / e.admissions) : '—';
  $('wait-max').textContent = e.admissions ? ms(e.wait_max_ms) : '—';
  const q = encodeURIComponent(e.id);
  $('user-page').href = `/?event=${q}`; $('test-page').href = `/?event=${q}&debug=1`;
  fillSettings(e, false); renderMode(e.mode);
}
function renderUsers(v) {
  $('users-count').textContent = v.total > v.users.length ? `showing ${v.users.length} of ${num(v.total)}` : num(v.total);
  $('users-empty').hidden = v.total > 0;
  $('users').replaceChildren(...v.users.map(id => {
    const row = document.createElement('tr'), cell = document.createElement('td'), action = document.createElement('td'), button = document.createElement('button');
    cell.textContent = id; button.textContent = 'Remove'; button.dataset.user = id;
    action.append(button); row.append(cell, action); return row;
  }));
}
async function refresh() {
  clearTimeout(timer);
  try {
    renderEvents(await api('/admin/api/events'));
    renderUsers(await api(`/admin/api/events/${encodeURIComponent(selected())}/active`));
    $('updated').textContent = `Updated ${new Date().toLocaleTimeString('en-US')}`; $('message').textContent = '';
  } catch (e) {
    if (!token) return;
    $('updated').textContent = 'Disconnected'; $('message').textContent = `Couldn't load data: ${e.message}`;
  }
  if (token) timer = setTimeout(refresh, 2000);
}
async function post(path, body) { return api(`/admin/api/events/${encodeURIComponent(selected())}/${path}`, 'POST', body); }
function show() { $('login').hidden = true; $('dashboard').hidden = false; refresh(); }
$('login-form').onsubmit = e => {
  e.preventDefault(); token = $('password').value; $('password').value = '';
  sessionStorage.setItem('admin_token', token); $('login-message').textContent = ''; show();
};
$('logout').onclick = () => logout();
$('event').onchange = () => { shownEvent = ''; refresh(); };
$('settings').onsubmit = async e => {
  e.preventDefault();
  const body = {name: $('set-name').value.trim(), active_limit: Number($('set-limit').value), remaining: Number($('set-remaining').value)};
  try { await post('settings', body); flash('settings-message', 'Settings saved.'); shownEvent = ''; document.activeElement.blur(); refresh(); }
  catch (err) { flash('settings-message', `Couldn't save: ${err.message}`, false); }
};
async function setMode(mode) {
  try { await post('settings', {mode}); renderMode(mode); flash('mode-message', mode === 'manual' ? 'Manual mode is on.' : 'Auto mode is on.'); refresh(); }
  catch (err) { flash('mode-message', `Couldn't change mode: ${err.message}`, false); }
}
$('mode-auto').onclick = () => setMode('auto');
$('mode-manual').onclick = () => setMode('manual');
$('admit').onsubmit = async e => {
  e.preventDefault();
  try {
    const v = await post('admit', {count: Number($('admit-count').value)});
    flash('mode-message', v.admitted ? `Admitted ${v.admitted} ${v.admitted === 1 ? 'person' : 'people'}.` : 'Nobody was admitted: the line is empty or capacity is full.', v.admitted > 0);
    refresh();
  } catch (err) { flash('mode-message', `Couldn't admit: ${err.message}`, false); }
};
$('users').onclick = async e => {
  const id = e.target.dataset && e.target.dataset.user;
  if (!id || !confirm(`Remove user ${id.slice(0, 12)}…? Their spot goes to the next person in line.`)) return;
  e.target.disabled = true;
  try { await post('kick', {user_id: id}); refresh(); }
  catch (err) { $('message').textContent = `Couldn't remove user: ${err.message}`; e.target.disabled = false; }
};
if (token) show();
