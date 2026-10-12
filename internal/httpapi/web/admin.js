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
const countdown = ms => {
  const s = Math.max(0, Math.ceil(ms / 1000)), d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60), r = s % 60;
  return d ? `${d}d ${h}h ${m}m` : h ? `${h}h ${m}m ${r}s` : m ? `${m}m ${r}s` : `${r}s`;
};
// datetime-local wants local time without a zone, e.g. 2026-10-12T20:00.
// Times are entered and shown in this computer's time zone. They are stored as
// an absolute instant, so each user sees the same moment in their own zone.
const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
const zoneShort = date => new Intl.DateTimeFormat('en-US', {timeZoneName: 'short'}).formatToParts(date).find(p => p.type === 'timeZoneName').value;
$('tz-note').textContent = `Time zone: ${zone} (${zoneShort(new Date())}), from this computer's clock. Visitors see the opening time converted to their own time zone.`;
const localInput = date => new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
function renderSchedule(e) {
  const manual = e.mode === 'manual', scheduled = manual && e.open_at > 0;
  $('schedule').hidden = !manual; $('schedule-form').hidden = scheduled; $('scheduled').hidden = !scheduled;
  // Doors stay closed until the opening, so manual admitting is unavailable meanwhile.
  $('admit').hidden = !manual || scheduled;
  if (scheduled) {
    $('scheduled-time').textContent = new Date(e.open_at).toLocaleString('en-US', {dateStyle: 'medium', timeStyle: 'short'}) + ` ${zoneShort(new Date(e.open_at))}`;
    $('scheduled-left').textContent = `in ${countdown(e.open_at - Date.now())} (${zone}). People can line up now; nobody is let in until then, and anyone already inside is sent back to the front of the line.`;
  } else if (!$('open-at').value) {
    const next = new Date(Date.now() + 5 * 60000); next.setSeconds(0, 0);
    $('open-at').value = localInput(next);
  }
}
function renderMode(mode) {
  const manual = mode === 'manual';
  $('mode-auto').setAttribute('aria-checked', String(!manual)); $('mode-manual').setAttribute('aria-checked', String(manual));
  $('mode-badge').textContent = manual ? 'MANUAL' : 'AUTO'; $('mode-badge').classList.toggle('manual', manual);
  $('admit').hidden = !manual;
  $('mode-help').textContent = manual
    ? 'Nobody is let in automatically. Schedule an opening time, or admit people from the front of the line with the button below.'
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
  fillSettings(e, false); renderMode(e.mode); renderSchedule(e);
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
const fullName = v => [v.first_name, v.last_name].filter(Boolean).join(' ') || '—';
const when = ms => ms ? new Date(ms).toLocaleString('en-US', {dateStyle: 'medium', timeStyle: 'medium'}) : '—';
function renderReservations(v) {
  $('res-count').textContent = v.total > v.reservations.length ? `newest ${v.reservations.length} of ${num(v.total)}` : num(v.total);
  $('res-empty').hidden = v.total > 0;
  $('reservations').replaceChildren(...v.reservations.map(r => {
    const row = document.createElement('tr');
    for (const text of [fullName(r), r.email || '—', when(r.reserved_at), r.reservation_id]) {
      const cell = document.createElement('td'); cell.textContent = text; row.append(cell);
    }
    return row;
  }));
}
// Quote every field, and neutralize values a spreadsheet would run as a formula.
const csvCell = value => { let s = String(value ?? ''); if (/^[=+\-@\t\r]/.test(s)) s = `'${s}`; return `"${s.replace(/"/g, '""')}"`; };
$('csv').onclick = async () => {
  try {
    const v = await api(`/admin/api/events/${encodeURIComponent(selected())}/reservations`);
    const rows = [['First name', 'Last name', 'Email', 'Reserved at', 'Reservation number', 'User ID'],
      ...v.reservations.map(r => [r.first_name, r.last_name, r.email, r.reserved_at ? new Date(r.reserved_at).toISOString() : '', r.reservation_id, r.user_id])];
    const blob = new Blob([rows.map(r => r.map(csvCell).join(',')).join('\r\n')], {type: 'text/csv'});
    const link = Object.assign(document.createElement('a'), {href: URL.createObjectURL(blob), download: `reservations-${selected()}.csv`});
    link.click(); URL.revokeObjectURL(link.href);
  } catch (err) { $('message').textContent = `Couldn't download reservations: ${err.message}`; }
};
async function refresh() {
  clearTimeout(timer);
  try {
    renderEvents(await api('/admin/api/events'));
    renderUsers(await api(`/admin/api/events/${encodeURIComponent(selected())}/active`));
    renderReservations(await api(`/admin/api/events/${encodeURIComponent(selected())}/reservations?limit=100`));
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
$('schedule-form').onsubmit = async e => {
  e.preventDefault();
  const openAt = new Date($('open-at').value).getTime();
  if (!(openAt > Date.now())) { flash('mode-message', 'Pick a time in the future.', false); return; }
  try { await post('settings', {mode: 'manual', open_at: openAt}); flash('mode-message', 'Opening scheduled. Anyone already admitted was moved back to the front of the line.'); refresh(); }
  catch (err) { flash('mode-message', `Couldn't schedule: ${err.message}`, false); }
};
$('cancel-schedule').onclick = async () => {
  try { await post('settings', {open_at: 0}); flash('mode-message', 'Schedule cancelled. Still in manual mode.'); refresh(); }
  catch (err) { flash('mode-message', `Couldn't cancel: ${err.message}`, false); }
};
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
