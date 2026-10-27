//go:build e2e

// Package e2e holds the END-TO-END test: the one that drives the whole
// running stack through its real HTTP API, its real queue, its real
// workers and its real database.
//
// It is behind the `e2e` build tag, so `go test ./...` does not run it
// and the normal CI coverage number is unaffected.
//
// Run it against a live stack:
//
//	docker compose --env-file .env.docker up -d --build
//	go test -tags=e2e ./internal/e2e/...
//
// What it proves, and why it cannot be proved any other way: the four
// pieces are wired to each other.
//
//	reserve ──► SQS ──► worker ──► Postgres
//	                    │
//	signed webhook ──► SQS ──► stripe-worker ──► Postgres
//
// A unit test can only show that each piece works ALONE. This one
// shows that a seat reserved through the public HTTP API becomes a
// CONFIRMED reservation with an audit trail, because a signed Stripe
// event travelled through a queue and was applied by a different
// process.
//
// Every prerequisite comes from the environment. If one is missing the
// test SKIPS rather than fails, so `go test -tags=e2e ./...` is safe
// to run anywhere.
package e2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// ── Configuration ──────────────────────────────────────────────────────

type env struct {
	apiBase        string
	dsn            string
	redisAddr      string
	jwtSecret      []byte
	webhookSecret  string
	eventID        int64
	stripePaymentI string
}

func loadEnv(t *testing.T) env {
	t.Helper()

	e := env{
		apiBase:        envOr("API_BASE_URL", "http://localhost:8080"),
		dsn:            envOr("DATABASE_URL", "postgres://tickets:tickets@localhost:5432/tickets?sslmode=disable"),
		redisAddr:      envOr("REDIS_ADDR", "localhost:6379"),
		jwtSecret:      []byte(os.Getenv("JWT_SECRET")),
		webhookSecret:  os.Getenv("STRIPE_WEBHOOK_SECRET"),
		eventID:        1,
		stripePaymentI: "pi_e2e_" + strconv.FormatInt(time.Now().UnixNano(), 36),
	}

	if len(e.jwtSecret) < 32 {
		t.Skip("JWT_SECRET is not set (or too short) — the e2e needs the stack's secret")
	}
	if e.webhookSecret == "" {
		t.Skip("STRIPE_WEBHOOK_SECRET is not set — the e2e needs Stripe enabled")
	}
	return e
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ── The test ───────────────────────────────────────────────────────────

// TestReserveToConfirmed is the whole chain in one test.
//
//	reserve ──► SQS ──► worker ──► Postgres (PENDING_PAYMENT)
//	signed webhook ──► SQS ──► stripe-worker ──► Postgres (CONFIRMED)
//
// and then the audit trail, which must show both transitions with the
// right actors.
func TestReserveToConfirmed(t *testing.T) {
	e := loadEnv(t)
	ctx := context.Background()

	// ── The clients ────────────────────────────────────────────────
	pool, err := db.NewPool(ctx, db.Config{DSN: e.dsn, MaxConns: 3, MinConns: 1})
	require.NoError(t, err, "the e2e needs a reachable Postgres")
	t.Cleanup(pool.Close)

	rdb := redis.NewClient(redis.Config{Addr: e.redisAddr})
	require.NoError(t, redis.Ping(ctx, rdb), "the e2e needs a reachable Redis")
	t.Cleanup(func() { _ = rdb.Close() })

	// ── Seed the inventory ──────────────────────────────────────────
	// The e2e does not go through cmd/seed-inventory (that is a
	// separate container); it puts the seats there directly.
	require.NoError(t, rdb.Set(ctx, redis.InventoryKey(e.eventID), 100, 0).Err())

	// ── A JWT for the customer ─────────────────────────────────────
	userID := "e2e-user-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	token, err := auth.Issue(userID, e.eventID, e.jwtSecret, 5*time.Minute)
	require.NoError(t, err)

	// ── 1. Reserve through the public API ──────────────────────────
	rid := reserve(t, e, token)
	t.Logf("reserved %s", rid)

	// ── 2. The worker writes the row ────────────────────────────────
	// The reservation only exists in Postgres after the SQS message is
	// consumed by cmd/worker. Until then the API has nothing to show.
	// The timeout has to respect the workers' long poll: a message
	// published mid-poll waits for that poll to end before it is
	// picked up. The default poll is 20 seconds, so 30 was not enough
	// — the e2e failed intermittently at exactly that boundary. The CI
	// job shortens the poll to 2 seconds; 60 is the safety margin for a
	// slow runner.
	row := eventually(t, 60*time.Second, "the worker to persist the reservation", func() (*db.ReservationRow, bool) {
		r, err := db.GetReservation(ctx, pool, rid)
		if err != nil {
			return nil, false
		}
		return &r, true
	})
	require.Equal(t, db.StatusPendingPayment, row.Status)
	require.Equal(t, 1, row.Seats)
	require.Greater(t, row.UnitPriceCents, int64(0),
		"the reservation must carry the price snapshot")

	// ── 3. A payment row, as /pay would have created ───────────────
	// The e2e cannot reach real Stripe, so it writes the row the /pay
	// handler writes. That is the point of the nullable
	// stripe_payment_intent_id: the row is durable BEFORE Stripe is
	// called.
	inserted, err := db.InsertPaymentIntent(ctx, pool, rid, userID, row.TotalPriceCents, row.Currency)
	require.NoError(t, err)
	require.True(t, inserted, "the payment row must be new")
	require.NoError(t, db.SetStripePaymentIntentID(ctx, pool, rid, e.stripePaymentI, "REQUIRES_PAYMENT_METHOD"))

	// ── 4. A signed Stripe webhook ─────────────────────────────────
	// Signed with the SDK's own ComputeSignature, so the API's
	// verification is exercised for real — not a hand-rolled
	// approximation of the format.
	eventID := "evt_e2e_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	payload := signedPaymentIntentSucceeded(t, e.webhookSecret, eventID, e.stripePaymentI, row.TotalPriceCents, row.Currency)

	resp, body := post(t, e.apiBase+"/webhooks/stripe", payload, map[string]string{
		"Stripe-Signature": signatureHeader(payload, e.webhookSecret),
		"Content-Type":     "application/json",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, "the webhook must be accepted: %s", body)

	// ── 5. The stripe-worker applies it ────────────────────────────
	confirmed := eventually(t, 60*time.Second, "the stripe-worker to confirm the reservation", func() (*db.ReservationRow, bool) {
		r, err := db.GetReservation(ctx, pool, rid)
		if err != nil || r.Status != db.StatusConfirmed {
			return nil, false
		}
		return &r, true
	})
	require.Equal(t, db.StatusConfirmed, confirmed.Status)

	// ── 6. The audit trail ──────────────────────────────────────────
	// The audit row is what makes the whole thing debuggable: an
	// operator can answer "who confirmed this, and when" without
	// reading application logs.
	events := eventually(t, 10*time.Second, "the audit trail to record the confirmation", func() ([]db.AuditEventRow, bool) {
		evs, err := db.ListAuditEvents(ctx, pool, rid)
		if err != nil {
			return nil, false
		}
		for _, ev := range evs {
			if ev.EventType == string(db.AuditEventConfirmed) {
				return evs, true
			}
		}
		return nil, false
	})

	var sawCreated, sawConfirmed bool
	for _, ev := range events {
		switch ev.EventType {
		case string(db.AuditEventCreated):
			sawCreated = true
			require.Equal(t, db.ActorTypeUser, ev.ActorType)
			require.Equal(t, userID, ev.ActorID)
		case string(db.AuditEventConfirmed):
			sawConfirmed = true
			require.Equal(t, db.ActorTypeStripeWebhook, ev.ActorType)
			require.Equal(t, eventID, ev.ActorID,
				"the actor must be the Stripe event id")
			require.Equal(t, db.StatusPendingPayment, ev.OldStatus)
			require.Equal(t, db.StatusConfirmed, ev.NewStatus)
		}
	}
	require.True(t, sawCreated, "the audit trail must record the creation")
	require.True(t, sawConfirmed, "the audit trail must record the confirmation")
}

// ── Helpers ────────────────────────────────────────────────────────────

// reserve POSTs to the public reserve endpoint and returns the id.
func reserve(t *testing.T, e env, token string) string {
	t.Helper()

	body := strings.NewReader(`{"seats_requested":1}`)
	req, err := http.NewRequest(http.MethodPost, e.apiBase+"/api/tickets/reserve", body)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, raw := do(t, req)
	require.Equal(t, http.StatusOK, resp.StatusCode, "reserve failed: %s", raw)

	var out struct {
		ReservationID string `json:"reservation_id"`
		Seats         int    `json:"seats"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &out), "reserve body: %s", raw)
	require.NotEmpty(t, out.ReservationID, "reserve body: %s", raw)
	return out.ReservationID
}

// signedPaymentIntentSucceeded builds the raw JSON body of a
// `payment_intent.succeeded` event.
func signedPaymentIntentSucceeded(t *testing.T, secret, eventID, paymentIntentID string, amountCents int64, currency string) []byte {
	t.Helper()

	body := map[string]any{
		"id":          eventID,
		"object":      "event",
		"api_version": "2025-08-27.basil",
		"type":        "payment_intent.succeeded",
		"data": map[string]any{
			"object": map[string]any{
				"id":       paymentIntentID,
				"object":   "payment_intent",
				"amount":   amountCents,
				"currency": currency,
				"status":   "succeeded",
			},
		},
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return raw
}

// signatureHeader builds the `Stripe-Signature` header the way Stripe
// does, using the SDK's own signing function.
func signatureHeader(payload []byte, secret string) string {
	sig := webhook.ComputeSignature(time.Now(), payload, secret)
	return fmt.Sprintf("t=%d,v1=%s", time.Now().Unix(), hex.EncodeToString(sig))
}

// eventually polls fn until it reports success, and fails the test
// with the label if it never does.
func eventually[T any](t *testing.T, timeout time.Duration, what string, fn func() (T, bool)) T {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last T
	for time.Now().Before(deadline) {
		v, ok := fn()
		if ok {
			return v
		}
		last = v
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
	return last
}

func post(t *testing.T, url string, body []byte, headers map[string]string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(raw)
}
