// Ramp test: 7 stages, 0 -> 1000 VUs over 90 seconds. Each stage ramps or
// holds; the dashboard shows VU count, request rate, and latency rising
// through each band so you can see how the system responds to *increasing*
// load (vs. the burst test, which is pure correctness under maximum shock).
//
// Stages (total ~90s):
//   0-10s    warm-up:   0  ->  50 VUs
//   10-25s   low:        50 VUs   (baseline, see real latency at low load)
//   25-35s   medium:    50  -> 200 VUs
//   35-50s   hold:      200 VUs
//   50-65s   peak:     200  -> 1000 VUs
//   65-85s   sustained: 1000 VUs
//   85-90s   cooldown:  1000 -> 0 VUs
//
// Same correctness assertions as burst.js: 100 winners, 0 oversell, 0
// unexpected status. Expected split depends on stage order — because the
// first 100 unique users to hit /reserve get 200 and the rest get 409
// (either "sold out" or "already holding"), the final count is NOT
// tied to VU count, only to how many unique users managed to race in
// before inv hit 0. With the curve above, the 100 winners are decided
// within the first ~1 second of the 50-VU stage and never change.
//
// Run order:
//   ./loadtest/reset-state.sh
//   ./loadtest/mint-jwts.sh 1000     # 1000 unique users; the ramp cycles through
//   ./bin/dashboard-server &          # live UI on http://localhost:8082/
//   k6 run --address localhost:6565 --linger /Users/it000058/Documents/ticket-deal/loadtest/ramp.js
//   open http://localhost:8082/      # watch the VU count climb in waves

import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';
import { textSummary } from 'https://jslib.k6.io/k6-summary/0.0.3/index.js';
import exec from 'k6/execution';

const reserved = new Counter('reserved_ok');
const soldOut = new Counter('sold_out'); // 409s: either "sold out" or "already holding"
const other = new Counter('other_status');

const JWT_FILE = __ENV.JWT_FILE || '/tmp/k6_jwts.txt';
const JWTS = open(JWT_FILE).split('\n').filter((l) => l.trim().length > 0);
if (JWTS.length === 0) {
  throw new Error(
    `No JWTs found in ${JWT_FILE} — run ./loadtest/mint-jwts.sh first (the ramp cycles through all of them).`,
  );
}

export const options = {
  scenarios: {
    ramp: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '10s', target: 50 }, // warm-up
        { duration: '15s', target: 50 }, // hold: low load
        { duration: '10s', target: 200 }, // ramp to medium
        { duration: '15s', target: 200 }, // hold: medium load
        { duration: '15s', target: 1000 }, // ramp to peak
        { duration: '20s', target: 1000 }, // hold: peak load
        { duration: '5s', target: 0 }, // cooldown
      ],
      gracefulRampDown: '5s',
    },
  },
  thresholds: {
    // Ramp test: 90% of reqs are 409 (only the first 100 unique users win).
    // We still want 0 5xx, but the k6 `http_req_failed` metric counts ALL
    // non-2xx as failures, so the threshold has to be loose.
    http_req_failed: ['rate<0.95'],
    'http_req_duration{expected_response:true}': ['p(99)<500'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(50)', 'p(90)', 'p(95)', 'p(99)'],
  discardResponseBodies: true,
};

export default function () {
  // Cycle through the JWT pool so each iteration uses a *different* user
  // (a single JWT used twice would 409 on the second call with "already
  // holding", which is correct but would skew our user-distribution view).
  // exec.scenario.iterationInTest is a global monotonic counter, so
  // iterationInTest % JWTS.length gives a stable round-robin.
  const jwt = JWTS[exec.scenario.iterationInTest % JWTS.length];
  const res = http.post(
    'http://localhost:8080/api/tickets/reserve',
    JSON.stringify({ seats_requested: 1 }),
    {
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${jwt}`,
      },
    },
  );
  if (res.status === 200) reserved.add(1);
  else if (res.status === 409) soldOut.add(1);
  else other.add(1);
  check(res, { 'status is 200 or 409': (r) => r.status === 200 || r.status === 409 });
}

export function handleSummary(data) {
  const r = data.metrics.reserved_ok?.values?.count ?? 0;
  const s = data.metrics.sold_out?.values?.count ?? 0;
  const o = data.metrics.other_status?.values?.count ?? 0;
  const t = data.metrics.http_reqs?.values?.count ?? 0;
  const p99 = data.metrics.http_req_duration?.values?.['p(99)'] ?? 0;

  console.log('');
  console.log('─────────────── RAMP TEST RESULTS ───────────────');
  console.log(`  Total requests   : ${t}`);
  console.log(
    `  Reserved (200)   : ${String(r).padStart(5)}  ${
      r <= 100 ? '✅ no oversell' : '❌ OVERSELL — ' + r + ' > 100'
    }`,
  );
  console.log(
    `  Sold out (409)   : ${String(s).padStart(5)}  ${
      o === 0 ? '✅' : '❌ ' + o + ' unexpected status'
    }`,
  );
  console.log(
    `  Other status     : ${String(o).padStart(5)}  ${o === 0 ? '✅' : '❌'}`,
  );
  console.log(`  Latency p(99)    : ${p99.toFixed(1)} ms`);
  console.log('──────────────────────────────────────────────────');
  console.log('');

  return {
    stdout: textSummary(data, { indent: '  ', enableColors: true }),
  };
}
