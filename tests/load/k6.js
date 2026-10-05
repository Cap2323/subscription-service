import http from 'k6/http';
import { check, sleep } from 'k6';
import { SharedArray } from 'k6/data';

// One token per distinct user. A one-token test measures a hot tenant, not sharding.
const tokens = new SharedArray('tokens', () => JSON.parse(open(__ENV.TOKENS_FILE || '../../.local/tokens.json')));
export const options = {
  stages: [{ duration: '30s', target: 20 }, { duration: '2m', target: 100 }, { duration: '30s', target: 0 }],
  thresholds: { http_req_failed: ['rate<0.01'], http_req_duration: ['p(95)<200', 'p(99)<500'] },
};
export default function () {
  const token = tokens[(__VU - 1 + __ITER * 131) % tokens.length];
  const root = __ENV.BASE_URL || 'http://127.0.0.1:8080';
  const headers = { Authorization: `Bearer ${token}` };
  const response = http.get(`${root}/api/v1/subscriptions?limit=20`, { headers, tags: { name: 'list' } });
  check(response, { 'list is 200': (r) => r.status === 200 });
  if (__ITER % 10 === 0) {
    const total = http.get(`${root}/api/v1/subscriptions/total?from=01-2025&to=12-2025`, { headers, tags: { name: 'total' } });
    check(total, { 'total is 200': (r) => r.status === 200 });
  }
  sleep(0.1);
}
