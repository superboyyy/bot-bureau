// HTTP helpers for reading and writing JSON, shared by the engine's API and LAN discovery so the two
// do not each carry their own copy.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const defaultJSONMax = 1 << 20

// ErrTooLarge is returned when a request body exceeds the cap ReadJSON / ReadJSONMax will take.
var ErrTooLarge = errors.New("request body too large")

func WriteJSON(rw http.ResponseWriter, code int, obj any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(obj)
}

// ReadJSON reads a request body with a 1MiB cap: the body is external input, and an unbounded read
// hands memory over to whoever is calling. Endpoints that legitimately carry more (composer
// attachments, which travel as base64 inside JSON) use ReadJSONMax with a larger cap.
func ReadJSON(r *http.Request, into any) error {
	return ReadJSONMax(r, into, defaultJSONMax)
}

// ReadJSONMax is ReadJSON with a caller-chosen cap. Reading one extra byte past the cap is how an
// oversize body is told apart from truncated JSON: LimitReader alone would hand json.Unmarshal a
// cut-off object and every too-large screenshot would surface as "invalid request body".
func ReadJSONMax(r *http.Request, into any, max int64) error {
	if max <= 0 {
		max = defaultJSONMax
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > max {
		return ErrTooLarge
	}
	return json.Unmarshal(raw, into)
}
