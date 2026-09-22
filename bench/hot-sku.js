// Hot-SKU workload: every virtual user reserves the same SKU in a tight loop.
// The SKU is seeded deep enough never to run out, so every request is a real
// mutation that has to take the same stock row lock.
//
//   k6 run -e VUS=100 -e DURATION=30s -e BASE=http://localhost:8081 bench/hot-sku.js
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';
import { uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const BASE = __ENV.BASE || 'http://localhost:8081';
const SKU = __ENV.SKU || 'HOT-SKU';

export const options = {
  vus: Number(__ENV.VUS || 100),
  duration: __ENV.DURATION || '30s',
  // No thresholds: this is a measurement, not a pass/fail test.
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

export const reserved = new Counter('reserved_201');
export const refused = new Counter('refused_409');
export const failed = new Counter('failed_other');
let failed_seen = 0;

export default function () {
  const res = http.post(`${BASE}/reserve`,
    JSON.stringify({ operation_id: uuidv4(), sku: SKU }),
    { headers: { 'Content-Type': 'application/json' } });
  if (res.status === 201) reserved.add(1);
  else if (res.status === 409) refused.add(1);
  else {
    failed.add(1);
    // Anything other than 201/409 is unexpected; show the first few so they can be explained.
    if (failed_seen++ < 10) console.error(`unexpected status ${res.status}: ${res.error || res.body}`);
  }
  check(res, { 'reserved': (r) => r.status === 201 });
}

export function handleSummary(data) {
  const out = __ENV.SUMMARY ? { [__ENV.SUMMARY]: JSON.stringify(data, null, 1) } : {};
  out.stdout = textSummary(data);
  return out;
}

function textSummary(data) {
  const m = data.metrics;
  const d = m.http_req_duration.values;
  const n = (v) => (v === undefined ? 0 : v);
  return [
    '',
    `requests:      ${m.http_reqs.values.count}  (${m.http_reqs.values.rate.toFixed(1)} req/s)`,
    `reserved 201:  ${n(m.reserved_201 && m.reserved_201.values.count)}`,
    `refused  409:  ${n(m.refused_409 && m.refused_409.values.count)}`,
    `failed other:  ${n(m.failed_other && m.failed_other.values.count)}`,
    `latency ms:    p50=${d['p(50)'].toFixed(2)}  p95=${d['p(95)'].toFixed(2)}  p99=${d['p(99)'].toFixed(2)}  max=${d.max.toFixed(1)}`,
    '',
  ].join('\n');
}
