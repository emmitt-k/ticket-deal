// Package apiutil holds HTTP response helpers used by handlers and
// middleware across the API. Centralising the JSON shape keeps every
// endpoint looking the same and makes it trivial to add envelope
// fields later (request_id, server_version, etc.) in one place.
package apiutil

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorBody is the stable JSON shape of every error response. Every
// non-2xx response should use this exact struct so clients have a
// single, well-documented contract.
type ErrorBody struct {
	Error   string `json:"error"`   // short machine code, e.g. "sold_out"
	Message string `json:"message"` // human-readable explanation
}

// WriteJSON encodes v as JSON and writes it with the given status.
// Any encoding failure falls back to a plain 500 — it should be
// impossible for our own payloads, and silently failing the body is
// worse than telling the client something went wrong.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode failed", "error", err)
	}
}

// WriteError is a shortcut for emitting an ErrorBody.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: code, Message: message})
}
