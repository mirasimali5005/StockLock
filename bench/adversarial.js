// Adversarial workload: 1,000 shoppers fight over a scarce SKU for a minute while
// the sweeper runs. Every request is recorded as one JSON line (via console.log) so
// cmd/verify can check the shoppers' answers against the database afterwards.
//
//   k6 run --log-format=raw --console-output=events.jsonl \
//      -e VUS=1000 -e DURATION=60s -e BASE=http://localhost:8081 bench/adversarial.js
//
// A shopper who gets a unit then behaves in one of these ways:
//   confirm        confirm within the hold window
//   abandon        abandon within the hold window
//   vanish         never come back (the sweeper must return the unit)
//   late           wait until after the deadline, then try to confirm (must fail)
//   double-confirm send the same confirm operation twice at once (one unit, same answer)
// and 15% of shoppers, decided before reserving, send their reserve 10 times at once
// with one operation id (retry storm), whatever the answer turns out to be.
import http from 'k6/http';
import { sleep } from 'k6';
import { uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const BASE = __ENV.BASE || 'http://localhost:8081';
const SKU = __ENV.SKU || 'ADV-SKU';
const VUS = Number(__ENV.VUS || 1000);
const HOLD_MS = Number(__ENV.HOLD_MS || 3000);
const HEADERS = { headers: { 'Content-Type': 'application/json' } };

export const options = {
  scenarios: {
    shoppers: {
      executor: 'ramping-vus',
      startVUs: 0,
      // Ramp over 5 s rather than all at once: 1,000 simultaneous connects overflow
      // macOS's default listen backlog and get reset before reaching the API.
      stages: [
        { duration: '5s', target: VUS },
        { duration: __ENV.DURATION || '55s', target: VUS },
      ],
      gracefulStop: '10s',
    },
  },
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

// record writes one event line. body is parsed JSON or null if unparseable.
function record(kind, opID, res, extra) {
  let body = null;
  try { body = JSON.parse(res.body); } catch (e) { /* status 0 or non-JSON */ }
  console.log(JSON.stringify(Object.assign({
    kind, op: opID, status: res.status, sent_at: extra.sent_at,
    reservation_id: body && body.reservation_id ? body.reservation_id : null,
    body: res.body || null,
  }, extra)));
}

function post(kind, opID, payload) {
  const sent_at = Date.now();
  const res = http.post(`${BASE}/${kind}`, JSON.stringify(payload), HEADERS);
  record(kind, opID, res, { sent_at });
  return res;
}

// Same request n times in parallel, one operation id.
function postStorm(kind, opID, payload, n, note) {
  const sent_at = Date.now();
  const reqs = [];
  for (let i = 0; i < n; i++) reqs.push(['POST', `${BASE}/${kind}`, JSON.stringify(payload), HEADERS]);
  const results = http.batch(reqs);
  for (const res of results) record(kind, opID, res, { sent_at, storm: n, note });
  return results;
}

function pick(list) { return list[Math.floor(Math.random() * list.length)]; }

export default function () {
  const reserveOp = uuidv4();
  const payload = { operation_id: reserveOp, sku: SKU };

  let first;
  if (Math.random() < 0.15) {
    first = postStorm('reserve', reserveOp, payload, 10, 'retry-storm')[0];
  } else {
    first = post('reserve', reserveOp, payload);
  }

  if (first.status === 201) {
    const rid = JSON.parse(first.body).reservation_id;
    const behaviour = pick(['confirm', 'abandon', 'vanish', 'late', 'double-confirm']);
    const finishOp = uuidv4();
    const finishPayload = { operation_id: finishOp, reservation_id: rid };
    switch (behaviour) {
      case 'confirm':
        sleep(0.2 + Math.random() * 0.8);
        post('confirm', finishOp, finishPayload);
        break;
      case 'abandon':
        sleep(0.2 + Math.random() * 0.8);
        post('abandon', finishOp, finishPayload);
        break;
      case 'vanish':
        break;
      case 'late':
        sleep((HOLD_MS + 1000) / 1000);
        post('confirm', finishOp, finishPayload);
        break;
      case 'double-confirm':
        sleep(0.2 + Math.random() * 0.8);
        postStorm('confirm', finishOp, finishPayload, 2, 'double-click');
        break;
    }
  }
  sleep(0.5 + Math.random());
}
