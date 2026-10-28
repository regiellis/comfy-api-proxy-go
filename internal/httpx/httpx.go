// Package httpx holds the response helpers every v2 handler shares.
package httpx

import (
	"encoding/json"
	"net/http"
)

// Envelope is the v2 error shape: {"error": {code, message, details}}.
// SDKs match on code, so codes are the reference's exact strings.
type Envelope struct {
	Error Detail `json:"error"`
}

// Detail is the body of an error envelope.
type Detail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// WriteJSON sends v with status. An encode failure after the status line
// means the client went away, so there is nothing left to report to it.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

// WriteError sends the error envelope. A nil details map encodes as null, as
// the reference does.
func WriteError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	WriteJSON(w, status, Envelope{Detail{code, message, details}})
}
