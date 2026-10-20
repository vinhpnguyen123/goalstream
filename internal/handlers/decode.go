package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

// decode reads one JSON object into v, answering 400/413 itself when it cannot.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		httpx.Error(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large")
		return false
	case err != nil:
		httpx.Error(w, r, http.StatusBadRequest, "validation_error", "request body must be a JSON object with the documented fields")
		return false
	}
	return true
}
