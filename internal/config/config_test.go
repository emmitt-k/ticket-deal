package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ── Helpers ────────────────────────────────────────────────────────────

// validSecret is a JWT_SECRET that passes the 32-byte minimum.
const validSecret = "0123456789abcdef0123456789abcdef" // 32 bytes

// clearConfigEnv blanks every environment variable Load() reads.
//
// Two reasons this is not optional:
//
//  1. `t.Setenv` marks a variable as SET even when the value is empty,
//     and `godotenv.Load()` (called inside Load) does not override a
//     variable that is already set. So blanking wins over a developer's
//     local `.env`.
//  2. A test that reads an unset variable is testing whatever the
//     developer's shell happened to have. That is how a test passes on
//     one machine and fails on another.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"API_PORT", "JWT_SECRET",
		"REDIS_ADDR", "REDIS_DB",
		"IP_LIMIT_CAPACITY", "IP_LIMIT_REFILL_RATE",
		"WAIT_ROOM_CAPACITY", "WAIT_ROOM_REFILL_RATE", "WAIT_ROOM_QUEUE_TTL",
		"RESERVE_HOLD_TTL_SECONDS",
		"AWS_REGION", "SQS_ENDPOINT_URL", "SQS_QUEUE_URL",
		"DATABASE_URL",
		"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_DB",
		"STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET",
		"STRIPE_PUBLISHABLE_KEY", "STRIPE_EVENTS_QUEUE_URL",
	} {
		t.Setenv(k, "")
	}
}

// loadOK loads the config with a valid JWT secret already set, and
// fails the test if Load returns an error.
func loadOK(t *testing.T) *Config {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("JWT_SECRET", validSecret)
	cfg, err := Load()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	return cfg
}

// ── Defaults ───────────────────────────────────────────────────────────

// TestLoad_Defaults — with nothing set, every knob gets its documented
// default. If a default changes, this is the test that notices.
func TestLoad_Defaults(t *testing.T) {
	cfg := loadOK(t)

	require.Equal(t, ":8080", cfg.Addr)
	require.Equal(t, []byte(validSecret), cfg.JWTSecret)
	require.Equal(t, "localhost:6379", cfg.Redis.Addr)
	require.Equal(t, 0, cfg.Redis.DB)
	require.Equal(t, 10, cfg.IPLimit.Capacity)
	require.Equal(t, 2.0, cfg.IPLimit.RefillRate)
	require.Equal(t, 100, cfg.WaitRoom.Capacity)
	require.Equal(t, 10.0, cfg.WaitRoom.RefillRate)
	require.Equal(t, 300, cfg.WaitRoom.QueueTTLSeconds)
	require.Equal(t, 600, cfg.ReserveHoldTTL)
	require.Equal(t, "us-east-1", cfg.SQS.Region)
	require.Empty(t, cfg.SQS.EndpointURL)
	require.Empty(t, cfg.SQS.QueueURL)
}

// TestLoad_NumericOverrides — the numeric knobs come from the env and
// are parsed, not stringly kept.
func TestLoad_NumericOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("API_PORT", "9999")
	t.Setenv("REDIS_ADDR", "redis:6379")
	t.Setenv("REDIS_DB", "3")
	t.Setenv("IP_LIMIT_CAPACITY", "42")
	t.Setenv("IP_LIMIT_REFILL_RATE", "7.5")
	t.Setenv("WAIT_ROOM_CAPACITY", "500")
	t.Setenv("WAIT_ROOM_REFILL_RATE", "25")
	t.Setenv("WAIT_ROOM_QUEUE_TTL", "120")
	t.Setenv("RESERVE_HOLD_TTL_SECONDS", "900")
	t.Setenv("AWS_REGION", "ap-southeast-1")
	t.Setenv("SQS_ENDPOINT_URL", "http://elasticmq:9324")
	t.Setenv("SQS_QUEUE_URL", "http://elasticmq:9324/000000000000/reservations")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, ":9999", cfg.Addr)
	require.Equal(t, "redis:6379", cfg.Redis.Addr)
	require.Equal(t, 3, cfg.Redis.DB)
	require.Equal(t, 42, cfg.IPLimit.Capacity)
	require.Equal(t, 7.5, cfg.IPLimit.RefillRate)
	require.Equal(t, 500, cfg.WaitRoom.Capacity)
	require.Equal(t, 25.0, cfg.WaitRoom.RefillRate)
	require.Equal(t, 120, cfg.WaitRoom.QueueTTLSeconds)
	require.Equal(t, 900, cfg.ReserveHoldTTL)
	require.Equal(t, "ap-southeast-1", cfg.SQS.Region)
	require.Equal(t, "http://elasticmq:9324", cfg.SQS.EndpointURL)
}

// TestLoad_NumericGarbageFallsBackToDefault — a malformed number is
// NOT a startup error; it falls back to the default. The alternative
// (refusing to boot because someone typed "ten" into an env var) is a
// worse failure mode than a slightly wrong rate limit.
func TestLoad_NumericGarbageFallsBackToDefault(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("IP_LIMIT_CAPACITY", "ten")
	t.Setenv("IP_LIMIT_REFILL_RATE", "not-a-float")
	t.Setenv("RESERVE_HOLD_TTL_SECONDS", "soon")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, 10, cfg.IPLimit.Capacity)
	require.Equal(t, 2.0, cfg.IPLimit.RefillRate)
	require.Equal(t, 600, cfg.ReserveHoldTTL)
}

// ── Startup errors ─────────────────────────────────────────────────────

// TestLoad_JWTSecretTooShort — a short signing key is a startup error.
// This is the one that must NOT be a warning: a weak HS256 key means
// anyone can mint a valid JWT for any user.
func TestLoad_JWTSecretTooShort(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JWT_SECRET", "too-short")

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "JWT_SECRET")
}

// TestLoad_JWTSecretBoundary — exactly 32 bytes is accepted; 31 is not.
func TestLoad_JWTSecretBoundary(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JWT_SECRET", strings.Repeat("a", 31))
	_, err := Load()
	require.Error(t, err, "31 bytes must be rejected")

	t.Setenv("JWT_SECRET", strings.Repeat("a", 32))
	cfg, err := Load()
	require.NoError(t, err, "32 bytes must be accepted")
	require.Len(t, cfg.JWTSecret, 32)
}

// TestLoad_InvalidPort — a bad API_PORT is a startup error, because
// the server cannot listen and there is no sensible default to fall
// back to (silently binding a different port would be worse).
func TestLoad_InvalidPort(t *testing.T) {
	for _, port := range []string{"abc", "0", "-1", "70000", "8080.5"} {
		t.Run(port, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("JWT_SECRET", validSecret)
			t.Setenv("API_PORT", port)

			_, err := Load()
			require.Error(t, err)
			require.Contains(t, err.Error(), "API_PORT")
		})
	}
}

// ── The Stripe both-or-neither rule ────────────────────────────────────
//
// This is the rule that is documented in three places (the code, the
// AGENTS.md convention, and the Stripe decision log) and was, until
// now, only ever verified by running the binary by hand. A regression
// here is a production incident: an API that can create PaymentIntents
// but cannot verify webhooks is exactly the state where payments get
// silently stuck.

// TestLoad_StripeAbsentIsAllowed — neither key set means the feature
// is off. That is a CHOICE, not an error: the reservation core does
// not need a payment account, and a ticket server refusing to boot
// without one is a strange dependency.
func TestLoad_StripeAbsentIsAllowed(t *testing.T) {
	cfg := loadOK(t)

	require.False(t, cfg.Stripe.Enabled, "Stripe must be disabled with no keys")
	require.Empty(t, cfg.Stripe.SecretKey)
	require.Empty(t, cfg.Stripe.WebhookSecret)
}

// TestLoad_StripeHalfConfiguredIsAnError — exactly one key is a BUG,
// and it is a startup error rather than a runtime surprise.
func TestLoad_StripeHalfConfiguredIsAnError(t *testing.T) {
	t.Run("secret only", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("JWT_SECRET", validSecret)
		t.Setenv("STRIPE_SECRET_KEY", "sk_test_abc")

		_, err := Load()
		require.Error(t, err)
		require.Contains(t, err.Error(), "STRIPE_SECRET_KEY")
		require.Contains(t, err.Error(), "STRIPE_WEBHOOK_SECRET")
	})

	t.Run("webhook secret only", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("JWT_SECRET", validSecret)
		t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_abc")

		_, err := Load()
		require.Error(t, err)
		require.Contains(t, err.Error(), "STRIPE_SECRET_KEY")
		require.Contains(t, err.Error(), "STRIPE_WEBHOOK_SECRET")
	})
}

// TestLoad_StripeMalformedKeyIsAnError — a key that cannot possibly
// work is caught at startup, not at the first /pay request.
func TestLoad_StripeMalformedKeyIsAnError(t *testing.T) {
	t.Run("secret does not look like a key", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("JWT_SECRET", validSecret)
		t.Setenv("STRIPE_SECRET_KEY", "not-a-stripe-key")
		t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_abc")

		_, err := Load()
		require.Error(t, err)
		require.Contains(t, err.Error(), "STRIPE_SECRET_KEY")
	})

	t.Run("webhook secret does not look like a key", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("JWT_SECRET", validSecret)
		t.Setenv("STRIPE_SECRET_KEY", "sk_test_abc")
		t.Setenv("STRIPE_WEBHOOK_SECRET", "nope")

		_, err := Load()
		require.Error(t, err)
		require.Contains(t, err.Error(), "STRIPE_WEBHOOK_SECRET")
	})
}

// TestLoad_StripeFullyConfigured — both keys present and well-formed
// turns the feature on.
func TestLoad_StripeFullyConfigured(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_abc")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_abc")
	t.Setenv("STRIPE_PUBLISHABLE_KEY", "pk_test_abc")
	t.Setenv("STRIPE_EVENTS_QUEUE_URL", "http://elasticmq:9324/000000000000/stripe-events")

	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.Stripe.Enabled)
	require.Equal(t, "sk_test_abc", cfg.Stripe.SecretKey)
	require.Equal(t, "whsec_abc", cfg.Stripe.WebhookSecret)
	require.Equal(t, "pk_test_abc", cfg.Stripe.PublishableKey)
	require.Equal(t, "http://elasticmq:9324/000000000000/stripe-events", cfg.Stripe.EventsQueueURL)
}

// ── The Postgres DSN ───────────────────────────────────────────────────

// TestLoad_DatabaseURLWins — an explicit DATABASE_URL takes priority
// over the POSTGRES_* family. It can encode sslmode, IAM auth, or
// anything else the part-by-part form cannot.
func TestLoad_DatabaseURLWins(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("DATABASE_URL", "postgres://u:p@db.internal:5432/prod?sslmode=require")
	t.Setenv("POSTGRES_USER", "ignored")
	t.Setenv("POSTGRES_HOST", "ignored")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "postgres://u:p@db.internal:5432/prod?sslmode=require", cfg.Postgres.DSN)
}

// TestLoad_DatabaseURLFromParts — with no DATABASE_URL, the DSN is
// composed from the POSTGRES_* family.
func TestLoad_DatabaseURLFromParts(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("POSTGRES_USER", "alice")
	t.Setenv("POSTGRES_PASSWORD", "s3cret")
	t.Setenv("POSTGRES_HOST", "pg")
	t.Setenv("POSTGRES_PORT", "6543")
	t.Setenv("POSTGRES_DB", "ticketing")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "postgres://alice:s3cret@pg:6543/ticketing?sslmode=disable", cfg.Postgres.DSN)
}

// TestBuildPostgresDSNFromEnv — the composed DSN's exact shape, and
// its defaults for every part that is not set.
func TestBuildPostgresDSNFromEnv(t *testing.T) {
	clearConfigEnv(t)

	require.Equal(t, "postgres://tickets:tickets@localhost:5432/tickets?sslmode=disable",
		buildPostgresDSNFromEnv())

	t.Setenv("POSTGRES_USER", "u")
	t.Setenv("POSTGRES_PASSWORD", "p")
	t.Setenv("POSTGRES_HOST", "h")
	t.Setenv("POSTGRES_PORT", "1")
	t.Setenv("POSTGRES_DB", "d")
	require.Equal(t, "postgres://u:p@h:1/d?sslmode=disable", buildPostgresDSNFromEnv())
}

// ── The small helpers ──────────────────────────────────────────────────

func TestAtoiOr(t *testing.T) {
	require.Equal(t, 7, atoiOr("7", 3))
	require.Equal(t, 3, atoiOr("", 3))
	require.Equal(t, 3, atoiOr("abc", 3))
	require.Equal(t, 3, atoiOr("3.5", 3))
}

func TestAtofOr(t *testing.T) {
	require.Equal(t, 2.5, atofOr("2.5", 1.0))
	require.Equal(t, 1.0, atofOr("", 1.0))
	require.Equal(t, 1.0, atofOr("abc", 1.0))
}

func TestGetEnv(t *testing.T) {
	t.Setenv("TD_TEST_GET_ENV", "value")
	require.Equal(t, "value", getEnv("TD_TEST_GET_ENV", "fallback"))

	t.Setenv("TD_TEST_GET_ENV", "")
	require.Equal(t, "fallback", getEnv("TD_TEST_GET_ENV", "fallback"))
}
