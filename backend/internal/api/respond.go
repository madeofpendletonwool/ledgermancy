// Package api wires the HTTP surface: routing, request decoding, and the
// JSON envelope every handler responds with.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// errorBody is the single error shape the frontend has to handle.
type errorBody struct {
	Error string `json:"error"`
}

// writeJSON sends a JSON response. Encoding failures are logged rather than
// surfaced: the status line is already on the wire by then, so there is no
// way to turn it into a useful error for the client.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("write json response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorBody{Error: message})
}

// maxRequestBody caps decoded request bodies. None of this API accepts large
// uploads, so a small ceiling costs nothing and removes a trivial DoS.
const maxRequestBody = 1 << 20 // 1 MiB

// decodeJSON reads a JSON request body into dst, rejecting unknown fields so
// a typo in a client payload fails loudly instead of being silently ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeJSONLimit(w, r, dst, maxRequestBody)
}

// decodeJSONLimit is decodeJSON with a caller-chosen body ceiling, for the few
// endpoints (CSV import) that legitimately carry a larger payload than the 1 MiB
// default. Everything else should use decodeJSON.
func decodeJSONLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntaxErr):
			return fmt.Errorf("malformed JSON at position %d", syntaxErr.Offset)
		case errors.As(err, &typeErr):
			return fmt.Errorf("field %q has the wrong type", typeErr.Field)
		case errors.Is(err, io.EOF):
			return errors.New("request body is empty")
		default:
			return fmt.Errorf("invalid request body: %w", err)
		}
	}

	// A second value in the body means the client sent something unintended.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

// decodeJSONLenient is decodeJSON without the unknown-field check, for payloads
// we do not control. Plaid adds webhook fields over time, and rejecting a
// payload because it gained a field would break syncing for no good reason.
func decodeJSONLenient(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}
