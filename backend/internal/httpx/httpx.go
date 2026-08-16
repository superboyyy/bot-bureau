// HTTP helpers for reading and writing JSON, shared by the engine's API and LAN discovery so the two
// do not each carry their own copy.
package httpx

import (
	"encoding/json"
	"io"
	"net/http"
)

func WriteJSON(rw http.ResponseWriter, code int, obj any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(obj)
}

// ReadJSON reads a request body with a cap: the body is external input, and an unbounded read hands
// memory over to whoever is calling.
func ReadJSON(r *http.Request, into any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}
