// Burst test: 1000 VUs each try to reserve 1 seat for event_id=1 (inv=100).
// Expected outcome with the Redis Lua reserve path: exactly 100 × 200, 900 × 409.
// Zero oversell (impossible — Lua is atomic), zero data loss (every loser gets 409).
//
// Run order:
//
//   ./loadtest/reset-state.sh     # DB → empty, Redis → inv=100, 0 holds
//   ./loadtest/mint-jwts.sh 1000  # pre-mint 1000 unique JWTs to /tmp/k6_jwts.txt
//   ./bin/dashboard-server &      # live UI on http://localhost:8082/
//   k6 run --address :6565 --linger loadtest/burst.js
//   open http://localhost:8082/   # see live metrics
//
// Thresholds are intentionally relaxed: we EXPECT 900/1000 to be 409s, so
// http_req_failed is bounded at 95% (it can never be < 90% in this test).
// The real assertions live in handleSummary() below — read the green ✅
// badges at the end of the run.

import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';
import { textSummary } from 'https://jslib.k6.io/k6-summary/0.0.3/index.js';

const reserved = new Counter('reserved_ok');
const soldOut = new Counter('sold_out');
const other = new Counter('other_status');

const JWT_FILE = __ENV.JWT_FILE || '/tmp/k6_jwts.txt';
const JWTS = open(JWT_FILE).split('\n').filter((l) => l.trim().length > 0);
if (JWTS.length === 0) {
  throw new Error(
    `No JWTs found in ${JWT_FILE} — run ./loadtest/mint-jwts.sh first (it pre-mints one JWT per VU).`,
  );
}

const TOTAL_VUS = parseInt(__ENV.VUS || '1000', 10);
if (JWTS.length < TOTAL_VUS) {
  console.warn(
    `⚠ only ${JWTS.length} JWTs in ${JWT_FILE} but VUs=${TOTAL_VUS} — ` +
      `some VUs will share a JWT and (correctly) collide on the same hold key`,
  );
}

export const options = {
  scenarios: {
    burst: {
      // per-vu-iterations: each VU does EXACTLY 1 iteration, so VU #N uses
      // JWT[N-1] and the iteration count equals TOTAL_VUS exactly.
      executor: 'per-vu-iterations',
      vus: TOTAL_VUS,
      iterations: 1,
      maxDuration: '30s',
    },
  },
  thresholds: {
    // We expect ~90% of requests to be 409s in this test, so a strict
    // http_req_failed threshold would always fire. The real correctness
    // check is in handleSummary() below.
    http_req_failed: ['rate<0.95'],
    'http_req_duration{expected_response:true}': ['p(99)<500'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(50)', 'p(90)', 'p(95)', 'p(99)'],
  discardResponseBodies: true,
};

export default function () {
  const jwt = JWTS[(__VU - 1) % JWTS.length];
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
  const expectedR = Math.min(TOTAL_VUS, 100); // we set inv=100

  console.log('');
  console.log('─────────────── LOAD TEST RESULTS ───────────────');
  console.log(`  Total requests    : ${t}`);
  console.log(
    `  Reserved (200)    : ${String(r).padStart(4)}  ${
      r === expectedR ? '✅' : '❌ expected ' + expectedR
    }`,
  );
  console.log(
    `  Sold out (409)    : ${String(s).padStart(4)}  ${
      r + s === t ? '✅' : '❌ unexpected loss (' + (t - r - s) + ' reqs went missing)'
    }`,
  );
  console.log(
    `  Other status      : ${String(o).padStart(4)}  ${
      o === 0 ? '✅' : '❌ expected 0 (no 401/500/etc.)'
    }`,
  );
  console.log(
    `  Oversell check    : ${r <= 100 ? '✅ no oversell' : '❌ OVERSELL — ' + r + ' > 100'}`,
  );
  console.log(`──────────────────────────────────────────────────`);
  console.log('');

  return {
    stdout: textSummary(data, { indent: '  ', enableColors: true }),
  };
}
