package model

import "testing"

func TestQuotaClassification(t *testing.T) {
	cases := []struct {
		status int
		msg    string
		quota  bool
	}{
		{400, "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing.", true},
		{429, "You exceeded your current quota, please check your plan and billing details.", true},
		{429, "insufficient_quota", true},
		{402, "payment required", true},
		// 普通限流不算额度耗尽
		// Ordinary rate limiting does not count as quota exhaustion
		{429, "Rate limit reached for requests", false},
		{500, "internal server error", false},
	}
	for _, c := range cases {
		got := classifyQuota(c.status, "test:model", c.msg)
		if (got != "") != c.quota {
			t.Fatalf("classifyQuota(%d, %q) = %q, want quota=%v", c.status, c.msg, got, c.quota)
		}
	}
}
