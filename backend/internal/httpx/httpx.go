// HTTP 小工具：JSON 读写。引擎的 API 和局域网发现都要用，放一处免得两边各写一份。
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

// ReadJSON 限流读请求体：请求体是外部输入，不设上限等于把内存交给对方。
// ReadJSON reads a request body with a cap: the body is external input, and an unbounded read hands
// memory over to whoever is calling.
func ReadJSON(r *http.Request, into any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}
