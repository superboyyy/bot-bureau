// 跨包共用的文本裁剪：工具输出和展示用的摘要都要限长，免得一条超长输出撑爆上下文或界面。
// Shared text trimming: tool output and display summaries are both capped so one oversized
// blob cannot blow up the context window or the UI.
package textutil

import "botbureau/backend/internal/i18n"

// Truncate 按上限截断，并在尾部说明被截过——静默截断会让人以为那就是全部输出。
// Truncate caps a string and says so at the end: silent truncation reads as if that were the whole output.
func Truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + i18n.T("\n...(output too long, truncated)")
}

// Brief 用于日志和审批卡这类一行展示，超长直接省略号收尾。
// Brief is for one-line displays such as logs and approval cards; overlong input just gets an ellipsis.
func Brief(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
