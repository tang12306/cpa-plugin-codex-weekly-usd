package main

// The usage endpoint is the cheap way to read a credential's quota.
//
// The Codex clients poll it for their status line, and the proxy's own
// management page reads it for its quota view - including straight after
// spending a rate-limit reset. It is a plain GET: no model is called and no
// quota is spent, and it reports both windows even for a credential that is
// out, where a model request is refused with a 429.
//
// It is not a published API, so the model probe stays behind it as the
// rotator's fallback. The operator's refresh has no fallback on purpose: a page
// load must never turn into a model request per credential.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	usageURL = "https://chatgpt.com/backend-api/wham/usage"
	// usageBodyLimit bounds what is read of the answer. The document is a few
	// kilobytes; anything far larger is not the document.
	usageBodyLimit = 256 << 10
)

// usageDoc is the part of the usage endpoint's answer this plugin reads.
type usageDoc struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Primary   *usageWindow `json:"primary_window"`
		Secondary *usageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	Credits *struct {
		HasCredits bool            `json:"has_credits"`
		Unlimited  bool            `json:"unlimited"`
		Balance    json.RawMessage `json:"balance"`
	} `json:"credits"`
	// Seen as {"type": ..., "details": ...}; a bare string is accepted too,
	// since that is how the same fact arrives in a response header.
	ReachedType json.RawMessage `json:"rate_limit_reached_type"`
}

type usageWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfter    int64   `json:"reset_after_seconds"`
	ResetAt       int64   `json:"reset_at"`
}

// readUsage asks the usage endpoint about one credential.
//
// answered is false when the endpoint gave nothing this plugin can use - it is
// missing, failing, or has changed shape - which is the caller's cue to try
// another way or to say so. A refusal and a transport failure both count as
// answers: the first is the credential's standing, and the second would recur
// on any other request sent through the same egress.
func readUsage(token, account, proxyURL, url string, now time.Time) (res probeResult, answered bool) {
	headers := map[string][]string{
		"authorization": {"Bearer " + token},
		"accept":        {"application/json"},
		"originator":    {"codex_cli_rs"},
		"user-agent":    {probeUserAgent},
	}
	if account != "" {
		headers["chatgpt-account-id"] = []string{account}
	}
	status, _, body, err := probeDo(proxyURL, "GET", url, headers, nil, usageBodyLimit)
	if err != nil {
		res.Error = err.Error()
		return res, true
	}
	res.StatusCode = status
	switch {
	case status == 401 || status == 403:
		return res, true
	case status/100 != 2:
		res.Error = fmt.Sprintf("usage endpoint answered HTTP %d", status)
		return res, false
	}
	rl, ok := parseUsage(body, now)
	if !ok {
		res.Error = "usage endpoint returned no quota windows"
		return res, false
	}
	res.Windows, res.PlanType, res.Credits = rl.Windows, rl.PlanType, rl.Credits
	return res, true
}

// parseUsage turns the usage document into the reading a response's quota
// headers would have produced, so both arrive on one code path.
func parseUsage(body []byte, now time.Time) (rateLimit, bool) {
	var doc usageDoc
	if err := json.Unmarshal(body, &doc); err != nil || doc.RateLimit == nil {
		return rateLimit{}, false
	}
	var rl rateLimit
	for _, w := range []*usageWindow{doc.RateLimit.Primary, doc.RateLimit.Secondary} {
		if w == nil || w.WindowSeconds <= 0 {
			continue
		}
		r := windowReading{
			Minutes:    int(w.WindowSeconds / 60),
			Percent:    clampPercent(w.UsedPercent),
			ResetAt:    w.ResetAt,
			ResetAfter: w.ResetAfter,
		}
		if r.ResetAt <= 0 && r.ResetAfter > 0 {
			r.ResetAt = now.Unix() + r.ResetAfter
		}
		rl.Windows = append(rl.Windows, r)
	}
	if len(rl.Windows) == 0 {
		return rateLimit{}, false
	}
	sort.Slice(rl.Windows, func(i, j int) bool { return rl.Windows[i].Minutes < rl.Windows[j].Minutes })

	rl.Found = true
	rl.PlanType = strings.TrimSpace(doc.PlanType)
	if doc.Credits != nil {
		rl.Credits.HasCredits = doc.Credits.HasCredits
		rl.Credits.Unlimited = doc.Credits.Unlimited
		rl.Credits.Balance = jsonScalar(doc.Credits.Balance)
	}
	rl.Credits.LimitReached = reachedType(doc.ReachedType)
	return rl, true
}

// reachedType names the limit a credential has hit, or nothing when it has hit
// none.
func reachedType(raw json.RawMessage) string {
	if s := jsonScalar(raw); s != "" {
		return s
	}
	var obj struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.Type)
	}
	return ""
}

// jsonScalar renders a JSON string or number as text; null and anything else
// read as empty.
func jsonScalar(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}
