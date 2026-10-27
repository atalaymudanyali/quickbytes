// Load test for retried checkouts, POST /orders. Run with `make load-checkout`.
//
// It plays a customer in a tunnel: the phone sends "Place order", loses the
// answer, and sends it again, many times, always with the same Idempotency-Key.
// Each of the 7 demo customers is one k6 "virtual user" (VU), and they all check
// out at the same time:
//
//   1. fill the cart with one dish;
//   2. send the same checkout 10 times at once (one key), like 10 impatient
//      retries. With the hang card each try gives up after 1 s, as if the
//      answer got lost, and tries again;
//   3. every try that gets an answer must name the same order;
//   4. ask the payment provider itself (the "bank") how many payments it holds
//      for that order. Anything but exactly 1 is a double charge.
//
// The cards take turns: approve, decline, slow, hang.
//
// KEYS=fresh breaks it on purpose: every try gets a new key, like an app that
// forgets to reuse it. The run must then fail.
import http from 'k6/http';
import { check, fail, sleep } from 'k6';
import exec from 'k6/execution';
import { Counter, Trend } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const PROVIDER = __ENV.PROVIDER_URL || 'http://localhost:8090';
const PROVIDER_KEY = __ENV.PROVIDER_KEY || 'sk_test_dev_only_quickbytes_0123456789abcdef';
const PASSWORD = __ENV.SEED_PASSWORD || 'quickbytes-demo';
const FRESH_KEYS = __ENV.KEYS === 'fresh';

// The seeded demo customers (`make seed`). Every one has a customer profile.
const CUSTOMERS = ['musteri', 'kurye', 'demo', 'hasan', 'elif', 'murat', 'zeynep']
  .map((name) => `${name}@demo.quickbytes.test`);
const CARDS = [
  { name: 'approve', number: '4242424242424242' },
  { name: 'decline', number: '4000000000000002' },
  { name: 'slow', number: '4000000000007777' },
  { name: 'hang', number: '4000000000003333' },
];
const RETRIES = 10; // tries sent at once for one checkout
const ROUNDS = 3; // times each customer goes through all four cards

export const options = {
  scenarios: {
    retries: {
      executor: 'per-vu-iterations',
      vus: CUSTOMERS.length,
      iterations: ROUNDS * CARDS.length,
      maxDuration: '5m',
    },
  },
  // Pass/fail rules: one double charge, or two orders from one key, fails the run.
  thresholds: {
    double_charges: ['count==0'],
    extra_orders: ['count==0'],
    checks: ['rate==1'],
    // Naming each card's numbers here makes k6 report them separately.
    ...Object.fromEntries(CARDS.map((c) => [`answer_ms{card:${c.name}}`, ['max>=0']])),
  },
  summaryTrendStats: ['med', 'p(95)', 'max'],
};

const checkouts = new Counter('checkouts');
const tries = new Counter('tries'); // every POST /orders sent, retries included
const replays = new Counter('replays'); // answered from the stored first answer
const busy = new Counter('busy'); // 409 idempotency_in_progress
const lost = new Counter('lost'); // gave up waiting (the answer "got lost")
const providerPayments = new Counter('provider_payments');
const doubleCharges = new Counter('double_charges');
const extraOrders = new Counter('extra_orders');
// From the first tap until the customer has a real answer (not a 409, not a lost
// one), per card: what waiting at "Place order" feels like, retries included.
const answerMs = new Trend('answer_ms', true);

function req(method, path, token, body, params = {}) {
  return http.request(method, `${BASE}${path}`, body ? JSON.stringify(body) : null, {
    ...params,
    headers: {
      'Content-Type': 'application/json',
      Authorization: token ? `Bearer ${token}` : '',
      ...(params.headers || {}),
    },
    tags: { name: params.name || 'setup' },
  });
}

function must(res, want, what) {
  if (res.status !== want) fail(`${what}: ${res.status} ${res.body}`);
  return res.json();
}

// setup runs once, before the customers start: it logs them in, makes sure each
// has an address, and picks a dish they can all order.
export function setup() {
  for (let i = 0; i < 30 && http.get(`${BASE}/readyz`).status !== 200; i++) {
    sleep(1);
  }

  const customers = CUSTOMERS.map((email) => {
    const login = must(req('POST', '/auth/login', '', { email, password: PASSWORD }), 200, `login ${email}`);
    const token = login.tokens.access_token;
    let addrs = must(req('GET', '/me/addresses', token), 200, 'addresses').addresses;
    if (addrs.length === 0) {
      addrs = [must(req('POST', '/me/addresses', token, {
        label: 'Yük testi', address_line: 'Moda Cd. No:1, Kadıköy', lat: 40.987, lng: 29.025,
      }), 201, 'add address')];
    }
    return { email, token, addressID: addrs[0].id };
  });

  // The first open restaurant near Moda with a dish that needs no picks.
  const near = must(req('GET', '/restaurants?lat=40.9810&lng=29.0270'), 200, 'near me').restaurants;
  for (const r of near.filter((x) => x.accepting_orders)) {
    const menu = must(req('GET', `/restaurants/${r.id}/menu`), 200, 'menu');
    for (const c of menu.categories) {
      const dish = c.items.find((i) => i.available && i.option_groups.every((g) => g.min_picks === 0));
      if (dish) {
        // Enough of it to reach the restaurant's minimum basket.
        const quantity = Math.min(20, Math.max(1, Math.ceil(r.min_basket_kurus / dish.price_kurus)));
        return { customers, dish: { id: dish.id, quantity }, restaurant: r.name };
      }
    }
  }
  fail('no open restaurant near Moda with a dish that needs no picks');
}

function uuid() {
  // A random (v4) UUID: Idempotency-Key only needs to be unique.
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    return (c === 'x' ? r : (r & 0x3) | 0x8).toString(16);
  });
}

export default function (data) {
  const me = data.customers[exec.vu.idInTest - 1];
  const card = CARDS[exec.vu.iterationInScenario % CARDS.length];

  // 1. A cart with one dish. Emptying it first clears anything left over.
  req('DELETE', '/cart', me.token);
  must(req('POST', '/cart/items', me.token, {
    menu_item_id: data.dish.id, quantity: data.dish.quantity, choice_ids: [],
  }), 200, 'add to cart');
  const before = must(req('GET', '/orders', me.token), 200, 'orders').orders;

  // 2. The same checkout, RETRIES times at once, one key. A try that gets no
  // answer (lost, or 409 "still working on it") waits a second and tries again.
  const key = uuid();
  const body = { address_id: me.addressID, card_number: card.number };
  const answers = [];
  const started = Date.now();
  let pending = Array.from({ length: RETRIES }, () => true);
  for (let round = 0; round < 40 && pending.some((p) => p); round++) {
    const batch = [];
    pending.forEach((p, i) => {
      if (!p) return;
      batch.push([i, {
        method: 'POST',
        url: `${BASE}/orders`,
        body: JSON.stringify(body),
        params: {
          headers: {
            'Content-Type': 'application/json',
            Authorization: `Bearer ${me.token}`,
            'Idempotency-Key': FRESH_KEYS ? uuid() : key,
          },
          tags: { name: 'checkout', card: card.name },
          // The hang card's first tries give up early: the answer "gets lost".
          timeout: card.name === 'hang' && round < 2 ? '1s' : '30s',
        },
      }]);
    });
    const results = http.batch(batch.map(([, r]) => r));
    results.forEach((res, j) => {
      const i = batch[j][0];
      tries.add(1);
      if (res.status === 0) {
        lost.add(1);
      } else if (res.status === 409 && res.json('error.code') === 'idempotency_in_progress') {
        busy.add(1);
      } else {
        pending[i] = false;
        if (answers.length === 0) answerMs.add(Date.now() - started, { card: card.name });
        answers.push(res);
        if (res.headers['Idempotent-Replayed'] === 'true') replays.add(1);
      }
    });
    if (pending.some((p) => p)) sleep(1);
  }
  checkouts.add(1);

  // 3. Every answer is 201 and names the same order.
  const ids = new Set(answers.map((r) => (r.status === 201 ? r.json('id') : `status ${r.status}`)));
  const orderID = answers.length > 0 && answers[0].status === 201 ? answers[0].json('id') : null;
  check(null, {
    'every try answered': () => answers.length === RETRIES,
    'every answer names one order': () => ids.size === 1 && orderID !== null,
  });
  if (!orderID) return;

  // The customer has exactly one new order.
  const after = must(req('GET', '/orders', me.token), 200, 'orders').orders;
  const fresh = after.filter((o) => !before.some((b) => b.id === o.id));
  if (fresh.length !== 1) extraOrders.add(Math.max(1, fresh.length - 1));
  check(null, { 'one new order': () => fresh.length === 1 && fresh[0].id === orderID });

  // 4. The provider's own books: payments whose reference is this order.
  const found = http.get(`${PROVIDER}/v1/payments?reference=${orderID}`, {
    headers: { Authorization: `Bearer ${PROVIDER_KEY}` }, tags: { name: 'provider' },
  }).json('payments');
  providerPayments.add(found.length);
  if (found.length > 1) doubleCharges.add(found.length - 1);
  check(null, { 'provider holds exactly one payment': () => found.length === 1 });
}

// A short summary instead of k6's long default one: the numbers for the README.
export function handleSummary(data) {
  const m = data.metrics;
  const n = (name) => (m[name] ? m[name].values.count : 0);
  const ms = (v) => `${(v / 1000).toFixed(2)} s`;
  const lines = [
    `customers=${CUSTOMERS.length}  checkouts=${n('checkouts')}  tries sent=${n('tries')}`,
    `  replays (stored answer)     ${n('replays')}`,
    `  409 still in progress       ${n('busy')}`,
    `  answers lost (1 s timeout)  ${n('lost')}`,
    `  provider payments           ${n('provider_payments')}`,
    `  double charges              ${n('double_charges')}`,
    `  extra orders                ${n('extra_orders')}`,
    '  first answer (p50 / p95):',
  ];
  for (const c of CARDS) {
    const t = m[`answer_ms{card:${c.name}}`];
    if (t) lines.push(`    ${c.name.padEnd(8)} ${ms(t.values.med)} / ${ms(t.values['p(95)'])}`);
  }
  const ok = Object.values(m).every((x) => !x.thresholds || Object.values(x.thresholds).every((t) => t.ok));
  lines.push(`  thresholds   ${ok ? 'passed' : 'FAILED'}`, '');
  return { stdout: lines.join('\n') };
}
