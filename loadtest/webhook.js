// Signed webhook replays. One charge.success is delivered over and over, the
// way Paystack retries: the first delivery settles the payment, every replay
// must still answer 200 and change nothing. A forged signature must be 401.
import http from 'k6/http';
import crypto from 'k6/crypto';
import { check } from 'k6';
import { BASE, buyerFor, cartBody, fixtures, headers, post, uuid } from './lib.js';

const SECRET = __ENV.PAYSTACK_SECRET;

export const options = {
  scenarios: {
    replay: { executor: 'constant-arrival-rate', rate: 30, timeUnit: '1s', duration: '20s', preAllocatedVUs: 10 },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<300'],
    checks: ['rate>0.99'],
  },
};

// setup makes one real checkout, so there is a payment to settle.
export function setup() {
  const buyer = buyerFor(1);
  const res = post('/v1/checkout', cartBody(fixtures.listings[0], 1), {
    headers: { ...headers(1, buyer.token), 'Idempotency-Key': uuid() },
  });
  if (res.status !== 201) throw new Error(`setup checkout failed: ${res.status} ${res.body}`);
  const body = JSON.stringify({
    event: 'charge.success',
    data: {
      id: 900000001, reference: res.json('reference'), amount: res.json('quote.charge.amount'),
      currency: 'GHS', fees: 199, channel: 'card', status: 'success', paid_at: new Date().toISOString(),
    },
  });
  return { body, signature: crypto.hmac('sha512', SECRET, body, 'hex') };
}

export default function (data) {
  const h = headers(__VU);

  const replay = http.post(`${BASE}/v1/webhooks/paystack`, data.body, {
    headers: { ...h, 'x-paystack-signature': data.signature },
  });
  check(replay, { 'replay 200': (r) => r.status === 200 });

  // Every tenth request is a forgery: wrong signature, same body.
  if (__ITER % 10 === 0) {
    // A 401 is the right answer here, so k6 must not count it as a failure.
    const forged = http.post(`${BASE}/v1/webhooks/paystack`, data.body, {
      headers: { ...h, 'x-paystack-signature': '0'.repeat(128) },
      responseCallback: http.expectedStatuses(401),
    });
    check(forged, { 'forged signature 401': (r) => r.status === 401 });
  }
}
