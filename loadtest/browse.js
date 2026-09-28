// Anonymous browsing: search, then open a listing. p95 < 300 ms.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { BASE, fixtures, headers, pick } from './lib.js';

export const options = {
  scenarios: {
    browse: { executor: 'constant-vus', vus: 20, duration: '30s' },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<300'],
  },
};

export default function () {
  const h = { headers: headers(__VU) };

  const search = http.get(`${BASE}/v1/listings?q=maize&limit=20`, h);
  check(search, {
    'search 200': (r) => r.status === 200,
    'search has items': (r) => Array.isArray(r.json('items')),
  });

  const listing = pick(fixtures.listings, __VU + __ITER);
  const detail = http.get(`${BASE}/v1/listings/${listing.slug}`, h);
  check(detail, {
    'detail 200': (r) => r.status === 200,
    'detail is the listing': (r) => r.json('slug') === listing.slug,
  });

  sleep(1);
}
