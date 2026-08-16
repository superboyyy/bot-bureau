// Shared text trimming: tool output and display summaries are both capped so one oversized
// blob cannot blow up the context window or the UI.
package textutil

import "botbureau/backend/internal/i18n"

// Truncate caps a string and says so at the end: silent truncation reads as if that were the whole output.
func Truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + i18n.T("\n...(output too long, truncated)")
}

// Brief is for one-line displays such as logs and approval cards; overlong input just gets an ellipsis.
func Brief(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
