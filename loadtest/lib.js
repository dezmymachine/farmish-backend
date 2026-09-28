// Shared helpers for the k6 scripts. `make loadtest` mounts the seeded
// fixtures at /data/fixtures.json and passes BASE_URL and PAYSTACK_SECRET.
import http from 'k6/http';
import crypto from 'k6/crypto';

export const BASE = __ENV.BASE_URL || 'http://127.0.0.1:18100';

// One entry per seeded buyer; a virtual user always uses the same one, so
// per-user rate limits apply to it and not to the whole run.
export const fixtures = JSON.parse(open('/data/fixtures.json'));

// Each virtual user gets its own client address. The API trusts
// X-Forwarded-For only from the loopback proxy the run configures, so the
// per-IP flood limit sees many clients, as it would in production, instead of
// one machine hammering the API.
export function headers(vu, token) {
  const h = {
    'Content-Type': 'application/json',
    'X-Forwarded-For': `10.42.${(vu >> 8) & 255}.${vu & 255}`,
  };
  if (token) h.Authorization = `Bearer ${token}`;
  return h;
}

export function buyerFor(vu) {
  return fixtures.buyers[(vu - 1) % fixtures.buyers.length];
}

export function pick(list, n) {
  return list[n % list.length];
}

export function post(path, body, params) {
  return http.post(`${BASE}${path}`, typeof body === 'string' ? body : JSON.stringify(body), params);
}

export function cartBody(listing, quantity) {
  return {
    lines: [{ listingId: listing.id, quantity }],
    delivery: [{ sellerId: fixtures.sellerId, method: 'pickup' }],
  };
}

// uuid returns a random v4 UUID, for Idempotency-Key headers. Built from k6's
// own randomness, so the scripts need no network to fetch a helper library.
export function uuid() {
  const b = new Uint8Array(crypto.randomBytes(16));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}
