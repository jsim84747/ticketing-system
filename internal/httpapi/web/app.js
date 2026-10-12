'use strict';
const $ = id => document.getElementById(id);
const eventID = new URLSearchParams(location.search).get('event') || document.documentElement.dataset.event || 'demo';
const useSSE = new URLSearchParams(location.search).get('transport') === 'sse';
// Test tools are for developers; the admin console links here with ?debug=1.
$('testing').hidden = new URLSearchParams(location.search).get('debug') !== '1';
$('mode').href = `/?event=${encodeURIComponent(eventID)}${useSSE ? '' : '&transport=sse'}${location.search.includes('debug=1') ? '&debug=1' : ''}`;
$('mode').textContent = useSSE ? '폴링 모드' : 'SSE 모드';
const base = `/events/${encodeURIComponent(eventID)}`;
$('event-id').textContent = eventID;
$('new-tab').href = `/?event=${encodeURIComponent(eventID)}&debug=1`;
let session = sessionStorage.getItem('session'), state = 'absent', admission = '', busy = false;
let initial = Number(sessionStorage.getItem(`initial:${eventID}`)) || 0;
let pollTimer, beatTimer, revision = 0;
async function api(path, method = 'GET', extra = {}) {
  const response = await fetch(path, {method, headers: {...(session ? {Authorization: `Bearer ${session}`} : {}), ...extra}, signal: AbortSignal.timeout(7000)});
  const data = response.status === 204 ? {} : await response.json();
  if (!response.ok) {
    if (response.status === 401) { sessionStorage.removeItem('session'); $('message').textContent = '세션이 만료되었습니다. 사용자 바꾸기를 눌러주세요.'; }
    throw new Error(data.error || `HTTP ${response.status}`);
  }
  return data;
}
function render(v) {
  state = v.status; admission = v.token || '';
  $('connection').textContent = '실시간 연결됨';
  $('waiting-view').hidden = state === 'admitted' || state === 'completed';
  $('purchase-view').hidden = state !== 'admitted'; $('complete-view').hidden = state !== 'completed';
  $('join').hidden = state === 'waiting'; $('leave').hidden = !['waiting', 'admitted'].includes(state);
  if (state === 'waiting') {
    if (!initial || v.position > initial) { initial = v.position; sessionStorage.setItem(`initial:${eventID}`, String(initial)); }
    const progress = Math.max(0, Math.min(99, (1 - v.position / initial) * 100));
    $('position').textContent = v.position.toLocaleString('ko-KR'); $('caption').textContent = '현재 나의 대기 순번';
    $('ahead').textContent = `${v.ahead.toLocaleString('ko-KR')}명`;
    $('eta').textContent = v.estimated_wait_seconds == null ? '계산 중' : v.estimated_wait_seconds < 60 ? `약 ${v.estimated_wait_seconds}초` : `약 ${Math.ceil(v.estimated_wait_seconds / 60)}분`;
    $('detail').textContent = '새로고침해도 순번이 유지됩니다.';
    $('progress').value = progress; $('progress-text').textContent = `${Math.floor(progress)}%`;
  } else if (state === 'admitted') {
    $('expires').textContent = new Date(v.expires_at).toLocaleTimeString('ko-KR');
    $('message').textContent = $('pause').checked ? 'heartbeat 중단 중입니다. 30초 후 입장이 만료됩니다.' : '10초마다 연결을 확인하고 있어요.';
  } else if (state === 'completed') {
    $('reservation').textContent = `예약 번호: ${v.reservation_id}`; $('message').textContent = '새 사용자를 만들어 다시 테스트할 수 있어요.';
  } else {
    initial = 0; sessionStorage.removeItem(`initial:${eventID}`); $('position').textContent = '—'; $('ahead').textContent = '—'; $('eta').textContent = '계산 중'; $('progress').value = 0; $('progress-text').textContent = '0%';
    $('caption').textContent = state === 'expired' ? '입장 시간이 만료되었어요' : '대기실 입장 전';
    $('detail').textContent = '아래 버튼을 눌러 대기열에 참여하세요.';
  }
}
async function poll() {
  try { if (!busy) { const version = revision; const v = await api(`${base}/queue/status`); if (!busy && version === revision) render(v); } }
  catch (e) { $('connection').textContent = '재연결 중'; $('message').textContent = `연결을 확인해 주세요: ${e.message}`; }
  finally { pollTimer = setTimeout(poll, 2000); }
}
async function stream() {
  $('transport-label').textContent = 'SSE로 순번을 수신합니다';
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
    $('message').textContent = 'SSE 연결이 끊겨 2초 폴링으로 전환했습니다.';
    $('transport-label').textContent = '2초마다 순번을 확인합니다'; poll();
  } finally { if (reader) await reader.cancel().catch(() => {}); }
}
async function beat() {
  try { if (!busy && !$('pause').checked && ['waiting', 'admitted'].includes(state)) await api(`${base}/queue/heartbeat`, 'POST'); }
  catch (e) { $('connection').textContent = 'heartbeat 실패'; }
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
$('pause').onchange = () => { $('message').textContent = $('pause').checked ? 'heartbeat를 중단했습니다. 마지막 heartbeat로부터 30초 후 자동 퇴장합니다.' : 'heartbeat를 다시 보냅니다.'; if (!$('pause').checked) api(`${base}/queue/heartbeat`, 'POST').catch(() => {}); };
async function start() {
  try {
    if (!session) {const v = await api('/session', 'POST'); session = v.session_token; sessionStorage.setItem('session', session); sessionStorage.setItem('user_id', v.user_id);}
    $('user-id').textContent = `USER / ${(sessionStorage.getItem('user_id') || '').slice(0, 12)}`;
    if (useSSE) { stream(); } else { await poll(); } beatTimer = setTimeout(beat, 10000);
  } catch (e) { $('message').textContent = `시작하지 못했습니다. 새로고침해 주세요: ${e.message}`; }
}
start();
