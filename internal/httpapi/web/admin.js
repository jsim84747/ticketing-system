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
  $('set-starts').value = e.starts_at ? localInput(new Date(e.starts_at)) : ''; $('set-venue').value = e.venue || ''; $('set-description').value = e.description || '';
  $('set-name').value = e.name; $('set-limit').value = e.active_limit; $('set-remaining').value = e.remaining; $('set-per-order').value = e.max_per_order;
  const preset = ['', 'Open', 'Closed'].includes(e.badge);
  setBadgeChoice(preset ? (e.badge || 'Open') : 'other'); $('set-badge').value = preset ? '' : e.badge;
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
// Before opening: let people line up early, or lock the join button until the time.
let lockChoice = false;
function setLockChoice(lock) {
  lockChoice = lock;
  document.querySelectorAll('[data-lock]').forEach(b => b.setAttribute('aria-checked', String((b.dataset.lock === '1') === lock)));
  $('lock-help').textContent = lock
    ? 'Like a real ticket sale: the Join button is greyed out with a countdown and unlocks at the exact time. Whoever clicks first gets the earliest place.'
    : 'People can join the line now and keep their place; admission starts at the opening time.';
}
document.querySelectorAll('[data-lock]').forEach(b => { b.onclick = () => setLockChoice(b.dataset.lock === '1'); });
setLockChoice(false);
function renderSchedule(e) {
  const manual = e.mode === 'manual', scheduled = manual && e.open_at > 0;
  $('schedule').hidden = !manual; $('schedule-form').hidden = scheduled; $('scheduled').hidden = !scheduled;
  // Doors stay closed until the opening, so manual admitting is unavailable meanwhile.
  $('admit').hidden = !manual || scheduled;
  if (scheduled) {
    $('scheduled-time').textContent = new Date(e.open_at).toLocaleString('en-US', {dateStyle: 'medium', timeStyle: 'short'}) + ` ${zoneShort(new Date(e.open_at))}`;
    $('scheduled-left').textContent = `in ${countdown(e.open_at - Date.now())} (${zone}). ` + (e.join_lock
      ? 'The Join button is locked until then; it unlocks at the exact time.'
      : 'People can line up now; nobody is let in until then, and anyone already inside is sent back to the front of the line.');
  } else if (!$('open-at').value) {
    const next = new Date(Date.now() + 5 * 60000); next.setSeconds(0, 0);
    $('open-at').value = localInput(next);
  }
}
// Sale status: Open and Closed are presets; Other reveals a free-text box.
let badgeChoice = 'Open';
function setBadgeChoice(choice) {
  badgeChoice = choice;
  document.querySelectorAll('[data-badge]').forEach(b => b.setAttribute('aria-checked', String(b.dataset.badge === choice)));
  $('set-badge').hidden = choice !== 'other';
}
document.querySelectorAll('[data-badge]').forEach(b => { b.onclick = () => { setBadgeChoice(b.dataset.badge); if (b.dataset.badge === 'other') $('set-badge').focus(); }; });
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
    if (before && list.some(e => e.id === before)) $('event').value = before;
  }
  // Show names, mark the main event; refresh labels every round since names change.
  for (const opt of $('event').options) {
    const ev = list.find(x => x.id === opt.value);
    if (ev) opt.textContent = `${ev.name || ev.id}${ev.name ? ` (${ev.id})` : ''}${ev.default ? ' ★ main' : ''}`;
  }
  const e = current();
  if (!e) return;
  $('make-main').hidden = e.default;
  $('delete-event').disabled = list.length <= 1; $('delete-event').title = list.length <= 1 ? 'Keep at least one event' : '';
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
// Only one row is edited at a time; the live refresh leaves the table alone
// while it is open so it never overwrites what the admin is typing.
let editing = '';
function cell(text) { const td = document.createElement('td'); td.textContent = text; return td; }
function input(value, label, type = 'text') { return Object.assign(document.createElement('input'), {value: value || '', type, ariaLabel: label, required: true}); }
function button(text, action, cls = '') { return Object.assign(document.createElement('button'), {type: 'button', textContent: text, className: cls, onclick: action}); }
function reservationRow(r) {
  const row = document.createElement('tr');
  if (editing !== r.user_id) {
    row.append(cell(fullName(r)), cell(r.email || '—'), cell(String(r.quantity || 1)), cell(when(r.reserved_at)), cell(r.reservation_id));
    const actions = document.createElement('td');
    actions.append(button('Edit', () => { editing = r.user_id; $('res-message').textContent = ''; row.replaceWith(reservationRow(r)); }),
      button('Delete', async e => {
        if (!confirm(`Delete the reservation for ${fullName(r)}? Their ticket goes back on sale. This can't be undone.`)) return;
        e.target.disabled = true;
        try { await api(`/admin/api/events/${encodeURIComponent(selected())}/reservations/${encodeURIComponent(r.user_id)}`, 'DELETE'); flash('res-message', `Deleted the reservation for ${fullName(r)}.`); refresh(); }
        catch (err) { flash('res-message', `Couldn't delete: ${err.message}`, false); e.target.disabled = false; }
      }, 'danger'));
    row.append(actions);
    return row;
  }
  const first = input(r.first_name, 'First name'), last = input(r.last_name, 'Last name'), email = input(r.email, 'Email');
  email.required = false; // admins may record any contact detail, or none
  const names = document.createElement('td'), mail = document.createElement('td'), actions = document.createElement('td');
  const box = document.createElement('div'); box.className = 'name-edit'; box.append(first, last); names.append(box); mail.append(email);
  const save = button('Save', async () => {
    save.disabled = true;
    try {
      await api(`/admin/api/events/${encodeURIComponent(selected())}/reservations/${encodeURIComponent(r.user_id)}`, 'PUT',
        {first_name: first.value.trim(), last_name: last.value.trim(), email: email.value.trim()});
      editing = ''; flash('res-message', 'Reservation updated.'); refresh();
    } catch (err) { flash('res-message', `Couldn't save: ${err.message}`, false); save.disabled = false; }
  }, 'save');
  const cancel = button('Cancel', () => { editing = ''; $('res-message').textContent = ''; refresh(); });
  actions.append(save, cancel);
  for (const field of [first, last, email]) field.onkeydown = e => { if (e.key === 'Enter') save.click(); if (e.key === 'Escape') cancel.click(); };
  row.append(names, mail, cell(String(r.quantity || 1)), cell(when(r.reserved_at)), cell(r.reservation_id), actions);
  setTimeout(() => first.focus());
  return row;
}
function renderReservations(v) {
  $('res-count').textContent = v.total > v.reservations.length ? `newest ${v.reservations.length} of ${num(v.total)}` : num(v.total);
  $('res-empty').hidden = v.total > 0;
  if (editing) return;
  $('reservations').replaceChildren(...v.reservations.map(reservationRow));
}
// Quote every field, and neutralize values a spreadsheet would run as a formula.
const csvCell = value => { let s = String(value ?? ''); if (/^[=+\-@\t\r]/.test(s)) s = `'${s}`; return `"${s.replace(/"/g, '""')}"`; };
$('csv').onclick = async () => {
  try {
    const v = await api(`/admin/api/events/${encodeURIComponent(selected())}/reservations`);
    const rows = [['First name', 'Last name', 'Email', 'Tickets', 'Reserved at', 'Reservation number', 'User ID'],
      ...v.reservations.map(r => [r.first_name, r.last_name, r.email, r.quantity || 1, r.reserved_at ? new Date(r.reserved_at).toISOString() : '', r.reservation_id, r.user_id])];
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
  if (token) timer = setTimeout(refresh, 1000);
}
async function post(path, body) { return api(`/admin/api/events/${encodeURIComponent(selected())}/${path}`, 'POST', body); }
function show() { $('login').hidden = true; $('dashboard').hidden = false; refresh(); }
$('login-form').onsubmit = e => {
  e.preventDefault(); token = $('password').value; $('password').value = '';
  sessionStorage.setItem('admin_token', token); $('login-message').textContent = ''; show();
};
$('logout').onclick = () => logout();
$('event').onchange = () => { shownEvent = ''; editing = ''; refresh(); };
$('settings').onsubmit = async e => {
  e.preventDefault();
  const badge = badgeChoice === 'other' ? $('set-badge').value.trim() : badgeChoice;
  if (!badge) { flash('settings-message', 'Type a sale status, or pick Open or Closed.', false); return; }
  const starts = $('set-starts').value ? new Date($('set-starts').value).getTime() : 0;
  const body = {starts_at: starts, venue: $('set-venue').value.trim(), description: $('set-description').value.trim(), name: $('set-name').value.trim(), active_limit: Number($('set-limit').value), remaining: Number($('set-remaining').value), max_per_order: Number($('set-per-order').value), badge};
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
  try { await post('settings', {mode: 'manual', open_at: openAt, join_lock: lockChoice}); flash('mode-message', 'Opening scheduled. Anyone already admitted was moved back to the front of the line.'); refresh(); }
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
$('kick-all').onclick = async () => {
  const e = current(), n = e ? e.active : 0;
  if (!n) { flash('users-message', 'No one is admitted right now.', false); return; }
  if (!confirm(`Remove all ${n} admitted ${n === 1 ? 'user' : 'users'}? Their spots go to the next people in line.`)) return;
  $('kick-all').disabled = true;
  try { const v = await post('kick-all'); flash('users-message', `Removed ${v.removed} ${v.removed === 1 ? 'user' : 'users'}. Their spots went to the next people in line.`); refresh(); }
  catch (err) { flash('users-message', `Couldn't remove users: ${err.message}`, false); }
  finally { $('kick-all').disabled = false; }
};
// ---- Event management ----
const slug = s => s.toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 64);
let idTouched = false;
$('new-event').onclick = () => { $('create-event').hidden = false; idTouched = false; $('create-message').textContent = ''; $('new-name').focus(); };
$('cancel-create').onclick = () => { $('create-event').hidden = true; };
$('new-name').oninput = () => { if (!idTouched) $('new-id').value = slug($('new-name').value); };
$('new-id').oninput = () => { idTouched = true; };
$('create-event').onsubmit = async e => {
  e.preventDefault();
  const id = $('new-id').value.trim();
  try {
    await api('/admin/api/events', 'POST', {id, name: $('new-name').value.trim(), tickets: Number($('new-tickets').value), active_limit: Number($('new-limit').value)});
    $('create-event').hidden = true; $('create-event').reset();
    await refresh(); $('event').value = id; shownEvent = ''; editing = ''; refresh();
    flash('event-message', `Created "${id}". Its visitor link is ${location.origin}/?event=${id}`);
  } catch (err) { flash('create-message', `Couldn't create: ${err.message}`, false); }
};
$('make-main').onclick = async () => {
  const id = selected();
  try { await post('default'); flash('event-message', `"${id}" is now the main event; ${location.origin}/ opens it.`); refresh(); }
  catch (err) { flash('event-message', `Couldn't change the main event: ${err.message}`, false); }
};
$('delete-event').onclick = async () => {
  const e = current(); if (!e) return;
  const label = e.name || e.id;
  if (!confirm(`Delete "${label}" and everything in it: its line, ${num(e.reserved)} reservations and settings? This can't be undone.`)) return;
  try { await api(`/admin/api/events/${encodeURIComponent(e.id)}`, 'DELETE'); $('event').value = ''; shownEvent = ''; editing = ''; flash('event-message', `Deleted "${label}".`); refresh(); }
  catch (err) { flash('event-message', `Couldn't delete: ${err.message}`, false); }
};
if (token) show();
