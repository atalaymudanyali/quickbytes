// Load test for the public menu, GET /restaurants/{id}/menu. Run with `make load-menu`.
//
// k6 starts 50 "virtual users" (VUs): each one is a pretend app that asks for the
// menu again and again for 30 seconds. MODE picks what is measured:
//
//   nocache  the API runs without Redis: every request reads PostgreSQL
//   cache    the API runs with Redis: almost every request is a cache HIT
//   etag     like cache, but the app sends If-None-Match and gets empty 304s
import http from 'k6/http';
import { check, fail, sleep } from 'k6';
import { Rate } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const MODE = __ENV.MODE || 'cache';

export const options = {
  vus: 50,
  duration: '30s',
  // Pass/fail rules: a broken run should fail loudly, not print nice numbers.
  thresholds: {
    http_req_failed: ['rate<0.01'], // under 1% errors
    checks: ['rate>0.99'],
    // Thresholds on the menu requests alone also make k6 report them separately
    // from the setup requests.
    'http_req_duration{name:menu}': ['p(95)<1000'],
    'http_reqs{name:menu}': ['count>0'],
  },
  summaryTrendStats: ['med', 'p(95)', 'p(99)', 'max'],
};

// Share of responses the cache answered (X-Cache: HIT).
const cacheHit = new Rate('cache_hit');

const SECTIONS = 8;
const DISHES = 10; // per section: 80 dishes, a realistic menu

function post(path, token, body) {
  const res = http.post(`${BASE}${path}`, JSON.stringify(body), {
    headers: { 'Content-Type': 'application/json', Authorization: token ? `Bearer ${token}` : '' },
    tags: { name: 'setup' },
  });
  if (res.status !== 201 && res.status !== 200) {
    fail(`POST ${path}: ${res.status} ${res.body}`);
  }
  return res.json();
}

// setup runs once, before the users start: it builds a menu through the real API,
// the same way an owner would.
export function setup() {
  // The API container may still be starting.
  for (let i = 0; i < 30 && http.get(`${BASE}/readyz`).status !== 200; i++) {
    sleep(1);
  }

  const email = `load-${Date.now()}-${Math.floor(Math.random() * 1e6)}@example.com`;
  const signup = post('/auth/signup', '', { email, password: 'load-test-password', full_name: 'Load Test' });
  post('/me/profiles', signup.tokens.access_token, { role: 'restaurant' });
  // A fresh access token lists the new restaurant role.
  const token = post('/auth/refresh', '', { refresh_token: signup.tokens.refresh_token }).access_token;

  const r = post('/restaurants', token, {
    name: 'Yük Testi Lokantası', address: 'Moda Cd. No:1, Kadıköy', lat: 40.987, lng: 29.025,
  });
  for (let s = 1; s <= SECTIONS; s++) {
    const c = post(`/restaurants/${r.id}/menu/categories`, token, { name: `Bölüm ${s}` });
    for (let d = 1; d <= DISHES; d++) {
      post(`/restaurants/${r.id}/menu/items`, token, {
        category_id: c.id,
        name: `Yemek ${s}.${d}`,
        description: 'Günün taze malzemeleriyle, ustanın tarifiyle hazırlanır.',
        price_kurus: 15000 + d * 1000,
      });
    }
  }

  const first = http.get(`${BASE}/restaurants/${r.id}/menu`);
  return { url: `${BASE}/restaurants/${r.id}/menu`, etag: first.headers['Etag'] };
}

// Each virtual user runs this in a loop for the whole test.
export default function (data) {
  const headers = MODE === 'etag' ? { 'If-None-Match': data.etag } : {};
  const res = http.get(data.url, { headers, tags: { name: 'menu' } });
  const want = MODE === 'etag' ? 304 : 200;
  check(res, { [`status ${want}`]: (r) => r.status === want });
  cacheHit.add(res.headers['X-Cache'] === 'HIT');
}

// A short summary instead of k6's long default one: the numbers for the README.
export function handleSummary(data) {
  const m = data.metrics;
  const ms = (v) => `${v.toFixed(2)} ms`;
  const d = m['http_req_duration{name:menu}'];
  const lines = [
    `mode=${MODE}  users=${options.vus}  duration=${options.duration}`,
    `  requests/s   ${m['http_reqs{name:menu}'].values.rate.toFixed(0)}`,
    `  p50 / p95    ${ms(d.values.med)} / ${ms(d.values['p(95)'])}`,
    `  cache hits   ${(m.cache_hit.values.rate * 100).toFixed(1)}%`,
    `  errors       ${(m.http_req_failed.values.rate * 100).toFixed(2)}%`,
    `  thresholds   ${Object.values(m).every((x) => !x.thresholds || Object.values(x.thresholds).every((t) => t.ok)) ? 'passed' : 'FAILED'}`,
    '',
  ];
  return { stdout: lines.join('\n') };
}
