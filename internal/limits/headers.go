package limits

import (
	"math"
	"net/http"
	"strconv"
	"time"
)

// SetHeaders emits the OpenAI six-header shape even when a unit is unlimited
// (0 / 0 / 0s). Token headers select the tightest input/output/total policy.
// OpenAI reset values are durations, e.g. 1s or 6m0s, not epoch timestamps:
// https://developers.openai.com/api/docs/guides/rate-limits
func SetHeaders(header http.Header, snapshots []Snapshot) {
	var requests, tokens, usd *Snapshot
	for index := range snapshots {
		entry := &snapshots[index]
		if entry.Action == Log {
			continue
		}
		switch entry.Unit {
		case Requests:
			requests = restrictive(requests, entry)
		case InputTokens, OutputTokens, TotalTokens:
			tokens = restrictive(tokens, entry)
		case USD:
			usd = restrictive(usd, entry)
		}
	}
	setUnitHeaders(header, "x-ratelimit-", "requests", requests)
	setUnitHeaders(header, "x-ratelimit-", "tokens", tokens)
	if usd != nil {
		setUnitHeaders(header, "x-omniswitch-", "usd", usd)
	} else {
		for _, field := range []string{"limit", "remaining", "reset"} {
			header.Del("x-omniswitch-" + field + "-usd")
		}
	}
}

func restrictive(current, candidate *Snapshot) *Snapshot {
	if current == nil || candidate.Remaining < current.Remaining ||
		(candidate.Remaining == current.Remaining && candidate.Limit < current.Limit) ||
		(candidate.Remaining == current.Remaining && candidate.Limit == current.Limit && candidate.ResetsInSeconds > current.ResetsInSeconds) {
		return candidate
	}
	return current
}

func setUnitHeaders(header http.Header, prefix, unit string, entry *Snapshot) {
	limit, remaining, reset := "0", "0", "0s"
	if entry != nil {
		limit = strconv.FormatFloat(entry.Limit, 'f', -1, 64)
		remaining = strconv.FormatFloat(entry.Remaining, 'f', -1, 64)
		reset = ResetValue(entry.ResetsInSeconds)
	}
	header.Set(prefix+"limit-"+unit, limit)
	header.Set(prefix+"remaining-"+unit, remaining)
	header.Set(prefix+"reset-"+unit, reset)
}

func ResetValue(seconds float64) string {
	return (time.Duration(math.Ceil(max(0, seconds)*1000)) * time.Millisecond).String()
}

func RetryAfter(duration time.Duration) string {
	return strconv.FormatInt(max(1, int64(math.Ceil(duration.Seconds()))), 10)
}
