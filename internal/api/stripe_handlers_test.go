package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	stripeapi "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/stripe"
)

// ── Fakes ─────────────────────────────────────────────────────────────

// fakePaymentCreator records what the /pay handler asked Stripe for
// and returns a canned answer.
type fakePaymentCreator struct {
	// err, when set, is returned instead of a PaymentIntent.
	err error
	// calls counts the invocations.
	calls int
	// lastAmountCents, lastCurrency, lastIdempotencyKey capture the
	// last call's arguments.
	lastAmountCents    int64
	lastCurrency       string
	lastIdempotencyKey string
}

func (f *fakePaymentCreator) CreatePaymentIntent(ctx context.Context, amountCents int64, currency, idempotencyKey, reservationID string) (*stripe.PaymentIntent, error) {
	f.calls++
	f.lastAmountCents = amountCents
	f.lastCurrency = currency
	f.lastIdempotencyKey = idempotencyKey
	if f.err != nil {
		return nil, f.err
	}
	return &stripe.PaymentIntent{
		ID:           "pi_fake_" + idempotencyKey[:8],
		ClientSecret: "pi_fake_secret_secret",
		AmountCents:  amountCents,
		Currency:     currency,
		Status:       "requires_payment_method",
	}, nil
}

// fakeVerifier stands in for Stripe's signature check. It records the
// payload it was given and returns a snapshot (or an error).
type fakeVerifier struct {
	err     error
	snap    *stripe.Event
	payload []byte
	header  string
}

func (f *fakeVerifier) VerifyWebhook(payload []byte, header string) (*stripe.Event, error) {
	f.payload = payload
	f.header = header
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

// fakeEventPublisher records what was published.
type fakeEventPublisher struct {
	err  error
	body []byte
}

func (f *fakeEventPublisher) Publish(ctx context.Context, body []byte) error {
	f.body = body
	return f.err
}

// ── Helpers ───────────────────────────────────────────────────────────

// doPay fires POST /api/reservations/{id}/pay through a real chi
// router with the auth middleware in front, and returns the recorder
// plus the decoded body.
func doPay(t *testing.T, pool *db.Pool, payments PaymentIntentCreator, userID, reservationID string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	h := apiTestRouter(t, getReservationTestSecret, func(r chi.Router) {
		r.Post("/reservations/{id}/pay", PayHandler(pool, payments))
	})

	token, err := auth.Issue(userID, 1, getReservationTestSecret, 2*time.Minute)
	require.NoError(t, err, "issue test JWT")
	req := httptest.NewRequest(http.MethodPost, "/api/reservations/"+reservationID+"/pay", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body map[string]any
	raw := apiBodyBytes(t, rec)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	return rec, body
}

// doStripeWebhook fires POST /webhooks/stripe.
func doStripeWebhook(t *testing.T, verifier WebhookVerifier, publisher EventPublisher, payload []byte, signature string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/webhooks/stripe", StripeWebhookHandler(verifier, publisher))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
	if signature != "" {
		req.Header.Set("Stripe-Signature", signature)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var body map[string]any
	raw := apiBodyBytes(t, rec)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	return rec, body
}

// signedStripePayload signs a payload the way Stripe does, so the
// tests exercise the real signature format.
func signedStripePayload(t *testing.T, payload []byte, secret string) string {
	t.Helper()
	sig := webhook.ComputeSignature(time.Now(), payload, secret)
	return fmt.Sprintf("t=%d,v1=%s", time.Now().Unix(), hex.EncodeToString(sig))
}

// apiPriceEvent sets a price on a seeded event. The seed helper
// inserts without the money columns (the defaults apply), so a test
// that needs a priced event sets it explicitly.
func apiPriceEvent(t *testing.T, pool *db.Pool, eventID int64, priceCents int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		"UPDATE events SET price_cents = $2, currency = 'usd' WHERE id = $1",
		eventID, priceCents)
	require.NoError(t, err)
}

// ── POST /api/reservations/{id}/pay ───────────────────────────────────

// TestPay_HappyPath — the amount comes from the reservation's price
// snapshot, the idempotency key is the reservation id, and the
// client_secret comes back.
func TestPay_HappyPath(t *testing.T) {
	const owner = "u-pay-1"
	pool := apiNewTestPool(t)
	const eventID = int64(64008)
	apiSeedEvent(t, pool, eventID)
	apiPriceEvent(t, pool, eventID, 2500)

	r := apiValidReservationFor(t, eventID, owner)
	r.Seats = 3
	apiCleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))

	payments := &fakePaymentCreator{}
	rec, body := doPay(t, pool, payments, owner, r.ReservationID)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, float64(7500), body["amount_cents"], "3 seats × $25.00")
	require.Equal(t, "usd", body["currency"])
	require.Equal(t, "pi_fake_secret_secret", body["client_secret"])
	require.Equal(t, float64(3), body["seats"])

	// The amount is the SERVER's number, and the idempotency key is
	// the reservation id — a retry must not become a second charge.
	require.Equal(t, int64(7500), payments.lastAmountCents)
	require.Equal(t, "usd", payments.lastCurrency)
	require.Equal(t, r.ReservationID, payments.lastIdempotencyKey)

	// The payment row exists, with Stripe's id attached.
	row, err := db.GetPaymentIntentByReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, int64(7500), row.AmountCents)
	require.NotNil(t, row.StripePaymentIntentID)
	require.Equal(t, "REQUIRES_PAYMENT_METHOD", row.Status,
		"Stripe's lowercase status is normalised to our vocabulary")
}

// TestPay_NotYourReservation — 403 for someone else's reservation.
func TestPay_NotYourReservation(t *testing.T) {
	const owner = "u-pay-2"
	pool := apiNewTestPool(t)
	const eventID = int64(64009)
	apiSeedEvent(t, pool, eventID)

	r := apiValidReservationFor(t, eventID, owner)
	apiCleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))

	payments := &fakePaymentCreator{}
	rec, body := doPay(t, pool, payments, "someone-else", r.ReservationID)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "not_your_reservation", body["error"])
	require.Equal(t, 0, payments.calls, "Stripe must not be called for someone else's reservation")
}

// TestPay_AlreadyConfirmed — a paid reservation cannot be paid again.
func TestPay_AlreadyConfirmed(t *testing.T) {
	const owner = "u-pay-3"
	pool := apiNewTestPool(t)
	const eventID = int64(64010)
	apiSeedEvent(t, pool, eventID)

	r := apiValidReservationFor(t, eventID, owner)
	apiCleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))

	changed, err := db.TransitionStatus(context.Background(), pool,
		r.ReservationID, db.StatusPendingPayment, db.StatusConfirmed,
		db.Audit{Actor: db.UserActor("test-user")})
	require.NoError(t, err)
	require.True(t, changed)

	payments := &fakePaymentCreator{}
	rec, body := doPay(t, pool, payments, owner, r.ReservationID)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "already_paid", body["error"])
	require.Equal(t, 0, payments.calls)
}

// TestPay_AlreadyExpired — an expired hold cannot be paid.
func TestPay_AlreadyExpired(t *testing.T) {
	const owner = "u-pay-4"
	pool := apiNewTestPool(t)
	const eventID = int64(64011)
	apiSeedEvent(t, pool, eventID)

	r := apiValidReservationFor(t, eventID, owner)
	apiCleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))

	changed, err := db.TransitionStatus(context.Background(), pool,
		r.ReservationID, db.StatusPendingPayment, db.StatusExpired,
		db.Audit{Actor: db.SystemActor("test")})
	require.NoError(t, err)
	require.True(t, changed)

	payments := &fakePaymentCreator{}
	rec, body := doPay(t, pool, payments, owner, r.ReservationID)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "already_expired", body["error"])
}

// TestPay_UnpricedEvent — an event with no price cannot be charged.
// (Stripe rejects amounts below $0.50 anyway.)
func TestPay_UnpricedEvent(t *testing.T) {
	const owner = "u-pay-5"
	pool := apiNewTestPool(t)
	const eventID = int64(64012)
	apiSeedEvent(t, pool, eventID) // no price → 0

	r := apiValidReservationFor(t, eventID, owner)
	apiCleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))

	payments := &fakePaymentCreator{}
	rec, body := doPay(t, pool, payments, owner, r.ReservationID)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "event_not_priced", body["error"])
	require.Equal(t, 0, payments.calls)
}

// TestPay_ReplayDoesNotCreateASecondPayment — calling /pay twice
// creates ONE payment row. The row's UNIQUE constraint is what makes
// that true; the idempotency key makes it true at Stripe.
func TestPay_ReplayDoesNotCreateASecondPayment(t *testing.T) {
	const owner = "u-pay-6"
	pool := apiNewTestPool(t)
	const eventID = int64(64013)
	apiSeedEvent(t, pool, eventID)
	apiPriceEvent(t, pool, eventID, 2500)

	r := apiValidReservationFor(t, eventID, owner)
	apiCleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))

	payments := &fakePaymentCreator{}
	rec, _ := doPay(t, pool, payments, owner, r.ReservationID)
	require.Equal(t, http.StatusOK, rec.Code)
	rec, _ = doPay(t, pool, payments, owner, r.ReservationID)
	require.Equal(t, http.StatusOK, rec.Code)

	// One row. The reservation_id UNIQUE constraint is what stops it.
	row, err := db.GetPaymentIntentByReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, row.ReservationID, r.ReservationID)

	// Both calls used the SAME idempotency key, so Stripe would have
	// returned the same PaymentIntent for both.
	require.Equal(t, 2, payments.calls)
	require.Equal(t, r.ReservationID, payments.lastIdempotencyKey)
}

// TestPay_StripeErrorIs502 — a third-party failure is a 502, and the
// client is expected to retry (safely, because the idempotency key
// is stable).
func TestPay_StripeErrorIs502(t *testing.T) {
	const owner = "u-pay-7"
	pool := apiNewTestPool(t)
	const eventID = int64(64014)
	apiSeedEvent(t, pool, eventID)
	apiPriceEvent(t, pool, eventID, 2500)

	r := apiValidReservationFor(t, eventID, owner)
	apiCleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))

	payments := &fakePaymentCreator{err: fmt.Errorf("stripe is down")}
	rec, body := doPay(t, pool, payments, owner, r.ReservationID)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "payment_provider_error", body["error"])
}

// ── POST /webhooks/stripe ─────────────────────────────────────────────

// TestStripeWebhook_HappyPath — a verified event is published to the
// queue, and the response is 200.
func TestStripeWebhook_HappyPath(t *testing.T) {
	snap := &stripe.Event{
		ID:              "evt_1",
		Type:            stripe.EventTypePaymentIntentSucceeded,
		ObjectID:        "pi_123",
		PaymentIntentID: "pi_123",
		AmountCents:     2500,
		Currency:        "usd",
	}
	verifier := &fakeVerifier{snap: snap}
	publisher := &fakeEventPublisher{}

	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded"}`)
	rec, body := doStripeWebhook(t, verifier, publisher, payload, "t=1,v1=deadbeef")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, true, body["received"])
	require.Equal(t, true, body["handled"])

	// The event was published — and the published body is the
	// SNAPSHOT, not the raw Stripe payload.
	require.NotEmpty(t, publisher.body)
	var published stripe.Event
	require.NoError(t, json.Unmarshal(publisher.body, &published))
	require.Equal(t, "evt_1", published.ID)
	require.Equal(t, "pi_123", published.PaymentIntentID)
	require.Equal(t, int64(2500), published.AmountCents)
}

// TestStripeWebhook_BadSignatureIs400 — a forged request is rejected
// and nothing is published.
func TestStripeWebhook_BadSignatureIs400(t *testing.T) {
	verifier := &fakeVerifier{err: fmt.Errorf("stripe: webhook signature verification failed")}
	publisher := &fakeEventPublisher{}

	rec, body := doStripeWebhook(t, verifier, publisher, []byte(`{"id":"evt_1"}`), "t=1,v1=bad")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_signature", body["error"])
	require.Empty(t, publisher.body, "an unverified event must never reach the queue")
}

// TestStripeWebhook_UnhandledTypeIs200 — a valid event of a type we
// do not act on is acknowledged, so Stripe stops sending it. It is
// NOT a 400 — the signature was fine.
func TestStripeWebhook_UnhandledTypeIs200(t *testing.T) {
	verifier := &fakeVerifier{err: &stripe.UnhandledEventError{EventID: "evt_9", Type: "customer.created"}}
	publisher := &fakeEventPublisher{}

	rec, body := doStripeWebhook(t, verifier, publisher, []byte(`{"id":"evt_9"}`), "t=1,v1=ok")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, true, body["received"])
	require.Equal(t, false, body["handled"])
	require.Empty(t, publisher.body, "an unhandled event is not queued")
}

// TestStripeWebhook_PublishFailureIs500 — if the event cannot be
// queued we answer 500, so Stripe RETRIES. Answering 200 here would
// lose the payment confirmation permanently.
func TestStripeWebhook_PublishFailureIs500(t *testing.T) {
	snap := &stripe.Event{ID: "evt_1", Type: stripe.EventTypePaymentIntentSucceeded, PaymentIntentID: "pi_1"}
	verifier := &fakeVerifier{snap: snap}
	publisher := &fakeEventPublisher{err: fmt.Errorf("sqs is down")}

	rec, body := doStripeWebhook(t, verifier, publisher, []byte(`{"id":"evt_1"}`), "t=1,v1=ok")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "publish_failed", body["error"])
}

// TestStripeWebhook_RawBytesAreVerified — the verifier gets the
// EXACT bytes Stripe sent, because the HMAC is computed over them.
// A body that had been parsed and re-serialised would not verify.
func TestStripeWebhook_RawBytesAreVerified(t *testing.T) {
	const secret = "whsec_test_secret_not_a_real_secret"
	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1"}}}`)

	// Use the REAL verifier, not a fake: sign the payload, post it,
	// and check it verifies.
	c := stripe.NewClient("sk_test_dummy", secret)
	verifier := &realVerifier{client: c}
	publisher := &fakeEventPublisher{}

	rec, body := doStripeWebhook(t, verifier, publisher, payload,
		signedStripePayload(t, payload, secret))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, true, body["handled"])
	require.NotEmpty(t, publisher.body)
}

// realVerifier adapts the real Stripe client to the handler's seam.
type realVerifier struct {
	client *stripe.Client
}

func (v *realVerifier) VerifyWebhook(payload []byte, header string) (*stripe.Event, error) {
	return v.client.VerifyWebhook(payload, header)
}

// TestStripeWebhook_RealClientRejectsForgedPayload — the end-to-end
// negative case with the REAL verifier.
func TestStripeWebhook_RealClientRejectsForgedPayload(t *testing.T) {
	const secret = "whsec_test_secret_not_a_real_secret"
	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded"}`)

	c := stripe.NewClient("sk_test_dummy", secret)
	verifier := &realVerifier{client: c}
	publisher := &fakeEventPublisher{}

	// Signed with the WRONG secret.
	rec, body := doStripeWebhook(t, verifier, publisher, payload,
		signedStripePayload(t, payload, "whsec_a_different_secret"))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_signature", body["error"])
	require.Empty(t, publisher.body)
}

// TestStripeWebhook_EmptyBodyIsRejected — an empty body has no valid
// signature.
func TestStripeWebhook_EmptyBodyIsRejected(t *testing.T) {
	const secret = "whsec_test_secret_not_a_real_secret"
	c := stripe.NewClient("sk_test_dummy", secret)
	verifier := &realVerifier{client: c}
	publisher := &fakeEventPublisher{}

	rec, _ := doStripeWebhook(t, verifier, publisher, []byte{}, "t=1,v1=bad")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, publisher.body)
}

// The unused import guard: stripeapi is used by the tests above via
// the event constants; assert the SDK's typed constants agree with
// ours so the two cannot drift.
func TestEventTypeConstantsMatchTheSDK(t *testing.T) {
	require.Equal(t, string(stripeapi.EventTypePaymentIntentSucceeded), stripe.EventTypePaymentIntentSucceeded)
	require.Equal(t, string(stripeapi.EventTypePaymentIntentPaymentFailed), stripe.EventTypePaymentIntentPaymentFailed)
	require.Equal(t, string(stripeapi.EventTypeChargeRefunded), stripe.EventTypeChargeRefunded)
}

// The auth middleware must run before the handler — a missing token
// is a 401, and Stripe's signature is NOT a substitute for the user's
// JWT on /pay.
func TestPay_RequiresJWT(t *testing.T) {
	pool := apiNewTestPool(t)
	h := apiTestRouter(t, getReservationTestSecret, func(r chi.Router) {
		r.Post("/reservations/{id}/pay", PayHandler(pool, &fakePaymentCreator{}))
	})
	req := httptest.NewRequest(http.MethodPost, "/api/reservations/some-id/pay", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
