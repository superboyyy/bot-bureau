package netx

// SSE 票据。
//
// 配对码本该只走 Authorization 头——头基本不会被记录，URL 到处都会被记录。但消息推送用的是
// 浏览器的 EventSource，而 EventSource API 不支持自定义请求头（换 WebSocket 也一样，浏览器那边
// 同样加不了头）。于是那一条连接只能把凭据放进 query，而 query 会被反向代理原样写进 access log，
// 日志还会轮转、备份、进集中式日志系统。公网 + 反代的部署里，这等于把配对码明文摊在日志里。
//
// 所以配对码不再接受 query 传递。客户端先用带头的 POST 换一张短时效票据，只有票据出现在 URL 上：
// 票据只对 /api/events 有效，十分钟过期，漏进日志也早已作废。
//
// SSE tickets.
//
// The pairing code should only ever travel in the Authorization header — headers are rarely logged,
// URLs are logged everywhere. But the message stream is a browser EventSource, and the EventSource API
// cannot set custom headers (switching to WebSocket does not help; the browser API cannot set them
// either). That one connection is therefore forced to carry its credential in the query string, which
// a reverse proxy writes verbatim into its access log — logs that then rotate, get backed up and ship
// to central logging. On a public deployment behind a proxy that leaves the pairing code sitting in
// plain text in a log file.
//
// So the pairing code is no longer accepted from the query at all. A client first exchanges it, over a
// POST that can set headers, for a short-lived ticket; only the ticket ever appears in a URL. A ticket
// works solely on /api/events and expires in ten minutes, so one that reaches a log is long dead.

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// TicketTTL 是票据的有效期。
//
// 十分钟：足够撑过 EventSource 自己的断线重连（它会在我们的代码跑之前先自动重连几次，
// 一次性票据会在那里直接 401），又短到日志里的票据等人看到时早就没用了。
//
// TicketTTL is how long a ticket stays valid.
//
// Ten minutes: long enough to survive EventSource's own reconnection attempts (it retries internally
// before our code ever runs, where a single-use ticket would simply 401), and short enough that a
// ticket found in a log is useless by the time anyone reads it.
const TicketTTL = 10 * time.Minute

type ticketStore struct {
	mu      sync.Mutex
	expires map[string]time.Time
}

func newTicketStore() *ticketStore {
	return &ticketStore{expires: map[string]time.Time{}}
}

// issue 发一张新票据，顺手清掉过期的。
// issue mints a ticket and sweeps expired ones on the way.
func (s *ticketStore) issue() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for t, exp := range s.expires {
		if now.After(exp) {
			delete(s.expires, t)
		}
	}
	s.expires[tok] = now.Add(TicketTTL)
	return tok, nil
}

// valid 判断票据是否还在有效期内。票据可在有效期内重复使用——
// EventSource 的内部重连不经过我们的代码，一次性票据会让它在那里断掉。
//
// valid reports whether a ticket is still live. Tickets are reusable within their window:
// EventSource's internal reconnects never pass through our code, and a single-use ticket would break
// the connection right there.
func (s *ticketStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.expires[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.expires, tok)
		return false
	}
	return true
}
