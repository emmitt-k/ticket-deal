# Dashboard Redesign — Control Room Aesthetic

> **Goal:** Transform the current 9-panel flat grid into a "control room" layout that feels alive at-a-glance, with strong visual hierarchy, semantic colors, and a few hero KPIs. Designed to look impressive in README screenshots.

---

## Current problems

- Flat 9-panel grid with no visual hierarchy
- All panels same size (12x8) — no emphasis
- Time-series charts with 1-2px lines, default colors
- Stats with no context (just a number)
- No row headers / sections
- Mixed-color lines look noisy

## Research findings (Grafana best practices)

From Grafana's own docs, the SRE community, and Reddit/GrafanaCON examples:

1. **Headline KPIs up top** — 4-6 large stat panels for "what matters most"
2. **Logical sections with row headers** — "REQUEST FLOW", "RESERVATION ENGINE", "INVENTORY"
3. **Semantic colors only** — green=healthy, yellow=warning, red=critical; never random
4. **Variable sizes** — mix 4x4 KPIs, 12x6 charts, 24x8 hero panels
5. **Sparkline trend indicators** on stat panels
6. **No legends on single-series** — saves space, reduces noise
7. **Threshold-based line coloring** — value changes the line color
8. **Consistent dark theme** — professional, easy on the eyes

---

## Proposed layout (24-column grid)

```
y=0  ┌────────────────────────────────────────────────────────────────────────────┐
     │  ROW 1: "Live Status" — full-width header bar (status badge + time)        │  h=2
y=2  ├────────────┬────────────┬────────────┬────────────┬────────────┬────────────┤
     │ HOLD RATE  │  SOLD OUT  │  ERROR %   │  p99 LAT   │ SEATS LEFT │  IN QUEUE  │  h=4 each
     │  (4 cols)  │  (4 cols)  │  (4 cols)  │  (4 cols)  │  (4 cols)  │  (4 cols)  │  K P I s
y=6  ├────────────┴────────────┴────────────┴────────────┴────────────┴────────────┤
     │  ROW 2: "Request Flow" header                                                    │  h=2
y=8  │  ┌──────────────────────────────────┐  ┌─────────────────────────────────────┐ │
     │  │ Request Rate (by path)         │  │ p99 Latency (by path)              │ │
     │  │   w=12, h=8 (with area fill)    │  │   w=12, h=8 (color by threshold)   │ │
y=16 ├──────────────────────────────────┴────────────────────────────────────────────┘
     │  ROW 3: "Reservation Engine" header                                                │  h=2
y=18 │  ┌──────────────────────────────────┐  ┌─────────────────────────────────────┐ │
     │  │ Reservation Outcomes            │  │ Worker Throughput + DB latency     │ │
     │  │   w=12, h=8 (stacked, color-    │  │   w=12, h=8 (two y-axes or split)  │ │
     │  │   coded: green/yellow/red)      │  │                                     │ │
y=26 ├──────────────────────────────────┴────────────────────────────────────────────┘
     │  ROW 4: "Background" header                                                        │  h=2
y=28 │  ┌──────────────────────────────────┐  ┌─────────────────────────────────────┐ │
     │  │ Expiration Activity             │  │ (could add more later)              │ │
     │  │   w=12, h=6 (worker vs watcher) │  │                                     │ │
y=34 └──────────────────────────────────┴────────────────────────────────────────────┘
```

---

## Panel details

### Row 1: KPI stats (6 panels, 4x4 each)

| KPI | Metric | Type | Visual |
|---|---|---|---|
| **HOLD RATE** | `rate(reservations_held_total{service="api"}[1m])` | stat | Big number + sparkline + green/yellow/red threshold |
| **SOLD OUT RATE** | `rate(reservations_sold_out_total{service="api"}[1m])` | stat | Big number + sparkline + warning-yellow color |
| **ERROR RATE** | `sum(rate(http_requests_total{status=~"5..*"}[1m]))` | stat | Big number + sparkline + red threshold (alert if >0) |
| **p99 LATENCY** | `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket{service="api"}[1m]))` | stat | Big number + sparkline + green/yellow/red |
| **SEATS LEFT** | `redis_seats_available{service="api"}` | stat | Big number + sparkline + red when 0 |
| **IN QUEUE** | `waiting_room_queue_length{service="api"}` | stat | Big number + sparkline + red when >80 |

### Row 2: Request Flow (2 panels, 12x8)

| Panel | Query | Visual |
|---|---|---|
| **Request Rate** | `sum by (path) (rate(http_requests_total{service="api"}[10s]))` | Stacked area, soft green/blue, legend on right |
| **p99 Latency** | `histogram_quantile(0.99, sum by (path, le) (rate(http_request_duration_seconds_bucket{service="api"}[10s])))` | Line with threshold colors |

### Row 3: Reservation Engine (2 panels, 12x8)

| Panel | Query | Visual |
|---|---|---|
| **Reservation Outcomes** | 4 metrics, stacked area | held=green, completed=blue, sold_out=yellow, OVERSOLD=red |
| **Worker Throughput + DB Latency** | Throughput (line) + p50/p99 latency (overlay) | Two y-axes |

### Row 4: Background (1-2 panels, 12x6)

| Panel | Query | Visual |
|---|---|---|
| **Expiration Activity** | 2 metrics, by service | Worker vs watcher lines, area fill |

### Bonus: "Live Status" row at the very top

- Markdown/text panel showing:
  - 🔴 LIVE • Last refresh: now • Event: 1 • VUs: -- (from a variable if we add one)
  - Just a status banner that makes it feel "alive"

---

## Visual design rules

| Element | Setting |
|---|---|
| Background | Grafana dark theme (default) |
| Stat text size | Large (display 36-48px) |
| Stat color | Threshold-based (green→yellow→red) |
| Stat sparkline | Show last 30m trend |
| Chart line width | 2-3px |
| Chart fill | 10-30% opacity |
| Chart grid | Subtle (1px, 10% opacity) |
| Chart tooltip | Shared crosshair |
| Legend | Hidden when 1 series; right-side table when multi |
| Font | Default monospace for numbers (consistency) |

---

## Files to change

- `monitoring/grafana/dashboards/ticket-deal-overview.json` — full rewrite

## What I'm NOT changing

- Metrics (already correct after recent fixes)
- Prometheus scrape config
- Data source provisioning
- Folder structure

## Estimated work

- 1 large JSON rewrite (~600 lines vs current ~290)
- Requires a careful hand-edit of gridPos values so the layout flows
- Should test in browser (refresh dashboard, click around, run a load test)
- ~30 min implementation
