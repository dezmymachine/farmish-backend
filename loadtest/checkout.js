// Signed-in buyers quote a cart and check out against the Paystack stub.
// p95 < 800 ms. The checkout is a `sensitive` operation (10/min per user), so
// each virtual user has its own buyer and paces itself under that budget.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { BASE, buyerFor, cartBody, fixtures, headers, pick, post, uuid } from './lib.js';

export const options = {
  scenarios: {
    checkout: { executor: 'constant-vus', vus: 10, duration: '60s' },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<800'],
    checks: ['rate>0.99'],
  },
};

export default function () {
  const buyer = buyerFor(__VU);
  const params = { headers: headers(__VU, buyer.token) };
  const listing = pick(fixtures.listings, __VU + __ITER);
  const cart = cartBody(listing, 1 + (__ITER % 3));

  const quote = post('/v1/checkout/quote', cart, params);
  check(quote, {
    'quote 200': (r) => r.status === 200,
    'quote carries a charge': (r) => r.json('charge.amount') > 0,
  });

  const created = post('/v1/checkout', cart, {
    headers: { ...params.headers, 'Idempotency-Key': uuid() },
  });
  check(created, {
    'checkout 201': (r) => r.status === 201,
    'checkout has a reference': (r) => !!r.json('reference'),
    'checkout redirects to the provider': (r) => `${r.json('authorizationUrl')}`.startsWith('https://checkout.paystack.test/'),
  });

  // Poll it back like the payment status page does.
  if (created.status === 201) {
    const status = http.get(`${BASE}/v1/checkouts/${created.json('checkoutId')}`, params);
    check(status, { 'poll 200': (r) => r.status === 200 });
  }

  // 10 checkouts a minute per user is the API's budget; stay under it.
  sleep(7);
}
