// k6 sürümü (make load-k6). Go sürümüyle aynı karışım: cmd/loadtest.
// Çalıştırma: k6 run -e API_URL=http://localhost:8080 loadtest/k6.js
import http from 'k6/http';
import { check } from 'k6';

const API = __ENV.API_URL || 'http://localhost:8080';

export const options = {
  scenarios: { mixed: { executor: 'constant-vus', vus: 50, duration: '60s' } },
  thresholds: { http_req_failed: ['rate<0.01'], http_req_duration: ['p(95)<800'] },
  summaryTrendStats: ['med', 'p(95)', 'p(99)', 'max'],
};

function login(email) {
  const r = http.post(`${API}/api/v1/auth/login`, JSON.stringify({ email, password: 'Demo.2026!' }), { headers: { 'Content-Type': 'application/json' } });
  return { headers: { Authorization: `Bearer ${r.json('access_token')}`, 'Content-Type': 'application/json' } };
}

export function setup() {
  return { ops: login('ops@pist.test'), mgmt: login('yonetim@pist.test'), chief: login('sef@pist.test') };
}

export default function (t) {
  const r = Math.random() * 100;
  let res;
  if (r < 30) res = http.get(`${API}/api/v1/flights?limit=50`, { ...t.ops, tags: { name: 'flights' } });
  else if (r < 50) res = http.get(`${API}/api/v1/dashboard`, { ...t.mgmt, tags: { name: 'dashboard' } });
  else if (r < 65) res = http.get(`${API}/api/v1/allocations`, { ...t.ops, tags: { name: 'allocations' } });
  else if (r < 75) res = http.get(`${API}/api/v1/tasks?limit=100`, { ...t.chief, tags: { name: 'tasks' } });
  else if (r < 80) res = http.get(`${API}/api/v1/reports/turnaround?group_by=airline`, { ...t.mgmt, tags: { name: 'report' } });
  else if (r < 88) {
    const body = JSON.stringify({ flight_id: 2 + Math.floor(Math.random() * 250), new_time: new Date(Date.now() + Math.random() * 4 * 3600e3).toISOString() });
    res = http.post(`${API}/api/v1/replan/preview`, body, { ...t.ops, tags: { name: 'preview' }, responseCallback: http.expectedStatuses(200, 422) });
  } else res = http.post(`${API}/api/v1/notices`, JSON.stringify({ message: 'k6', level: 'info' }), { ...t.ops, tags: { name: 'notice' } });
  check(res, { 'durum < 500': (x) => x.status < 500 });
}
