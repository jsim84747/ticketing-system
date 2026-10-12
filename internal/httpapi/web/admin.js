'use strict';
const $ = id => document.getElementById(id);
let token = sessionStorage.getItem('admin_token') || '', events = [], timer;
const num = n => Number(n).toLocaleString('ko-KR');
const ms = n => n < 1000 ? `${(n / 1000).toFixed(1)}초` : n < 60000 ? `${Math.round(n / 1000)}초` : `${Math.round(n / 60000)}분`;
async function api(path, method = 'GET', body) {
  const response = await fetch(path, {method, headers: {Authorization: `Bearer ${token}`, ...(body ? {'Content-Type': 'application/json'} : {})}, body: body && JSON.stringify(body), signal: AbortSignal.timeout(7000)});
  const data = response.status === 204 ? {} : await response.json();
  if (response.status === 401) { logout('비밀번호가 올바르지 않습니다.'); throw new Error('unauthorized'); }
  if (response.status === 404 && data.error === 'admin disabled') { logout('서버에 ADMIN_TOKEN이 설정되지 않아 관리자 기능이 꺼져 있습니다.'); throw new Error('disabled'); }
  if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
  return data;
}
function logout(message = '') {
  clearTimeout(timer); token = ''; sessionStorage.removeItem('admin_token');
  $('dashboard').hidden = true; $('login').hidden = false; $('login-message').textContent = message;
}
function selected() { return $('event').value || (events[0] && events[0].id); }
function renderEvents(list) {
  events = list;
  const current = selected();
  if ($('event').options.length !== list.length) {
    $('event').replaceChildren(...list.map(e => new Option(e.id, e.id)));
    if (current) $('event').value = current;
  }
  const e = list.find(x => x.id === selected());
  if (!e) return;
  const usage = e.active_limit ? Math.min(100, e.active / e.active_limit * 100) : 0;
  $('active').textContent = num(e.active); $('limit').textContent = `/ ${num(e.active_limit)} 명`;
  $('usage').value = usage; $('usage-text').textContent = `${Math.floor(usage)}%`;
  $('waiting').textContent = `${num(e.waiting)}명`; $('remaining').textContent = `${num(e.remaining)}장`;
  $('reserved').textContent = `${num(e.reserved)}건`; $('admissions').textContent = `${num(e.admissions)}명`;
  $('peak').textContent = `${num(e.peak_active)}명`; $('violations').textContent = `${num(e.order_violations)}건`;
  $('violations').parentElement.classList.toggle('alert', e.order_violations > 0);
  $('wait-avg').textContent = e.admissions ? ms(e.wait_total_ms / e.admissions) : '—';
  $('wait-max').textContent = e.admissions ? ms(e.wait_max_ms) : '—';
  const q = encodeURIComponent(e.id);
  $('user-page').href = `/?event=${q}`; $('test-page').href = `/?event=${q}&debug=1`;
}
function renderUsers(v) {
  $('users-count').textContent = v.total > v.users.length ? `${num(v.total)}명 중 ${v.users.length}명 표시` : `${num(v.total)}명`;
  $('users-empty').hidden = v.total > 0;
  $('users').replaceChildren(...v.users.map(id => {
    const row = document.createElement('tr'), cell = document.createElement('td'), action = document.createElement('td'), button = document.createElement('button');
    cell.textContent = id; button.textContent = '내보내기'; button.dataset.user = id;
    action.append(button); row.append(cell, action); return row;
  }));
}
async function refresh() {
  clearTimeout(timer);
  try {
    renderEvents(await api('/admin/api/events'));
    renderUsers(await api(`/admin/api/events/${encodeURIComponent(selected())}/active`));
    $('updated').textContent = `업데이트 ${new Date().toLocaleTimeString('ko-KR')}`; $('message').textContent = '';
  } catch (e) {
    if (!token) return;
    $('updated').textContent = '연결 끊김'; $('message').textContent = `불러오지 못했습니다: ${e.message}`;
  }
  if (token) timer = setTimeout(refresh, 2000);
}
function show() { $('login').hidden = true; $('dashboard').hidden = false; refresh(); }
$('login-form').onsubmit = e => {
  e.preventDefault(); token = $('password').value; $('password').value = '';
  sessionStorage.setItem('admin_token', token); $('login-message').textContent = ''; show();
};
$('logout').onclick = () => logout();
$('event').onchange = refresh;
$('users').onclick = async e => {
  const id = e.target.dataset && e.target.dataset.user;
  if (!id || !confirm(`사용자 ${id.slice(0, 12)}… 을(를) 내보낼까요? 자리는 다음 대기자에게 넘어갑니다.`)) return;
  e.target.disabled = true;
  try { await api(`/admin/api/events/${encodeURIComponent(selected())}/kick`, 'POST', {user_id: id}); refresh(); }
  catch (err) { $('message').textContent = `내보내지 못했습니다: ${err.message}`; e.target.disabled = false; }
};
if (token) show();
