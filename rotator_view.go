package main

import (
	"sort"
	"strconv"
	"time"
)

// The rotator needs three things the accounting side already knows: what the
// last reading of a credential was, how fast quota is currently going, and what
// a percentage point of a given credential is worth. They live here rather than
// in rotator.go because they are all reads of the accounting state.

// burnWindow is the sample horizon for the burn rate. Long enough to survive a
// quiet minute, short enough to notice a burst: a credential seen today went
// from 84% to 99% in forty-five minutes.
const burnWindow = time.Hour

// accountFor resolves an auth entry to the accumulator that has been watching
// it. Callers must hold no lock; this takes the state lock itself.
func (a *App) accountFor(e authEntry) *Account {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, acct := range a.accounts {
		if acct == nil {
			continue
		}
		switch {
		case e.AuthIndex != "" && acct.AuthIndex == e.AuthIndex,
			e.ID != "" && acct.AuthID == e.ID,
			e.Name != "" && acct.AuthID == e.Name:
			return acct
		}
	}
	return nil
}

// observedWindows reports what live traffic last saw, and when. A credential
// that is serving needs no probe: every response it produces carries a reading.
func (a *App) observedWindows(e authEntry) ([]windowReading, int64) {
	acct := a.accountFor(e)
	if acct == nil {
		return nil, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]windowReading, 0, len(acct.Windows))
	newest := int64(0)
	for key, w := range acct.Windows {
		if w == nil {
			continue
		}
		minutes := w.Minutes
		if minutes == 0 {
			if n, err := strconv.Atoi(key); err == nil {
				minutes = n
			}
		}
		out = append(out, windowReading{Minutes: minutes, Percent: w.Percent, ResetAt: w.ResetAt})
		if w.ObservedAt > newest {
			newest = w.ObservedAt
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Minutes < out[j].Minutes })
	return out, newest
}

// trafficRejection reports whether live traffic has seen upstream refuse this
// credential outright, and has not seen it work since.
//
// This is the cheapest evidence there is - it costs nothing, because the
// requests were being made anyway - and it is the only evidence that a
// credential is dead which does not require asking. Without it a revoked
// credential keeps being ranked on its last healthy quota reading, which is
// exactly what happened: one refused at 07:19 was still being reported at 9%
// headroom a day later.
func (a *App) trafficRejection(e authEntry) (bool, string, int64) {
	acct := a.accountFor(e)
	if acct == nil {
		return false, "", 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	rejected, reason, at := false, "", int64(0)
	for _, h := range acct.Models {
		if h == nil || !h.Rejected {
			continue
		}
		if h.RejectedAt > at {
			rejected, reason, at = true, h.Reason, h.RejectedAt
		}
	}
	return rejected, reason, at
}

// observeWindows folds a probe's reading into the accounting state, exactly as
// a served request's headers would be.
//
// Without it a probe only ever reaches the rotator's own view: the panel keeps
// showing whatever percentage live traffic last saw, and a window the clock
// said had rolled over stays marked inferred for good, because nothing ever
// confirms it. That was the whole point of re-reading it.
//
// Movement with no spend of ours behind it is recorded as unexplained, which is
// the truth: from this plugin's side the credential was idle, so anything that
// moved was spent by something else.
func (a *App) observeWindows(e authEntry, windows []windowReading, now time.Time) {
	if len(windows) == 0 {
		return
	}
	acct := a.accountFor(e)
	if acct == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range windows {
		if r.Minutes <= 0 {
			continue
		}
		acct.window(r.Minutes).advance(r, now)
	}
	a.dirty = true
}

// burnRates measures percentage points consumed per hour, per window length,
// from the calibration samples of the last hour.
//
// The rate has to be per window: a five-hour window and a weekly one move at
// completely different speeds against the same traffic, and pairing one
// window's headroom with another's rate produces a projection that is wrong by
// more than an order of magnitude.
func (a *App) burnRates(e authEntry) map[int]float64 {
	acct := a.accountFor(e)
	if acct == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	cutoff := time.Now().Add(-burnWindow).Unix()
	rates := make(map[int]float64, len(acct.Windows))
	for key, w := range acct.Windows {
		if w == nil {
			continue
		}
		minutes := w.Minutes
		if minutes == 0 {
			if n, err := strconv.Atoi(key); err == nil {
				minutes = n
			}
		}
		var moved float64
		var oldest int64
		for _, s := range w.Samples {
			if s.TS < cutoff {
				continue
			}
			moved += s.DP
			if oldest == 0 || s.TS < oldest {
				oldest = s.TS
			}
		}
		if moved <= 0 || oldest == 0 {
			continue
		}
		// Measure over the span actually covered by samples, not the whole
		// hour: a burst that started ten minutes ago is a ten-minute rate, and
		// dividing it by an hour would hide exactly the case worth catching.
		hours := time.Since(time.Unix(oldest, 0)).Hours()
		if hours < 1.0/60 {
			hours = 1.0 / 60
		}
		rates[minutes] = moved / hours
	}
	return rates
}

// fleetUSDPerHour is how fast the whole fleet is spending right now, used to
// turn a credential's remaining dollars into hours of service.
func (a *App) fleetUSDPerHour() float64 {
	a.mu.Lock()
	accounts := make([]*Account, 0, len(a.accounts))
	for _, acct := range a.accounts {
		accounts = append(accounts, acct)
	}
	now := time.Now()
	nowHour := now.Unix() / 3600
	// The hour in progress counts for the fraction of it that has elapsed.
	// Excluding it would make this zero for the first hour after a restart,
	// which is exactly when a rotation decision is most likely to be needed.
	partial := float64(now.Unix()%3600) / 3600
	if partial < 1.0/60 {
		partial = 1.0 / 60
	}

	var total, hours float64
	for h := nowHour - 2; h <= nowHour; h++ {
		key := strconv.FormatInt(h, 10)
		sum, found := 0.0, false
		for _, acct := range accounts {
			if acct == nil {
				continue
			}
			if bucket := acct.Hours[key]; bucket != nil {
				sum += bucket.USD
				found = true
			}
		}
		if !found {
			continue
		}
		total += sum
		if h == nowHour {
			hours += partial
		} else {
			hours++
		}
	}
	a.mu.Unlock()

	if hours <= 0 || total <= 0 {
		return 0
	}
	return total / hours
}

// quotaUSDFor is what this credential's window is worth in dollars. It is the
// figure that makes credentials comparable at all: a Plus account at 100% can
// be worth less than a Team account at 40%, and ranking on percentages alone
// would pick the wrong one.
func (a *App) quotaUSDFor(e authEntry, minutes int) float64 {
	acct := a.accountFor(e)
	if acct == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	// Priced in the model this credential is actually serving where that is
	// known. A window's quota is one pool, but a dollar of an expensive model
	// empties it faster - answering with the cheap model's figure is how a
	// credential came to look like it could serve for hours longer than it
	// could, the moment traffic moved to a costlier model.
	weights := a.deriveModelWeights()
	if minutes > 0 {
		if w := acct.Windows[strconv.Itoa(minutes)]; w != nil {
			if est := w.EstimateWith(weights); est.QuotaUSD > 0 {
				return est.QuotaUSD
			}
		}
	}
	// No estimate for that window - fall back to the largest one this
	// credential has, which is better than treating it as worthless.
	best := 0.0
	for _, w := range acct.Windows {
		if w == nil {
			continue
		}
		if est := w.EstimateWith(weights); est.QuotaUSD > best {
			best = est.QuotaUSD
		}
	}
	return best
}

// clearRejection retires a recorded refusal because a later probe was served.
// recordHealth clears one the same way when a request succeeds: upstream
// accepting the credential is the proof, and it makes no difference whether
// the request carrying that proof came from a caller or from this plugin.
//
// Without this the two halves deadlock. A 401 is recorded against the account,
// retireRejected disables the credential on the strength of it, and derive goes
// on reporting the refusal - so rank writes the credential off as a dead token,
// the rotator never promotes it, and no request ever reaches it. The only event
// that could clear the flag is the traffic the flag itself is preventing, so a
// re-authorised credential stays dead on the panel forever. One did, for a day,
// while its own probes were coming back 200 the whole time.
func (a *App) clearRejection(e authEntry, now time.Time) bool {
	acct := a.accountFor(e)
	if acct == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	cleared := false
	for _, h := range acct.Models {
		if h == nil || !h.Rejected {
			continue
		}
		// Only the refusal is lifted. LastOK stays where it was because no
		// model call has succeeded - a probe reads quota, it does not serve -
		// and overwriting it would claim service this credential has not given.
		h.Rejected, h.RejectedAt, h.Reason = false, 0, ""
		cleared = true
	}
	if cleared {
		a.dirty = true
	}
	return cleared
}
