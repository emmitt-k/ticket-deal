package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/auth"
)

// ── Shared test scaffolding ─────────────────────────────────────────────
//
// Why this file exists: the handler tests used to call the handler
// function directly, e.g.
//
//	handler := auth.Middleware(secret)(GetReservationHandler(pool))
//	req := httptest.NewRequest(http.MethodGet, "/api/reservations/"+id, nil)
//	handler.ServeHTTP(rec, req)
//
// That works for handlers that read the request body or query string,
// but NOT for handlers that read a chi URL param. chi.URLParam(r, "id")
// reads the route context that chi's router puts in the request
// context — with no router, it returns "" and the handler correctly
// answers 400 missing_id.
//
// Every test that exercised GET /api/reservations/{id} or
// POST /api/reservations/{id}/cancel was therefore asserting against a
// 400 instead of the real status — and passing/failing for the wrong
// reason. The suite had never actually run in CI, because it needs a
// live Postgres and Redis and t.Skip()s when they're unreachable.
//
// These helpers mount the handlers on a REAL chi router, exactly like
// cmd/api/main.go does, so the URL params resolve the way they do in
// production.

// apiTestRouter mounts handlers on a chi router with the auth
// middleware in front, mirroring cmd/api/main.go's wiring.
//
//	register receives the router scoped under /api; it is responsible
//	for registering the routes (e.g. r.Get("/reservations/{id}", h)).
func apiTestRouter(t *testing.T, secret []byte, register func(r chi.Router)) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Middleware(secret))
		register(r)
	})
	return r
}

// apiBodyBytes reads a recorder's body and puts it back, so a test can
// decode it more than once.
//
// Without this, a helper that decodes the body (to return parsed
// JSON) leaves the recorder empty, and a test that decodes it again
// gets io.EOF instead of the error payload it was checking for.
func apiBodyBytes(t *testing.T, rec *httptest.ResponseRecorder) []byte {
	t.Helper()
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	// Put it back so the test can read it. (ResponseRecorder.Body is a
	// *bytes.Buffer, so we replace it wholesale.)
	rec.Body = bytes.NewBuffer(body)
	return body
}
