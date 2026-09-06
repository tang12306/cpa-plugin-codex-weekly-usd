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

	if minutes > 0 {
		if w := acct.Windows[strconv.Itoa(minutes)]; w != nil {
			if est := w.Estimate(); est.QuotaUSD > 0 {
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
		if est := w.Estimate(); est.QuotaUSD > best {
			best = est.QuotaUSD
		}
	}
	return best
}
