// PistKoordinat arayüzü. Derleme adımı yok: tek ES modülü, hash tabanlı yönlendirme.
// Her ekran yükleniyor / boş / hata durumlarını ayrı ayrı çizer.

const TZ = 'Europe/Istanbul';
const app = document.getElementById('app');

// ------------------------------------------------------------------ yardımcılar

function h(tag, attrs = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'style') el.style.cssText = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

const fmtTime = (s) => s ? new Date(s).toLocaleTimeString('tr-TR', { timeZone: TZ, hour: '2-digit', minute: '2-digit' }) : '–';
const fmtDateTime = (s) => s ? new Date(s).toLocaleString('tr-TR', { timeZone: TZ, day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' }) : '–';
const today = () => new Date().toLocaleDateString('sv-SE', { timeZone: TZ });
const num = (n) => new Intl.NumberFormat('tr-TR').format(n ?? 0);
const pct = (x) => `%${(x * 100).toFixed(1).replace('.', ',')}`;
const standOrder = (a, b) => a.localeCompare(b, 'tr', { numeric: true });
const plate = (code, off) => h('span', { class: 'plate' + (off ? ' off' : '') }, code || '—');

function uuid() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 0x0f) | 0x40; b[8] = (b[8] & 0x3f) | 0x80;
  const x = [...b].map((v) => v.toString(16).padStart(2, '0')).join('');
  return `${x.slice(0, 8)}-${x.slice(8, 12)}-${x.slice(12, 16)}-${x.slice(16, 20)}-${x.slice(20)}`;
}

const STATUS = {
  SCHEDULED: ['Planlı', 'plain'], DELAYED: ['Gecikmeli', 'late'], CANCELLED: ['İptal', 'stop'],
  LANDED: ['İndi', 'ok'], DEPARTED: ['Kalktı', 'ok'],
  PENDING: ['Atanmadı', 'stop'], ASSIGNED: ['Atandı', 'plain'], STARTED: ['Sürüyor', 'late'], DONE: ['Bitti', 'ok'],
};
const tag = (s) => { const [t, c] = STATUS[s] || [s, 'plain']; return h('span', { class: `tag ${c}` }, t); };
const TASK = { FUEL: 'Yakıt', BAGGAGE: 'Bagaj', CLEANING: 'Temizlik', BRIDGE: 'Köprü' };
const REASON = {
  NO_COMPATIBLE_STAND: 'Uyumlu stand yok',
  ALL_COMPATIBLE_STANDS_OCCUPIED: 'Uyumlu standların hepsi dolu',
  INVALID_TIMES: 'Saatler geçersiz',
  CAPACITY_EXHAUSTED: 'Ekip/araç kapasitesi dolu',
  NO_CREW_ON_SHIFT: 'Vardiyada ekip yok',
  NO_VALID_CERTIFICATE: 'Geçerli belgeli ekip yok',
  NO_CREW_OF_TYPE: 'Bu türde ekip yok',
};

function loading(rows = 4) { return h('div', { 'aria-busy': 'true' }, Array.from({ length: rows }, () => h('div', { class: 'skeleton' }))); }
function empty(msg, action) { return h('div', { class: 'state' }, h('p', {}, msg), action); }
function failure(err, retry) {
  return h('div', { class: 'state err', role: 'alert' },
    h('p', {}, err.message || 'İstek tamamlanamadı.'),
    retry && h('button', { class: 'btn ghost', onclick: retry }, 'Tekrar dene'));
}

let toastTimer;
function toast(msg, isErr) {
  document.querySelector('.toast')?.remove();
  const t = h('div', { class: 'toast' + (isErr ? ' err' : ''), role: 'status' }, msg);
  document.body.append(t);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.remove(), 4500);
}

// Bir panelin içeriğini async yükler; yükleniyor/hata durumlarını kendisi yönetir.
async function fill(container, loader, render) {
  container.replaceChildren(loading());
  try {
    const data = await loader();
    container.replaceChildren(render(data));
  } catch (err) {
    container.replaceChildren(failure(err, () => fill(container, loader, render)));
  }
}

// ------------------------------------------------------------------ oturum ve API

const store = {
  get auth() { try { return JSON.parse(localStorage.getItem('pk.auth')); } catch { return null; } },
  set auth(v) { v ? localStorage.setItem('pk.auth', JSON.stringify(v)) : localStorage.removeItem('pk.auth'); },
};

class ApiError extends Error {
  constructor(status, code, message) { super(message); this.status = status; this.code = code; }
}

let refreshing = null;
async function refreshToken() {
  const a = store.auth;
  if (!a?.refresh) throw new ApiError(401, 'no_session', 'Oturum süresi doldu.');
  refreshing ??= fetch('/api/v1/auth/refresh', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ refresh_token: a.refresh }) })
    .then(async (r) => {
      if (!r.ok) throw new ApiError(401, 'expired', 'Oturum süresi doldu, tekrar giriş yapın.');
      const t = await r.json();
      store.auth = { access: t.access_token, refresh: t.refresh_token, user: t.user };
    })
    .finally(() => { refreshing = null; });
  return refreshing;
}

async function api(path, { method = 'GET', body, headers = {}, retry = true } = {}) {
  const a = store.auth;
  let res;
  try {
    res = await fetch(path, {
      method,
      headers: { ...(body ? { 'Content-Type': 'application/json' } : {}), ...(a ? { Authorization: `Bearer ${a.access}` } : {}), ...headers },
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new ApiError(0, 'network', 'Sunucuya ulaşılamıyor. Bağlantınızı kontrol edin.');
  }
  if (res.status === 401 && retry && a?.refresh) {
    try { await refreshToken(); } catch (e) { logout(); throw e; }
    return api(path, { method, body, headers, retry: false });
  }
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) {
    const e = data?.error || {};
    throw new ApiError(res.status, e.code, e.message || `İstek başarısız (HTTP ${res.status}).`);
  }
  return data;
}

function logout() {
  const a = store.auth;
  if (a?.refresh) fetch('/api/v1/auth/logout', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ refresh_token: a.refresh }) }).catch(() => {});
  store.auth = null;
  stream.close();
  location.hash = '#/giris';
}

// ------------------------------------------------------------------ canlı akış (SSE)

const stream = {
  es: null, listeners: new Set(), state: 'idle',
  open() {
    const a = store.auth;
    if (!a || this.es) return;
    this.es = new EventSource(`/api/v1/stream?access_token=${encodeURIComponent(a.access)}`);
    this.es.onopen = () => this.set('on');
    this.es.onerror = () => {
      this.set('off');
      // token süresi dolduysa EventSource sürekli 401 alır; yenileyip yeniden bağlan
      this.close();
      setTimeout(() => refreshToken().catch(() => {}).finally(() => this.open()), 3000);
    };
    for (const t of ['flight.updated', 'allocation.run', 'allocation.changed', 'replan.applied', 'task.updated', 'assignment.run', 'notice', 'stand.updated']) {
      this.es.addEventListener(t, (e) => { const ev = JSON.parse(e.data); this.listeners.forEach((fn) => fn(ev)); });
    }
  },
  close() { this.es?.close(); this.es = null; },
  set(s) { this.state = s; document.querySelectorAll('.live').forEach((el) => { el.className = `live ${s}`; el.lastChild.textContent = s === 'on' ? 'Canlı' : 'Bağlantı yok'; }); },
  on(fn) { this.listeners.add(fn); return () => this.listeners.delete(fn); },
};

// ------------------------------------------------------------------ yönlendirme

const ROLE_NAME = { ops_controller: 'Operasyon kontrolörü', ground_chief: 'Yer hizmetleri şefi', crew_lead: 'Ekip lideri', airline_rep: 'Havayolu temsilcisi', management: 'Yönetim' };

const ROUTES = {
  pano: { title: 'Pano', roles: ['ops_controller', 'ground_chief', 'management'], view: viewDashboard },
  ucuslar: { title: 'Uçuşlar', roles: ['ops_controller', 'ground_chief', 'management', 'airline_rep'], view: viewFlights },
  standlar: { title: 'Standlar', roles: ['ops_controller', 'ground_chief', 'management', 'airline_rep'], view: viewStands },
  planlama: { title: 'Gecikme planlama', roles: ['ops_controller', 'ground_chief'], view: viewReplan },
  ekipler: { title: 'Ekipler ve görevler', roles: ['ground_chief', 'ops_controller', 'management'], view: viewCrews },
  gorevlerim: { title: 'Görevlerim', roles: ['crew_lead'], view: viewMyTasks },
  raporlar: { title: 'Raporlar', roles: ['management', 'ops_controller'], view: viewReports },
  denetim: { title: 'Denetim kaydı', roles: ['management'], view: viewAudit },
};

let cleanup = [];
function route() {
  cleanup.forEach((fn) => fn()); cleanup = [];
  const a = store.auth;
  const name = location.hash.replace(/^#\/?/, '').split('?')[0];
  if (!a) return viewLogin();
  const allowed = Object.entries(ROUTES).filter(([, r]) => r.roles.includes(a.user.role));
  const target = ROUTES[name] && ROUTES[name].roles.includes(a.user.role) ? name : allowed[0][0];
  if (target !== name) { location.replace(`#/${target}`); return; }
  if (a.user.role !== 'airline_rep') stream.open();

  const main = h('main', { id: 'main' });
  const rail = h('nav', { class: 'rail', 'aria-label': 'Ana menü' },
    h('div', { class: 'brand' }, plate('PK'), h('b', {}, 'PistKoordinat')),
    allowed.map(([key, r]) => h('a', { href: `#/${key}`, 'aria-current': key === target ? 'page' : null }, r.title)),
    h('div', { class: 'who' }, h('div', {}, a.user.full_name), h('div', {}, ROLE_NAME[a.user.role]),
      h('button', { onclick: logout }, 'Çıkış yap')));
  app.replaceChildren(h('div', { class: 'shell' }, rail, main));
  ROUTES[target].view(main, a.user);
}

function header(title, sub, ...right) {
  const clock = h('span', { class: 'clock', 'aria-label': 'Havalimanı saati' }, fmtTime(new Date()));
  const t = setInterval(() => { clock.textContent = fmtTime(new Date()); }, 15000);
  cleanup.push(() => clearInterval(t));
  const live = h('span', { class: `live ${stream.state}` }, h('i'), stream.state === 'on' ? 'Canlı' : 'Bağlanıyor');
  return h('div', { class: 'head' },
    h('div', {}, h('h1', {}, title), sub && h('p', {}, sub)),
    h('div', { style: 'display:flex;gap:16px;align-items:center;flex-wrap:wrap' }, ...right, live, clock));
}

// ------------------------------------------------------------------ giriş

const DEMO = [['ops@pist.test', 'Operasyon'], ['sef@pist.test', 'Şef'], ['ekip@pist.test', 'Ekip lideri'], ['havayolu@pist.test', 'Havayolu'], ['yonetim@pist.test', 'Yönetim']];

function viewLogin() {
  stream.close();
  const email = h('input', { type: 'email', name: 'email', autocomplete: 'username', required: true });
  const pw = h('input', { type: 'password', name: 'password', autocomplete: 'current-password', required: true });
  const msg = h('p', { class: 'small', role: 'alert', style: 'color:var(--stop)' });
  const btn = h('button', { class: 'btn', type: 'submit' }, 'Giriş yap');
  const form = h('form', {
    onsubmit: async (e) => {
      e.preventDefault(); msg.textContent = ''; btn.disabled = true;
      try {
        const t = await api('/api/v1/auth/login', { method: 'POST', body: { email: email.value, password: pw.value }, retry: false });
        store.auth = { access: t.access_token, refresh: t.refresh_token, user: t.user };
        location.hash = '#/'; route();
      } catch (err) {
        msg.textContent = err.code === 'invalid_credentials' ? 'E-posta ya da parola hatalı.' : err.message;
      } finally { btn.disabled = false; }
    },
  },
  plate('PK'),
  h('h1', {}, 'PistKoordinat'),
  h('p', { class: 'muted' }, 'Stand, kapı ve yer hizmeti operasyonu'),
  h('label', { class: 'f' }, 'E-posta', email),
  h('label', { class: 'f' }, 'Parola', pw),
  msg, btn,
  h('div', { class: 'demo' }, 'Demo hesaplar (parola Demo.2026!): ',
    DEMO.map(([e, n], i) => [i ? ', ' : '', h('button', { type: 'button', onclick: () => { email.value = e; pw.value = 'Demo.2026!'; } }, n)])));
  app.replaceChildren(h('div', { class: 'login' }, form));
  email.focus();
}

// ------------------------------------------------------------------ pano

function viewDashboard(main) {
  const strip = h('div', { class: 'strip' });
  const pending = h('div');
  const risks = h('div');
  const log = h('ol', { class: 'log', 'aria-label': 'Canlı olaylar' });
  main.append(header('Operasyon panosu', `${today()} günü, canlı`), strip,
    h('div', { class: 'split' },
      h('div', {},
        h('section', { class: 'panel' }, h('h2', {}, 'Plandan sapan uçuşlar'), pending),
        h('section', { class: 'panel' }, h('h2', {}, 'Turnaround riski'), risks)),
      h('section', { class: 'panel' }, h('h2', {}, 'Son olaylar'), log)));

  const loadStrip = () => fill(strip, () => api('/api/v1/dashboard'), (d) => {
    const items = [
      [num(d.flights), `uçuş (${d.arrivals} geliş, ${d.departures} gidiş)`],
      [num(d.delayed), `gecikmeli, ort. ${String(d.avg_delay_min).replace('.', ',')} dk`],
      [num(d.cancelled), 'iptal'],
      [`${d.allocated}/${d.turnarounds}`, 'turnaround standlı'],
      [num(d.on_stand_now), `şu an standda (${d.stands_active} aktif stand)`],
      [num(d.tasks.PENDING || 0), 'atanamayan görev'],
    ];
    const f = document.createDocumentFragment();
    items.forEach(([v, l]) => f.append(h('div', {}, h('strong', {}, v), h('span', {}, l))));
    return f;
  });
  const loadPending = () => fill(pending, () => api('/api/v1/replan/pending'), (d) => d.items.length === 0
    ? empty('Tüm uçuşlar planla uyumlu. Besleme yeni saat gönderdiğinde burada görünür.')
    : h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {}, h('th', {}, 'Uçuş'), h('th', {}, 'Plan'), h('th', {}, 'Besleme'), h('th', { class: 'num' }, 'Fark'), h('th'))),
      h('tbody', {}, d.items.slice(0, 8).map((p) => h('tr', {},
        h('td', {}, h('span', { class: 'fno' }, p.flight_no), ' ', h('span', { class: 'muted small' }, p.direction === 'ARR' ? 'geliş' : 'gidiş')),
        h('td', {}, fmtTime(p.planned_time)), h('td', {}, fmtTime(p.feed_time)),
        h('td', { class: 'num' }, h('span', { class: `tag ${p.diff_min > 0 ? 'late' : 'ok'}` }, `${p.diff_min > 0 ? '+' : ''}${p.diff_min} dk`)),
        h('td', {}, h('a', { href: `#/planlama?flight=${p.flight_id}&time=${encodeURIComponent(p.feed_time)}` }, 'Önizle'))))))));
  const loadRisks = () => fill(risks, () => api('/api/v1/risks'), (d) => {
    const list = d.risks.filter((r) => r.level === 'CRITICAL').concat(d.risks.filter((r) => r.level === 'WARNING')).slice(0, 8);
    if (!list.length) return empty('Hedef süreyi aşma riski olan turnaround yok.');
    return h('ul', { class: 'log' }, list.map((r) => h('li', {},
      h('span', { class: `tag ${r.level === 'CRITICAL' ? 'stop' : 'late'}` }, r.level === 'CRITICAL' ? 'Kritik' : 'Uyarı'),
      h('span', {}, `Turnaround ${r.turnaround_id}: ${r.message}`))));
  });
  loadStrip(); loadPending(); loadRisks();

  const describe = (ev) => {
    const d = ev.data || {};
    switch (ev.type) {
      case 'flight.updated': return `${d.flight_no} ${d.status === 'DELAYED' ? 'gecikmeli' : (STATUS[d.status]?.[0] || d.status).toLowerCase()}, tahmini ${fmtTime(d.est_time)}`;
      case 'task.updated': return `Görev ${d.task_id}: ${STATUS[d.state?.status]?.[0] || ''}${d.result === 'MERGED' ? ' (başka cihaz önce bildirmişti)' : ''}`;
      case 'replan.applied': return `Yeniden planlandı: ${d.turnarounds} turnaround, ${d.stand_changes} stand, ${d.task_changes} görev`;
      case 'allocation.run': return `Stand tahsisi: ${d.allocated} yerleşti, ${d.unallocated} yerleşemedi`;
      case 'assignment.run': return `Ekip ataması: ${d.assigned} görev, ${d.risks} risk`;
      case 'notice': return d.message;
      default: return ev.type;
    }
  };
  let pendingReload;
  cleanup.push(stream.on((ev) => {
    if (log.firstChild?.classList?.contains('state')) log.replaceChildren();
    log.prepend(h('li', {}, h('time', {}, fmtTime(ev.at)), h('span', {}, describe(ev))));
    while (log.children.length > 120) log.lastChild.remove();
    clearTimeout(pendingReload);
    pendingReload = setTimeout(() => { loadStrip(); if (ev.type !== 'notice') loadPending(); }, 1500);
  }));
  log.append(h('li', { class: 'state' }, h('span'), h('span', {}, 'Olaylar geldikçe burada listelenir.')));
}

// ------------------------------------------------------------------ uçuşlar

function viewFlights(main, user) {
  const date = h('input', { type: 'date', value: today() });
  const dir = h('select', {}, h('option', { value: '' }, 'Tümü'), h('option', { value: 'ARR' }, 'Geliş'), h('option', { value: 'DEP' }, 'Gidiş'));
  const st = h('select', {}, h('option', { value: '' }, 'Tümü'), ...['SCHEDULED', 'DELAYED', 'CANCELLED', 'LANDED', 'DEPARTED'].map((s) => h('option', { value: s }, STATUS[s][0])));
  const tail = h('input', { placeholder: 'TC-YKA', size: 9 });
  const body = h('div');
  const sub = user.role === 'airline_rep' ? `Yalnızca ${user.airline_code} uçuşları` : 'Gelen ve giden uçuşlar';
  main.append(header('Uçuş çizelgesi', sub),
    h('section', { class: 'panel' },
      h('form', { class: 'bar', onsubmit: (e) => { e.preventDefault(); load(); } },
        h('label', { class: 'f' }, 'Gün', date), h('label', { class: 'f' }, 'Yön', dir),
        h('label', { class: 'f' }, 'Durum', st), h('label', { class: 'f' }, 'Kuyruk no', tail),
        h('button', { class: 'btn' }, 'Listele')),
      body));

  let cursor = null;
  const tbody = h('tbody');
  const more = h('button', { class: 'btn ghost', onclick: () => page(true) }, 'Daha fazla göster');
  const qs = () => new URLSearchParams({ date: date.value, direction: dir.value, status: st.value, tail: tail.value.trim().toUpperCase(), limit: 60 });
  const row = (f) => h('tr', {},
    h('td', {}, h('span', { class: 'fno' }, f.flight_no)),
    h('td', {}, f.direction === 'ARR' ? `${f.other_airport} →` : `→ ${f.other_airport}`),
    h('td', {}, f.tail_number, h('div', { class: 'muted small' }, f.aircraft_type)),
    h('td', {}, fmtTime(f.sched_time)),
    h('td', {}, f.delay_min ? h('span', {}, fmtTime(f.est_time), ' ', h('span', { class: 'tag late' }, `+${f.delay_min}`)) : fmtTime(f.est_time)),
    h('td', {}, tag(f.status)),
    h('td', {}, f.stand_code ? plate(f.stand_code) : h('span', { class: 'muted' }, '—'), f.gate_code ? h('span', { class: 'muted small' }, ` ${f.gate_code}`) : ''));

  async function page(append) {
    if (!append) { cursor = null; body.replaceChildren(loading(6)); }
    more.disabled = true;
    try {
      const q = qs(); if (cursor) q.set('cursor', cursor);
      const d = await api(`/api/v1/flights?${q}`);
      cursor = d.next_cursor || null;
      if (!append) {
        if (!d.items.length) { body.replaceChildren(empty('Bu filtrelerle uçuş yok. Günü ya da filtreleri değiştirin.')); return; }
        tbody.replaceChildren();
        body.replaceChildren(h('div', { class: 'table-wrap' }, h('table', {},
          h('thead', {}, h('tr', {}, ['Uçuş', 'Rota', 'Uçak', 'Plan', 'Tahmini', 'Durum', 'Stand'].map((x) => h('th', {}, x)))), tbody)), more);
      }
      d.items.forEach((f) => tbody.append(row(f)));
      more.hidden = !cursor;
    } catch (err) {
      if (append) toast(err.message, true); else body.replaceChildren(failure(err, () => page(false)));
    } finally { more.disabled = false; }
  }
  const load = () => page(false);
  load();
}

// ------------------------------------------------------------------ standlar ve Gantt

function viewStands(main, user) {
  const date = h('input', { type: 'date', value: today() });
  const ganttBox = h('div');
  const runBox = h('div');
  const inv = h('div');
  const canRun = user.role === 'ops_controller';
  const runBtn = canRun && h('button', { class: 'btn', onclick: run }, 'Tahsisi yeniden çöz');
  main.append(header('Stand çizelgesi', 'Uçak boyutu, köprü ve 15 dakika tampon kurallarına göre', h('label', { class: 'f' }, 'Gün', date), runBtn),
    ganttBox, runBox,
    h('section', { class: 'panel' }, h('h2', {}, 'Stand envanteri'), inv));
  date.addEventListener('change', () => { loadGantt(); loadRun(); });

  function loadGantt() {
    fill(ganttBox, () => api(`/api/v1/gantt?date=${date.value}`), (d) => {
      if (!d.slots.length) return h('div', { class: 'panel' }, empty('Bu gün için tahsis yok.'));
      const day0 = new Date(`${d.op_date}T00:00:00+03:00`).getTime();
      const startH = 4, endH = 25;
      const span = (endH - startH) * 3600e3;
      const x = (t) => ((new Date(t).getTime() - day0 - startH * 3600e3) / span) * 100;
      const byStand = {};
      d.slots.forEach((s) => (byStand[s.stand_code] ||= []).push(s));
      const ticks = h('div', { class: 'ticks' });
      for (let hr = startH; hr <= endH; hr += 2) ticks.append(h('span', { style: `left:${((hr - startH) / (endH - startH)) * 100}%` }, `${String(hr % 24).padStart(2, '0')}:00`));
      const inner = h('div', { class: 'gantt-inner' }, h('div', { class: 'g-hours' }, h('div'), ticks));
      d.stands.slice().sort(standOrder).forEach((code) => {
        const lane = h('div', { class: 'g-lane' });
        (byStand[code] || []).forEach((s) => {
          const l = Math.max(0, x(s.on_block)); const r = Math.min(100, x(s.off_block));
          if (r <= 0 || l >= 100) return;
          lane.append(h('div', { class: 'g-slot buf', style: `left:${l}%;width:${Math.max(0.4, r - l)}%`,
            title: `${s.tail_number} ${s.flights}\n${fmtTime(s.on_block)}–${fmtTime(s.off_block)} (+15 dk tampon)` }, s.flights || s.tail_number));
        });
        inner.append(h('div', { class: 'g-row' }, plate(code), lane));
      });
      const nowX = x(new Date());
      if (nowX > 0 && nowX < 100 && d.op_date === today()) {
        inner.append(h('div', { class: 'g-now', style: `left:calc(70px + (100% - 70px) * ${nowX / 100})`, title: 'Şimdi' }));
      }
      return h('div', { class: 'gantt', role: 'img', 'aria-label': `${d.slots.length} tahsisli stand doluluk çizelgesi` }, inner);
    });
  }
  function renderRun(r, fresh) {
    if (!r) return document.createTextNode('');
    return h('section', { class: 'panel' },
      h('h2', {}, fresh ? 'Tahsis sonucu' : 'Son tahsis'),
      h('p', {}, `${r.allocated}/${r.total} turnaround yerleşti`, r.solve_ms != null ? `, çözüm ${String(r.solve_ms).replace('.', ',')} ms` : '', r.changed != null ? `, ${r.changed} stand değişti.` : '.'),
      r.unallocated?.length
        ? h('div', { class: 'table-wrap', style: 'margin-top:10px' }, h('table', {},
          h('thead', {}, h('tr', {}, h('th', {}, 'Turnaround'), h('th', {}, 'Uçak'), h('th', {}, 'Neden'), h('th', {}, 'Ayrıntı'))),
          h('tbody', {}, r.unallocated.map((u) => h('tr', {}, h('td', {}, u.turnaround_id), h('td', {}, u.tail_number),
            h('td', {}, h('span', { class: 'tag stop' }, REASON[u.reason] || u.reason)), h('td', { class: 'small' }, u.detail))))))
        : h('p', { class: 'muted', style: 'margin-top:6px' }, 'Yerleşemeyen uçuş yok.'));
  }
  function loadRun() {
    if (user.role === 'airline_rep') return;
    fill(runBox, () => api(`/api/v1/allocations/runs/latest?date=${date.value}`).catch((e) => { if (e.status === 404) return null; throw e; }), (r) => renderRun(r, false));
  }
  async function run() {
    runBtn.disabled = true;
    try {
      const r = await api('/api/v1/allocations/run', { method: 'POST', body: { date: date.value }, headers: { 'Idempotency-Key': uuid() } });
      runBox.replaceChildren(renderRun(r, true));
      toast(`Tahsis tamamlandı: ${r.allocated}/${r.total} yerleşti.`);
      loadGantt();
    } catch (err) { toast(err.message, true); } finally { runBtn.disabled = false; }
  }
  fill(inv, () => api('/api/v1/stands'), (d) => h('div', { class: 'table-wrap' }, h('table', {},
    h('thead', {}, h('tr', {}, ['Stand', 'Maks. boyut', 'Köprü', 'Kapı', 'Uyumlu tipler', 'Durum'].map((x) => h('th', {}, x)))),
    h('tbody', {}, d.items.slice().sort((a, b) => standOrder(a.code, b.code)).map((s) => {
      const toggle = canRun && h('button', { class: 'btn ghost', onclick: async (e) => {
        e.target.disabled = true;
        try { await api(`/api/v1/stands/${s.id}`, { method: 'PATCH', body: { active: !s.active } }); s.active = !s.active; toast(`${s.code} ${s.active ? 'kullanıma açıldı' : 'kapatıldı'}.`); viewStandsRefresh(); } catch (err) { toast(err.message, true); } finally { e.target.disabled = false; }
      } }, s.active ? 'Kapat' : 'Aç');
      return h('tr', {}, h('td', {}, plate(s.code, !s.active)), h('td', {}, s.max_size_class), h('td', {}, s.has_bridge ? 'Var' : '—'),
        h('td', {}, s.gate_code || '—'), h('td', { class: 'small' }, s.compatible_types.join(', ')),
        h('td', {}, s.active ? h('span', { class: 'tag ok' }, 'Açık') : h('span', { class: 'tag stop' }, 'Kapalı'), ' ', toggle));
    })))));
  const viewStandsRefresh = () => route();
  loadGantt(); loadRun();
}

// ------------------------------------------------------------------ gecikme planlama (what-if)

function viewReplan(main, user) {
  const params = new URLSearchParams(location.hash.split('?')[1] || '');
  const flight = h('input', { type: 'number', min: 1, required: true, value: params.get('flight') || '', style: 'width:110px' });
  const when = h('input', { type: 'datetime-local', required: true });
  if (params.get('time')) when.value = toLocalInput(params.get('time'));
  const pendingBox = h('div');
  const result = h('div');
  main.append(header('Gecikme planlama', 'Bir uçuşun yeni saatinin zincirleme etkisini uygulamadan önce görün'),
    h('div', { class: 'split' },
      h('section', { class: 'panel' },
        h('h2', {}, 'Ne olur?'),
        h('form', { class: 'bar', onsubmit: (e) => { e.preventDefault(); preview(); } },
          h('label', { class: 'f' }, 'Uçuş no (id)', flight), h('label', { class: 'f' }, 'Yeni saat', when),
          h('button', { class: 'btn' }, 'Önizle')),
        result),
      h('section', { class: 'panel' }, h('h2', {}, 'Plandan sapanlar'), pendingBox)));

  fill(pendingBox, () => api('/api/v1/replan/pending'), (d) => d.items.length === 0 ? empty('Plandan sapan uçuş yok.')
    : h('ul', { class: 'log' }, d.items.slice(0, 15).map((p) => h('li', {},
      h('span', { class: `tag ${p.diff_min > 0 ? 'late' : 'ok'}` }, `${p.diff_min > 0 ? '+' : ''}${p.diff_min}`),
      h('button', { class: 'btn ghost', style: 'justify-self:start', onclick: () => { flight.value = p.flight_id; when.value = toLocalInput(p.feed_time); preview(); } },
        `${p.flight_no} → ${fmtTime(p.feed_time)}`)))));

  let last = null;
  async function preview() {
    if (!flight.value || !when.value) return;
    result.replaceChildren(loading(5));
    try {
      const body = { flight_id: Number(flight.value), new_time: fromLocalInput(when.value) };
      const plan = await api('/api/v1/replan/preview', { method: 'POST', body });
      last = { body, plan };
      result.replaceChildren(renderPlan(plan, user.role === 'ops_controller'));
    } catch (err) { result.replaceChildren(failure(err)); }
  }
  async function apply(btn) {
    btn.disabled = true;
    try {
      const plan = await api('/api/v1/replan/apply', { method: 'POST', body: { ...last.body, fingerprint: last.plan.fingerprint }, headers: { 'Idempotency-Key': `replan-${last.plan.fingerprint}` } });
      toast(`Plan uygulandı: ${plan.turnarounds.length} turnaround, ${plan.stands.length} stand, ${plan.tasks.length} görev güncellendi.`);
      result.replaceChildren(h('p', {}, 'Plan uygulandı. Yeni bir uçuş seçebilirsiniz.'));
    } catch (err) {
      toast(err.code === 'plan_changed' ? 'Önizlemeden sonra plan değişti. Tekrar önizleyin.' : err.message, true);
      btn.disabled = false;
    }
  }
  function renderPlan(p, canApply) {
    const f = document.createDocumentFragment();
    if (!p.turnarounds.length) { f.append(empty('Bu saat mevcut planı değiştirmiyor; gecikme yer süresinde emiliyor.')); return f; }
    f.append(h('p', {}, `${p.turnarounds.length} turnaround kayıyor, ${p.stands.length} stand ve ${p.tasks.filter((t) => t.old_crew_id !== t.new_crew_id).length} ekip ataması değişiyor.`));
    f.append(h('h3', { style: 'margin:14px 0 6px' }, 'Turnaround saatleri'), h('div', { class: 'table-wrap' }, h('table', { class: 'diff' },
      h('thead', {}, h('tr', {}, ['Turnaround', 'Uçak', 'Eski blok', 'Yeni blok'].map((x) => h('th', {}, x)))),
      h('tbody', {}, p.turnarounds.map((t) => h('tr', {}, h('td', {}, t.turnaround_id), h('td', {}, t.tail_number),
        h('td', { class: 'old' }, `${fmtTime(t.old_on_block)}–${fmtTime(t.old_off_block)}`),
        h('td', { class: 'new' }, `${fmtTime(t.new_on_block)}–${fmtTime(t.new_off_block)}`)))))));
    if (p.stands.length) f.append(h('h3', { style: 'margin:14px 0 6px' }, 'Stand değişiklikleri'), h('ul', { class: 'log' },
      p.stands.map((s) => h('li', {}, h('span', {}, s.turnaround_id), h('span', {}, `Stand ${s.old_stand_id || '—'} → ${s.new_stand_id || 'yer yok'}`)))));
    if (p.unallocated?.length) f.append(h('p', { class: 'state err' }, `${p.unallocated.length} turnaround için uygun stand bulunamıyor.`));
    if (p.risks?.length) f.append(h('h3', { style: 'margin:14px 0 6px' }, 'Riskler'), h('ul', { class: 'log' },
      p.risks.map((r) => h('li', {}, h('span', { class: `tag ${r.level === 'CRITICAL' ? 'stop' : 'late'}` }, r.level === 'CRITICAL' ? 'Kritik' : 'Uyarı'), h('span', { class: 'small' }, r.message)))));
    if (canApply) {
      const b = h('button', { class: 'btn warn', style: 'margin-top:14px', onclick: (e) => apply(e.target) }, 'Planı uygula');
      f.append(b);
    } else f.append(h('p', { class: 'muted small', style: 'margin-top:12px' }, 'Uygulama yetkisi operasyon kontrolöründe.'));
    return f;
  }
  if (flight.value && when.value) preview();
}

function toLocalInput(iso) {
  const d = new Date(iso);
  const p = new Intl.DateTimeFormat('sv-SE', { timeZone: TZ, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(d);
  return p.replace(' ', 'T');
}
const fromLocalInput = (v) => new Date(`${v}:00+03:00`).toISOString();

// ------------------------------------------------------------------ ekipler

function viewCrews(main, user) {
  const crewSel = h('select', {}, h('option', { value: '' }, 'Tüm ekipler'));
  const tasksBox = h('div');
  const riskBox = h('div');
  const isChief = user.role === 'ground_chief';
  const runBtn = isChief && h('button', { class: 'btn', onclick: run }, 'Ekipleri yeniden ata');
  main.append(header('Ekipler ve görevler', 'Belge geçerliliği, 11 saat dinlenme ve 4 yakıt aracı sınırıyla', runBtn),
    h('section', { class: 'panel' }, h('h2', {}, 'Riskler ve kural ihlalleri'), riskBox),
    h('section', { class: 'panel' }, h('div', { class: 'bar' }, h('h2', { style: 'margin:0 auto 0 0' }, 'Görevler'), h('label', { class: 'f' }, 'Ekip', crewSel)), tasksBox));

  api('/api/v1/crews').then((d) => d.items.forEach((c) => {
    const expired = Object.values(c.certifications).some((v) => v < today());
    crewSel.append(h('option', { value: c.id }, `${c.name}${expired ? ' (belge süresi dolmuş)' : ''}`));
  })).catch(() => {});
  crewSel.addEventListener('change', loadTasks);

  function renderRisk(d) {
    const f = document.createDocumentFragment();
    const crit = d.risks.filter((r) => r.level === 'CRITICAL'); const warn = d.risks.filter((r) => r.level === 'WARNING');
    f.append(h('p', {}, `${d.assigned}/${d.tasks} görev atanabiliyor. ${crit.length} kritik, ${warn.length} uyarı.`));
    if (d.shift_violations.length) f.append(h('p', { class: 'small', style: 'margin-top:6px' },
      `Dinlenme kuralı: ${d.shift_violations.map((v) => `ekip ${v.crew_id} için ${String(v.rest_hours).replace('.', ',')} saat dinlenmeli vardiya kullanılmıyor`).join('; ')}.`));
    const reasons = {};
    d.unassigned.forEach((u) => { reasons[u.reason] = (reasons[u.reason] || 0) + 1; });
    if (Object.keys(reasons).length) f.append(h('p', { class: 'small muted', style: 'margin-top:6px' },
      'Atanamayan görev nedenleri: ', Object.entries(reasons).map(([k, n]) => `${REASON[k] || k} (${n})`).join(', ')));
    return f;
  }
  const loadRisk = () => fill(riskBox, () => api('/api/v1/risks'), renderRisk);
  function loadTasks() {
    const q = new URLSearchParams({ limit: 500 }); if (crewSel.value) q.set('crew_id', crewSel.value);
    fill(tasksBox, () => api(`/api/v1/tasks?${q}`), (d) => d.items.length === 0 ? empty('Bu seçim için görev yok.')
      : h('div', { class: 'table-wrap' }, h('table', {},
        h('thead', {}, h('tr', {}, ['Saat', 'Görev', 'Uçak / uçuş', 'Stand', 'Ekip', 'Araç', 'Durum'].map((x) => h('th', {}, x)))),
        h('tbody', {}, d.items.map((t) => h('tr', {},
          h('td', {}, `${fmtTime(t.planned_start)}–${fmtTime(t.planned_end)}`), h('td', {}, TASK[t.task_type]),
          h('td', {}, t.tail_number, h('div', { class: 'muted small' }, t.flights)), h('td', {}, t.stand_code ? plate(t.stand_code) : '—'),
          h('td', {}, t.crew_name || '—'), h('td', {}, t.equipment_code || ''), h('td', {}, tag(t.status))))))));
  }
  async function run() {
    runBtn.disabled = true;
    try {
      const r = await api('/api/v1/assignments/run', { method: 'POST', body: { date: today() }, headers: { 'Idempotency-Key': uuid() } });
      toast(`${r.assigned} görev atandı, ${r.changed} ekip değişti.`);
      riskBox.replaceChildren(renderRisk(r)); loadTasks();
    } catch (err) { toast(err.message, true); } finally { runBtn.disabled = false; }
  }
  loadRisk(); loadTasks();
}

// ------------------------------------------------------------------ ekip lideri: çevrimdışı çalışan görev ekranı

const queue = {
  get items() { try { return JSON.parse(localStorage.getItem('pk.queue')) || []; } catch { return []; } },
  set items(v) { localStorage.setItem('pk.queue', JSON.stringify(v)); },
  get device() { let d = localStorage.getItem('pk.device'); if (!d) { d = `tablet-${uuid().slice(0, 8)}`; localStorage.setItem('pk.device', d); } return d; },
};

async function flushQueue() {
  const items = queue.items;
  if (!items.length || !navigator.onLine) return null;
  const res = await api('/api/v1/sync/task-events', { method: 'POST', body: { device_id: queue.device, events: items.slice(0, 200) } });
  const done = new Set(res.results.map((r) => r.client_event_id));
  queue.items = queue.items.filter((e) => !done.has(e.client_event_id));
  return res.results;
}

function viewMyTasks(main) {
  const qbar = h('div');
  const list = h('div', { class: 'task-list' });
  main.append(header('Görevlerim', 'Ağ yokken de başladı/bitti girebilirsiniz; bağlantı gelince gönderilir.'), qbar, list);

  const cacheKey = `pk.mytasks.${today()}`;
  let tasks = [];
  try { tasks = JSON.parse(localStorage.getItem(cacheKey)) || []; } catch { /* yok */ }

  function renderQueue() {
    const n = queue.items.length;
    if (!n && navigator.onLine) { qbar.replaceChildren(); return; }
    qbar.replaceChildren(h('div', { class: 'queue', role: 'status' },
      h('span', {}, navigator.onLine ? `${n} olay gönderilmeyi bekliyor.` : `Çevrimdışı. ${n} olay cihazda saklanıyor.`),
      navigator.onLine && n ? h('button', { class: 'btn ghost', onclick: sync }, 'Şimdi gönder') : ''));
  }
  function applyLocal() {
    // kuyruktaki olayları yerel görünüme uygula (sunucu cevabı gelene kadar)
    const pending = queue.items;
    return tasks.map((t) => {
      let s = t.status;
      for (const e of pending.filter((x) => x.task_id === t.id)) s = e.type === 'FINISHED' ? 'DONE' : (s === 'DONE' ? s : 'STARTED');
      return { ...t, status: s };
    });
  }
  function render() {
    renderQueue();
    const view = applyLocal();
    if (!view.length) { list.replaceChildren(empty(navigator.onLine ? 'Bugün ekibinize atanmış görev yok.' : 'Çevrimdışısınız ve bu cihazda kayıtlı görev listesi yok.')); return; }
    const order = { STARTED: 0, ASSIGNED: 1, PENDING: 2, DONE: 3 };
    list.replaceChildren(...view.slice().sort((a, b) => (order[a.status] - order[b.status]) || a.planned_start.localeCompare(b.planned_start)).map((t) => {
      const act = t.status === 'ASSIGNED' ? h('button', { class: 'btn', onclick: () => record(t, 'STARTED') }, 'Başladı')
        : t.status === 'STARTED' ? h('button', { class: 'btn', onclick: () => record(t, 'FINISHED') }, 'Bitti')
          : tag(t.status);
      return h('article', { class: `task ${t.status === 'STARTED' ? 'started' : t.status === 'DONE' ? 'done' : ''}` },
        h('div', { class: 'when' }, fmtTime(t.planned_start)),
        h('div', {}, h('div', { style: 'font-weight:600' }, `${TASK[t.task_type]}, ${t.tail_number}`),
          h('div', { class: 'small muted' }, t.stand_code ? ['Stand ', plate(t.stand_code), ` · ${t.flights}`] : t.flights)),
        h('div', { class: 'act' }, act));
    }));
  }
  function record(t, type) {
    queue.items = [...queue.items, { client_event_id: uuid(), task_id: t.id, type, occurred_at: new Date().toISOString() }];
    render();
    sync();
  }
  async function sync() {
    if (!navigator.onLine) { render(); return; }
    try {
      const results = await flushQueue();
      if (results) {
        const merged = results.filter((r) => r.result === 'MERGED').length;
        const rejected = results.filter((r) => r.result === 'REJECTED');
        if (merged) toast(`${merged} güncelleme başka bir cihazdan önce bildirilmişti; sunucudaki durum gösteriliyor.`);
        if (rejected.length) toast(`${rejected.length} olay reddedildi: ${rejected[0].reason}`, true);
        results.forEach((r) => { const t = tasks.find((x) => x.id === r.task_id); if (t && r.state) t.status = r.state.status; });
        localStorage.setItem(cacheKey, JSON.stringify(tasks));
      }
      await load(false);
    } catch (err) {
      if (err.status === 0) render(); else toast(err.message, true);
    }
  }
  async function load(showLoading = true) {
    if (!navigator.onLine) { render(); return; }
    if (showLoading && !tasks.length) list.replaceChildren(loading(5));
    try {
      const d = await api('/api/v1/my/tasks?limit=500');
      tasks = d.items;
      localStorage.setItem(cacheKey, JSON.stringify(tasks));
      render();
    } catch (err) {
      if (tasks.length) { render(); toast(err.message, true); } else list.replaceChildren(failure(err, () => load()));
    }
  }
  const online = () => sync();
  const offline = () => render();
  window.addEventListener('online', online);
  window.addEventListener('offline', offline);
  cleanup.push(() => { window.removeEventListener('online', online); window.removeEventListener('offline', offline); });
  cleanup.push(stream.on((ev) => { if (ev.type === 'task.updated' && tasks.some((t) => t.id === ev.data.task_id)) load(false); }));
  render();
  sync();
}

// ------------------------------------------------------------------ raporlar

function viewReports(main, user) {
  let group = 'type';
  const seg = h('div', { class: 'seg', role: 'group', 'aria-label': 'Gruplama' },
    [['type', 'Uçak tipi'], ['airline', 'Havayolu'], ['stand', 'Stand']].map(([k, l]) =>
      h('button', { 'aria-pressed': k === group ? 'true' : 'false', onclick: (e) => { group = k; seg.querySelectorAll('button').forEach((b) => b.setAttribute('aria-pressed', 'false')); e.target.setAttribute('aria-pressed', 'true'); load(); } }, l)));
  const body = h('div');
  const dq = h('div');
  const refresh = user.role === 'management' && h('button', { class: 'btn ghost', onclick: async (e) => {
    e.target.disabled = true;
    try { const r = await api('/api/v1/reports/refresh', { method: 'POST' }); toast(`Rapor verisi ${num(r.duration_ms)} ms'de yenilendi.`); load(); } catch (err) { toast(err.message, true); } finally { e.target.disabled = false; }
  } }, 'Veriyi yenile');
  main.append(header('Turnaround performansı', 'İlk görev başlangıcından son görev bitişine süre dağılımı', seg, refresh),
    h('section', { class: 'panel' }, body),
    user.role === 'management' ? h('section', { class: 'panel' }, h('h2', {}, 'Veri kalitesi'), dq) : '');

  function load() {
    fill(body, () => api(`/api/v1/reports/turnaround?group_by=${group}`), (d) => {
      if (!d.rows.length) return empty('Rapor için tamamlanmış turnaround yok. Görevler bittikçe ve veri yenilendikçe dolar.');
      const max = Math.max(...d.rows.map((r) => r.p90_min));
      const o = d.overall;
      return h('div', {},
        h('p', { class: 'small muted', style: 'margin-bottom:10px' }, `${num(d.total_turnarounds)} turnaround, sorgu ${Math.round(d.query_ms)} ms. Genel medyan ${o.p50_min} dk, P90 ${o.p90_min} dk, hedef ihlali ${pct(o.violation_rate)}.`),
        h('div', { class: 'table-wrap' }, h('table', {},
          h('thead', {}, h('tr', {}, h('th', {}, group === 'stand' ? 'Stand' : group === 'airline' ? 'Havayolu' : 'Tip'), h('th', { class: 'num' }, 'Adet'),
            h('th', { class: 'num' }, 'Medyan'), h('th', { class: 'num' }, 'P90'), h('th', {}, 'Dağılım (medyan / P90)'), h('th', { class: 'num' }, 'Hedef'), h('th', { class: 'num' }, 'İhlal'))),
          h('tbody', {}, d.rows.map((r) => h('tr', {},
            h('td', {}, group === 'stand' ? plate(r.group) : h('b', {}, r.group)), h('td', { class: 'num' }, num(r.count)),
            h('td', { class: 'num' }, `${r.p50_min} dk`), h('td', { class: 'num' }, `${r.p90_min} dk`),
            h('td', { style: 'min-width:160px' }, h('span', { class: 'bars', style: `width:${(r.p50_min / max) * 100}px` }), ' ',
              h('span', { class: 'bars p90', style: `width:${((r.p90_min - r.p50_min) / max) * 100}px` })),
            h('td', { class: 'num' }, `${r.target_min} dk`),
            h('td', { class: 'num' }, h('span', { class: `tag ${r.violation_rate > 0.3 ? 'stop' : r.violation_rate > 0.15 ? 'late' : 'ok'}` }, pct(r.violation_rate)))))))));
    });
  }
  load();
  if (user.role === 'management') fill(dq, () => api('/api/v1/reports/data-quality'), (d) => d.issues.length === 0 ? empty('Tutarsız zaman damgası bulunmadı.')
    : h('div', {}, h('p', { class: 'small muted', style: 'margin-bottom:8px' }, 'Bu görevlerin turnaroundları rapora dahil edilmiyor.'),
      h('table', {}, h('tbody', {}, d.issues.map((i) => h('tr', {}, h('td', {}, {
        FINISHED_BEFORE_STARTED: 'Bitiş başlangıçtan önce', FINISHED_WITHOUT_STARTED: 'Başlangıç kaydı yok',
        TASK_LONGER_THAN_6H: '6 saatten uzun görev', OCCURRED_IN_FUTURE: 'Sunucuya ulaştığı andan sonra gerçekleşmiş',
      }[i.issue] || i.issue), h('td', { class: 'num' }, num(i.tasks))))))));
}

// ------------------------------------------------------------------ denetim kaydı

function viewAudit(main) {
  const body = h('div');
  const tbody = h('tbody');
  const more = h('button', { class: 'btn ghost', onclick: () => page(true) }, 'Daha eski kayıtlar');
  main.append(header('Denetim kaydı', 'Kim, ne zaman, neyi değiştirdi. Kayıtlar değiştirilemez ve silinemez.'), h('section', { class: 'panel' }, body));
  let before = '';
  async function page(append) {
    if (!append) body.replaceChildren(loading(6));
    try {
      const d = await api(`/api/v1/audit${before ? `?before=${before}` : ''}`);
      before = d.next_before;
      if (!append) {
        if (!d.items.length) { body.replaceChildren(empty('Henüz kayıt yok.')); return; }
        body.replaceChildren(h('div', { class: 'table-wrap' }, h('table', {},
          h('thead', {}, h('tr', {}, ['Zaman', 'Kullanıcı', 'İşlem', 'Kayıt', 'Yeni değer'].map((x) => h('th', {}, x)))), tbody)), more);
      }
      d.items.forEach((a) => tbody.append(h('tr', {}, h('td', {}, fmtDateTime(a.at)), h('td', {}, a.actor_id ? `${a.actor_id} (${ROLE_NAME[a.actor_role] || a.actor_role})` : 'sistem'),
        h('td', {}, a.action), h('td', {}, `${a.entity} ${a.entity_id}`),
        h('td', { class: 'small muted', style: 'max-width:420px;overflow-wrap:anywhere' }, a.new_value ? JSON.stringify(a.new_value).slice(0, 220) : ''))));
      more.hidden = !before;
    } catch (err) { body.replaceChildren(failure(err, () => page(false))); }
  }
  page(false);
}

// ------------------------------------------------------------------ başlat

window.addEventListener('hashchange', route);
route();
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});
