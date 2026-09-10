package main

import (
	"math"
	"sort"
	"time"
)

// CLIProxyAPI cools a credential down per model, not as a whole: upstream keeps
// a separate allowance for each model, so one credential can happily serve
// gpt-5.6-sol while it is locked out of gpt-6-astra. A model goes dark when
// every credential that serves it is cooling at the same time, and from the
// outside that looks nothing like a credential outage - the auth list still
// reports every file as active and every other model keeps working.
//
// This file tracks the standing of each (credential, model) pair so the panel
// can answer "why is this one model failing, and when does it come back"
// without anyone reading the proxy's logs.

const (
	// fullPercent is where a window counts as exhausted. Upstream reports whole
	// numbers, so anything from 99.5 up is a full window.
	fullPercent = 99.5
	// singlePointMinRequests keeps the single-credential warning off models
	// that have only ever been touched once or twice.
	singlePointMinRequests = 20
)

// Model availability states, both per credential and rolled up per model.
const (
	stateOK         = "ok"
	stateCooling    = "cooling"
	stateRecovering = "recovering"
	stateDisabled   = "disabled"
	// stateRejected is upstream refusing the credential itself. It is not a
	// cooldown and does not end on its own: only a fresh login clears it.
	stateRejected = "rejected"
	stateDegraded = "degraded"
	stateDown     = "down"
)

// ModelHealth is one credential's standing on one model.
type ModelHealth struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Failed   int64  `json:"failed"`

	LastSeen int64 `json:"last_seen"`
	LastOK   int64 `json:"last_ok,omitempty"`
	LastFail int64 `json:"last_fail,omitempty"`

	// The rest is set together when a request is refused for quota reasons.
	Reason        string `json:"reason,omitempty"`
	BlockedWindow int    `json:"blocked_window_minutes,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	// Estimated marks a deadline inferred from the soonest window reset rather
	// than read off a window that was actually full: credits can run out and
	// refuse the request while every percentage still looks fine.
	Estimated bool  `json:"cooldown_estimated,omitempty"`
	Blocks    int64 `json:"blocks,omitempty"`

	// Rejected records that upstream refused the credential itself - 401 or
	// 403 - rather than refusing one request. It is kept apart from the quota
	// fields because it means something completely different: a cooldown ends
	// on its own, a rejection only ends when someone logs the account back in.
	Rejected   bool  `json:"rejected,omitempty"`
	RejectedAt int64 `json:"rejected_at,omitempty"`
}

// state is derived rather than stored, so a deadline that has quietly passed
// stops reading as a live lockout.
func (h *ModelHealth) state(now time.Time) string {
	switch {
	case h.Rejected:
		// Cleared by the success branch of recordHealth, not by comparing
		// timestamps: at one-second resolution a success and a rejection in the
		// same second are indistinguishable, and the comparison then reads the
		// rejection as already answered. A credential upstream will not accept
		// has no availability to describe, whatever its last quota reading said.
		return stateRejected
	case h.CooldownUntil > now.Unix():
		return stateCooling
	case h.CooldownUntil > 0 && h.LastFail > h.LastOK:
		// The deadline has passed but nothing has succeeded since, so the
		// credential is expected back rather than known to be back.
		return stateRecovering
	default:
		return stateOK
	}
}

// recordHealth folds one request into the (credential, model) ledger.
func (acct *Account) recordHealth(model string, rec usageRecord, rl rateLimit, now time.Time, keepDays int) {
	if model == "" {
		return
	}
	if acct.Models == nil {
		acct.Models = map[string]*ModelHealth{}
	}
	h := acct.Models[model]
	if h == nil {
		h = &ModelHealth{Model: model}
		acct.Models[model] = h
	}
	defer acct.pruneHealth(now, keepDays)

	h.LastSeen = now.Unix()
	h.Requests++

	if !rec.Failed {
		h.LastOK = now.Unix()
		// A served request proves the lockout is over, whatever the recorded
		// deadline said. Upstream hands quota back early often enough that
		// trusting the deadline alone would leave a working credential shown as
		// cooling for hours.
		h.CooldownUntil, h.Reason, h.BlockedWindow, h.Estimated = 0, "", 0, false
		h.Rejected, h.RejectedAt = false, 0
		return
	}

	h.Failed++

	// An authentication failure is not an ordinary error and must not be
	// forgotten. 401 and 403 mean upstream has stopped accepting the credential
	// itself, which is exactly the failure no quota reading can describe - a
	// refused request carries no quota headers, so the branch below would drop
	// it. That is how a revoked credential went on being reported at its last
	// healthy percentage for a day.
	if rec.Failure.rejected() {
		h.LastFail = now.Unix()
		h.Rejected, h.RejectedAt = true, now.Unix()
		h.Reason = rec.Failure.reason()
		h.CooldownUntil, h.BlockedWindow, h.Estimated = 0, 0, false
		return
	}

	deadline, minutes, estimated, ok := cooldownFrom(rl)
	if !ok {
		// Any other failure carrying no quota signal is an ordinary error - a
		// bad request, a dropped connection - and says nothing about
		// availability.
		return
	}
	if h.CooldownUntil <= now.Unix() {
		h.Blocks++
	}
	h.LastFail = now.Unix()
	h.CooldownUntil = deadline
	h.BlockedWindow = minutes
	h.Estimated = estimated
	h.Reason = rl.Credits.LimitReached
}

// cooldownFrom works out how long a refused request locks this model out. The
// proxy cools down until the reset upstream reported, so the deadline belongs
// to whichever window is actually full - the later one when both are - and is
// not simply the next reset to come round.
func cooldownFrom(rl rateLimit) (deadline int64, minutes int, estimated bool, ok bool) {
	if !rl.Found {
		return 0, 0, false, false
	}
	for _, w := range rl.Windows {
		if w.Percent >= fullPercent && w.ResetAt > deadline {
			deadline, minutes = w.ResetAt, w.Minutes
		}
	}
	if deadline > 0 {
		return deadline, minutes, false, true
	}
	if rl.Credits.LimitReached == "" {
		return 0, 0, false, false
	}
	// Credits ran out while every percentage still reads under the limit, so
	// there is no full window to take a deadline from. The soonest reset is the
	// earliest moment the situation can change; it is flagged as a guess.
	for _, w := range rl.Windows {
		if w.ResetAt > 0 && (deadline == 0 || w.ResetAt < deadline) {
			deadline, minutes = w.ResetAt, w.Minutes
		}
	}
	if deadline == 0 {
		return 0, 0, false, false
	}
	return deadline, minutes, true, true
}

func (acct *Account) pruneHealth(now time.Time, keepDays int) {
	if keepDays <= 0 || len(acct.Models) == 0 {
		return
	}
	cutoff := now.AddDate(0, 0, -keepDays).Unix()
	for name, h := range acct.Models {
		if h == nil || h.LastSeen < cutoff {
			delete(acct.Models, name)
		}
	}
}

// modelBucket accumulates one model across every credential.
type modelBucket struct {
	creds                                           []map[string]any
	total, available, cooling, recovering, disabled int
	requests, failed                                int64
	nextBack                                        int64
	soleCredential                                  string
}

// modelHealth rolls the per-credential records up per model: how many
// credentials can serve it right now, which ones are locked out, and when the
// first one is due back. Disabled credentials are listed but never counted as
// capacity - eight of nine disabled is exactly the situation that turns one
// credential's 429 into a dead model.
//
// Every live credential counts towards every model. This used to be built
// from the per-model ledger, which only has an entry for a model once a
// credential has served it - so a credential that had only ever run astra was
// not counted as sol capacity, and the panel warned "only one credential can
// serve sol" while two enabled accounts could. Any Codex account serves any
// model; history says what a credential has done, not what it can do.
//
// What is NOT carried across models is a quota lockout. The used-percentage is
// one meter shared by every model on the account - it runs straight across
// back-to-back model switches - but enforcement is per model: on this fleet,
// sol has been refused at 100% and terra or luna served on the same account,
// at the same 100%, seconds to minutes later. A lockout on one model therefore
// says nothing reliable about another, and inventing one would hide capacity
// that is really there. A refusal of the credential itself (401/403) is
// different: it is about the token, and every model uses the same token.
func modelHealth(entries []authEntry, accounts []*Account, now time.Time) ([]map[string]any, []map[string]any) {
	buckets := map[string]*modelBucket{}

	// The credentials to report on. With no auth list - a failure to ask, not
	// evidence that nothing exists - fall back to whatever has accounting.
	type cred struct {
		entry    authEntry
		known    bool
		acct     *Account
		disabled bool
		name     string
		refused  *ModelHealth
	}
	creds := make([]cred, 0, len(entries))
	if len(entries) > 0 {
		for _, e := range entries {
			if e.Type != "" && e.Type != "codex" {
				continue
			}
			c := cred{entry: e, known: true, disabled: e.Disabled,
				name: firstNonEmpty(e.Label, e.Email, e.Name, e.ID)}
			for _, acct := range accounts {
				if acct == nil {
					continue
				}
				if _, ok := lookupAuth([]authEntry{e}, acct); ok {
					c.acct = acct
					c.name = displayName(entries, acct)
					break
				}
			}
			creds = append(creds, c)
		}
	} else {
		for _, acct := range accounts {
			if acct != nil {
				creds = append(creds, cred{acct: acct, name: displayName(entries, acct)})
			}
		}
	}

	// Every model the fleet has served is a model every credential can serve.
	models := map[string]bool{}
	for i := range creds {
		c := &creds[i]
		if c.acct == nil {
			continue
		}
		for model, h := range c.acct.Models {
			if h == nil {
				continue
			}
			models[model] = true
			if h.Rejected && (c.refused == nil || h.RejectedAt > c.refused.RejectedAt) {
				c.refused = h
			}
		}
	}

	for model := range models {
		b := &modelBucket{}
		buckets[model] = b
		for _, c := range creds {
			var h *ModelHealth
			if c.acct != nil {
				h = c.acct.Models[model]
			}
			state := stateOK
			untested := h == nil
			switch {
			case c.disabled:
				state = stateDisabled
			case c.refused != nil:
				state = stateRejected
			case h != nil:
				state = h.state(now)
			}

			row := map[string]any{
				"credential": c.name,
				"state":      state,
				"requests":   int64(0),
				"failed":     int64(0),
				"blocks":     int64(0),
			}
			if c.acct != nil {
				row["auth_id"] = c.acct.AuthID
			} else {
				row["auth_id"] = firstNonEmpty(c.entry.Name, c.entry.ID)
			}
			if untested {
				// Capacity on the strength of the account, not of anything
				// this credential has shown on this model. Worth saying so.
				row["untested"] = true
			}
			if h != nil {
				row["requests"], row["failed"], row["blocks"] = h.Requests, h.Failed, h.Blocks
				if h.LastSeen > 0 {
					row["last_seen_age_seconds"] = ageSeconds(h.LastSeen, now)
				}
				if h.LastOK > 0 {
					row["last_ok_age_seconds"] = ageSeconds(h.LastOK, now)
				}
				if h.CooldownUntil > 0 && state != stateOK && state != stateDisabled && state != stateRejected {
					until := time.Unix(h.CooldownUntil, 0)
					row["cooldown_until"] = until.UTC().Format(time.RFC3339)
					row["cooldown_in_seconds"] = int64(math.Max(0, time.Until(until).Seconds()))
					row["cooldown_estimated"] = h.Estimated
					if h.Reason != "" {
						row["reason"] = h.Reason
					}
					if h.BlockedWindow > 0 {
						row["blocked_window"] = windowLabel(h.BlockedWindow)
					}
				}
				b.requests += h.Requests
				b.failed += h.Failed
			}
			if state == stateRejected {
				row["reason"] = firstNonEmpty(c.refused.Reason, "unauthorized")
				if c.refused.RejectedAt > 0 {
					row["rejected_age_seconds"] = ageSeconds(c.refused.RejectedAt, now)
				}
				if h == nil || !h.Rejected {
					// Refused on another model; the token is the same one.
					row["refused_on"] = c.refused.Model
				}
			}
			b.creds = append(b.creds, row)

			if c.disabled {
				b.disabled++
				continue
			}
			b.total++
			b.soleCredential = c.name
			switch state {
			case stateCooling:
				b.cooling++
				if b.nextBack == 0 || h.CooldownUntil < b.nextBack {
					b.nextBack = h.CooldownUntil
				}
			case stateRecovering:
				b.recovering++
			case stateRejected:
				// Counted as capacity it is not: a refused token serves nothing.
			default:
				b.available++
			}
		}
	}

	names := make([]string, 0, len(buckets))
	for model := range buckets {
		names = append(names, model)
	}
	sort.Strings(names)

	rows := make([]map[string]any, 0, len(names))
	warnings := make([]map[string]any, 0)

	for _, model := range names {
		b := buckets[model]
		sort.SliceStable(b.creds, func(i, j int) bool {
			return credRank(b.creds[i]) < credRank(b.creds[j])
		})

		state := stateOK
		switch {
		case b.total > 0 && b.available == 0:
			state = stateDown
		case b.cooling+b.recovering > 0:
			state = stateDegraded
		}

		row := map[string]any{
			"model":         model,
			"state":         state,
			"credentials":   b.total,
			"available":     b.available,
			"cooling":       b.cooling,
			"recovering":    b.recovering,
			"disabled":      b.disabled,
			"requests":      b.requests,
			"failed":        b.failed,
			"single_point":  b.total == 1,
			"by_credential": b.creds,
		}
		if b.nextBack > 0 {
			back := time.Unix(b.nextBack, 0)
			row["next_recovery_at"] = back.UTC().Format(time.RFC3339)
			row["next_recovery_in_seconds"] = int64(math.Max(0, time.Until(back).Seconds()))
		}
		rows = append(rows, row)

		if state == stateDown {
			warn := map[string]any{"code": "model_unavailable", "model": model, "credentials": b.total}
			if b.nextBack > 0 {
				warn["in_seconds"] = row["next_recovery_in_seconds"]
			}
			warnings = append(warnings, warn)
		}
		// One credential for a model is a standing outage waiting to happen:
		// its next 429 takes the whole model down, and nothing in the proxy's
		// own status will say so beforehand.
		if b.total == 1 && b.requests >= singlePointMinRequests {
			warnings = append(warnings, map[string]any{
				"code":       "model_single_point",
				"model":      model,
				"credential": b.soleCredential,
				"disabled":   b.disabled,
			})
		}
	}

	// Broken models first, then the busiest: the panel's top row should be
	// whatever is actually wrong.
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := modelRank(rows[i]), modelRank(rows[j])
		if ri != rj {
			return ri < rj
		}
		return rows[i]["requests"].(int64) > rows[j]["requests"].(int64)
	})
	return rows, warnings
}

func credRank(row map[string]any) int {
	switch row["state"] {
	case stateCooling:
		return 0
	case stateRecovering:
		return 1
	case stateOK:
		return 2
	default:
		return 3
	}
}

func modelRank(row map[string]any) int {
	switch row["state"] {
	case stateDown:
		return 0
	case stateDegraded:
		return 1
	default:
		if sole, _ := row["single_point"].(bool); sole {
			return 2
		}
		return 3
	}
}

func ageSeconds(stamp int64, now time.Time) int64 {
	return int64(math.Max(0, now.Sub(time.Unix(stamp, 0)).Seconds()))
}
