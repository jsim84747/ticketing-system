'use strict';
const $ = id => document.getElementById(id);
const eventID = new URLSearchParams(location.search).get('event') || document.documentElement.dataset.event || 'demo';
const useSSE = new URLSearchParams(location.search).get('transport') === 'sse';
// Test tools are for developers; the admin console links here with ?debug=1.
$('testing').hidden = new URLSearchParams(location.search).get('debug') !== '1';
$('mode').href = `/?event=${encodeURIComponent(eventID)}${useSSE ? '' : '&transport=sse'}${location.search.includes('debug=1') ? '&debug=1' : ''}`;
$('mode').textContent = useSSE ? 'Polling mode' : 'SSE mode';
const base = `/events/${encodeURIComponent(eventID)}`;
$('event-id').textContent = eventID;
$('new-tab').href = `/?event=${encodeURIComponent(eventID)}&debug=1`;
let session = sessionStorage.getItem('session'), state = 'absent', admission = '', busy = false;
let initial = Number(sessionStorage.getItem(`initial:${eventID}`)) || 0;
let pollTimer, beatTimer, revision = 0, openAt = 0, mode = 'auto';
async function api(path, method = 'GET', extra = {}) {
  const response = await fetch(path, {method, headers: {...(session ? {Authorization: `Bearer ${session}`} : {}), ...extra}, signal: AbortSignal.timeout(7000)});
  const data = response.status === 204 ? {} : await response.json();
  if (!response.ok) {
    if (response.status === 401) { sessionStorage.removeItem('session'); $('message').textContent = 'Your session expired. Click "Switch user" to start again.'; }
    throw new Error(data.error || `HTTP ${response.status}`);
  }
  return data;
}
function countdown(ms) {
  const s = Math.max(0, Math.ceil(ms / 1000)), h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), r = s % 60;
  return h ? `${h}:${String(m).padStart(2, '0')}:${String(r).padStart(2, '0')}` : `${m}:${String(r).padStart(2, '0')}`;
}
const opensSoon = () => mode === 'manual' && openAt > 0;
const doors = () => new Date(openAt).toLocaleTimeString('en-US', {hour: 'numeric', minute: '2-digit'});
// Ticks every second between polls so the countdown moves smoothly.
setInterval(() => { if (state === 'waiting' && opensSoon()) $('eta').textContent = openAt > Date.now() ? `Opens in ${countdown(openAt - Date.now())}` : 'Opening now'; }, 1000);
function render(v) {
  state = v.status; admission = v.token || '';
  if (v.mode) { mode = v.mode; openAt = v.open_at || 0; }
  $('connection').textContent = 'Live';
  if (v.name) document.querySelectorAll('.ticket-name').forEach(el => { el.textContent = v.name; });
  $('waiting-view').hidden = state === 'admitted' || state === 'completed';
  $('purchase-view').hidden = state !== 'admitted'; $('complete-view').hidden = state !== 'completed';
  $('join').hidden = state === 'waiting'; $('leave').hidden = !['waiting', 'admitted'].includes(state);
  if (state === 'waiting') {
    if (!initial || v.position > initial) { initial = v.position; sessionStorage.setItem(`initial:${eventID}`, String(initial)); }
    const progress = Math.max(0, Math.min(99, (1 - v.position / initial) * 100));
    $('position').textContent = `#${v.position.toLocaleString('en-US')}`; $('caption').textContent = 'Your place in line';
    $('ahead').textContent = v.ahead.toLocaleString('en-US');
    $('eta').textContent = opensSoon() ? (openAt > Date.now() ? `Opens in ${countdown(openAt - Date.now())}` : 'Opening now') : v.mode === 'manual' ? 'Set by organizer' : v.estimated_wait_seconds == null ? 'Calculating' : v.estimated_wait_seconds < 60 ? `About ${v.estimated_wait_seconds} sec` : `About ${Math.ceil(v.estimated_wait_seconds / 60)} min`;
    $('detail').textContent = opensSoon() ? `Doors open at ${doors()}. Stay on this page and you'll be let in in order.` : v.mode === 'manual' ? 'The organizer is letting people in. Your place is saved, even if you refresh.' : 'Your place is saved, even if you refresh.';
    $('progress').value = progress; $('progress-text').textContent = `${Math.floor(progress)}%`;
  } else if (state === 'admitted') {
    $('expires').textContent = new Date(v.expires_at).toLocaleTimeString('en-US');
    $('message').textContent = $('pause').checked ? 'Heartbeat paused. Your spot will expire in 30 seconds.' : 'Checking your connection every 10 seconds.';
  } else if (state === 'completed') {
    $('reservation').textContent = `Reservation number: ${v.reservation_id}`; $('message').textContent = '';
  } else {
    initial = 0; sessionStorage.removeItem(`initial:${eventID}`); $('position').textContent = '—'; $('ahead').textContent = '—'; $('eta').textContent = 'Calculating'; $('progress').value = 0; $('progress-text').textContent = '0%';
    $('caption').textContent = state === 'expired' ? 'Your spot expired' : 'Not in line yet';
    $('detail').textContent = opensSoon() ? `The line is open. Doors open at ${doors()}; join now to get your place.` : 'Click the button below to join the line.';
  }
}
async function poll() {
  try { if (!busy) { const version = revision; const v = await api(`${base}/queue/status`); if (!busy && version === revision) render(v); } }
  catch (e) { $('connection').textContent = 'Reconnecting'; $('message').textContent = `Please check your connection: ${e.message}`; }
  finally { pollTimer = setTimeout(poll, 2000); }
}
async function stream() {
  $('transport-label').textContent = 'Receiving updates over SSE';
  let reader;
  try {
    const response = await fetch(`${base}/queue/stream`, {headers: {Authorization: `Bearer ${session}`}});
    if (!response.ok || !response.body) throw new Error('stream unavailable');
    reader = response.body.getReader(); const decoder = new TextDecoder(); let pending = '';
    while (true) {
      const version = revision; const {value, done} = await reader.read();
      if (done) throw new Error('stream closed');
      pending += decoder.decode(value, {stream: true});
      let split;
      while ((split = pending.indexOf('\n\n')) >= 0) {
        const frame = pending.slice(0, split); pending = pending.slice(split + 2);
        const data = frame.split('\n').find(line => line.startsWith('data: '));
        if (data && !busy && version === revision) render(JSON.parse(data.slice(6)));
      }
    }
  } catch (e) {
    $('message').textContent = 'SSE disconnected, switched to checking every 2 seconds.';
    $('transport-label').textContent = 'Checking your place every 2 seconds'; poll();
  } finally { if (reader) await reader.cancel().catch(() => {}); }
}
async function beat() {
  try { if (!busy && !$('pause').checked && ['waiting', 'admitted'].includes(state)) await api(`${base}/queue/heartbeat`, 'POST'); }
  catch (e) { $('connection').textContent = 'Heartbeat failed'; }
  finally { beatTimer = setTimeout(beat, 10000); }
}
async function act(button, fn) {
  if (busy) return; revision++; busy = true; button.disabled = true; $('message').textContent = '';
  try { await fn(); } catch (e) { $('message').textContent = e.message; }
  finally { busy = false; button.disabled = false; }
}
$('join').onclick = () => act($('join'), async () => render(await api(`${base}/queue`, 'POST')));
$('leave').onclick = () => act($('leave'), async () => { await api(`${base}/queue/leave`, 'POST'); render({status: 'absent'}); });
$('buy').onclick = () => act($('buy'), async () => { const v = await api(`${base}/reserve`, 'POST', {'X-Admission-Token': admission}); render({status: 'completed', ...v}); });
$('new-user').onclick = () => act($('new-user'), async () => {
  if (session) { try { await api(`${base}/queue/leave`, 'POST'); } catch (e) { if (!e.message.includes('session')) throw e; } }
  sessionStorage.clear(); location.reload();
});
$('pause').onchange = () => { $('message').textContent = $('pause').checked ? 'Heartbeat paused. You will be removed 30 seconds after the last heartbeat.' : 'Heartbeat resumed.'; if (!$('pause').checked) api(`${base}/queue/heartbeat`, 'POST').catch(() => {}); };
async function start() {
  try {
    if (!session) {const v = await api('/session', 'POST'); session = v.session_token; sessionStorage.setItem('session', session); sessionStorage.setItem('user_id', v.user_id);}
    $('user-id').textContent = `USER / ${(sessionStorage.getItem('user_id') || '').slice(0, 12)}`;
    if (useSSE) { stream(); } else { await poll(); } beatTimer = setTimeout(beat, 10000);
  } catch (e) { $('message').textContent = `Couldn't start. Please refresh the page: ${e.message}`; }
}
start();
