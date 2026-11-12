package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// heldSpend is traffic served on a model the price book could not price at the
// time, kept as tokens so that it can be billed once a price turns up.
//
// A model is in traffic the day it launches and in the catalog some hours
// later. Billing those first requests at nothing and moving on left every
// figure that covered them short for good, and the "no public price" warning
// standing long after the catalog had one. Requests over the long-context
// threshold are held apart, because the surcharge applies per request and a sum
// of tokens cannot say which requests crossed it.
type heldSpend struct {
	Requests int64   `json:"requests"`
	Tokens   Tokens  `json:"tokens"`
	Long     *Tokens `json:"long,omitempty"`
}

func (h *heldSpend) add(d Tokens, long bool) {
	h.Requests++
	if !long {
		h.Tokens.add(d)
		return
	}
	if h.Long == nil {
		h.Long = &Tokens{}
	}
	h.Long.add(d)
}

// bill prices the held tokens as the requests would have been priced had the
// rate been known then: what they cost, and what caching saved.
func (h *heldSpend) bill(p Price, longMultiplier float64) (cost, saved float64) {
	cost = p.Cost(h.Tokens, 0, 0)
	cacheRead := h.Tokens.CacheRead
	if h.Long != nil {
		long := p.Cost(*h.Long, 0, 0)
		if longMultiplier > 0 {
			long *= longMultiplier
		}
		cost += long
		cacheRead += h.Long.CacheRead
	}
	if p.CacheRead > 0 && p.CacheRead < p.Input {
		saved = float64(cacheRead) * (p.Input - p.CacheRead) / 1e6
	}
	return cost, saved
}

// holdIn adds one request to model's held spend in m, creating either as needed.
func holdIn(m map[string]*heldSpend, model string, d Tokens, long bool) map[string]*heldSpend {
	if m == nil {
		m = map[string]*heldSpend{}
	}
	h := m[model]
	if h == nil {
		h = &heldSpend{}
		m[model] = h
	}
	h.add(d, long)
	return m
}

// unpricedNow is what this credential is still short of a price for: the
// models, and how many requests. Read off the current windows and the hourly
// series, so it clears itself the moment the spend is billed. The same request
// is in every window and in one hour, so the counts are compared, not added.
func (acct *Account) unpricedNow() ([]string, int64) {
	models := map[string]bool{}
	var most int64
	for _, w := range acct.Windows {
		if w == nil {
			continue
		}
		var n int64
		for m, agg := range w.ByModel {
			if agg != nil && agg.Unpriced > 0 {
				models[m] = true
				n += agg.Unpriced
			}
		}
		most = max(most, n)
	}
	var hours int64
	for _, b := range acct.Hours {
		if b == nil {
			continue
		}
		for m, h := range b.Held {
			models[m] = true
			hours += h.Requests
		}
	}
	return sortedKeys(models), max(most, hours)
}

// holdsUnpriced reports whether any credential holds spend that a price would
// bill. A count kept by an older build with no tokens behind it does not
// count: no price can do anything for it.
func (a *App) holdsUnpriced() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, acct := range a.accounts {
		if acct == nil {
			continue
		}
		for _, w := range acct.Windows {
			if w == nil {
				continue
			}
			if len(w.PendingHeld) > 0 || len(w.Held) > 0 {
				return true
			}
			for _, agg := range w.ByModel {
				if agg != nil && agg.Held != nil {
					return true
				}
			}
		}
		for _, b := range acct.Hours {
			if b != nil && len(b.Held) > 0 {
				return true
			}
		}
	}
	return false
}

// repriceHeld bills the spend held for want of a price wherever the price book
// now has one, into every ledger that billing it at the time would have
// reached. It runs after each catalog fetch, on a config change and at start.
func (a *App) repriceHeld() {
	a.mu.Lock()
	mult := a.cfg.LongContextMultiplier
	now := time.Now()
	models := map[string]bool{}
	var requests int64
	for _, acct := range a.accounts {
		if acct != nil {
			requests += acct.reprice(a.prices, mult, models, now)
		}
	}
	if len(models) > 0 {
		a.dirty = true
	}
	a.mu.Unlock()
	if len(models) > 0 {
		hostLog("info", fmt.Sprintf("billed spend held until a price was found: %s (%d request(s))",
			strings.Join(sortedKeys(models), ", "), requests))
	}
}

// reprice bills this credential's held spend on each model the price book can
// price, and reports how many requests that was. models collects the models
// billed.
//
// Each ledger gets what a priced request adds to it in HandleUsage: the
// cycle's totals and the model's row, pending spend not yet matched to a
// percentage step, the calibration step it was matched to, and the hour it was
// served in - which alone carries the lifetime total, because a request is in
// one hour but in every window.
func (acct *Account) reprice(pb *priceBook, mult float64, models map[string]bool, now time.Time) int64 {
	var requests int64
	for _, w := range acct.Windows {
		if w == nil {
			continue
		}
		for model, agg := range w.ByModel {
			if agg == nil || agg.Held == nil {
				continue
			}
			p, ok := pb.Lookup(model)
			if !ok {
				continue
			}
			cost, saved := agg.Held.bill(p, mult)
			agg.USD += cost
			agg.SavedUSD += saved
			w.USD += cost
			w.SavedUSD += saved
			agg.Unpriced = max(0, agg.Unpriced-agg.Held.Requests)
			agg.Held = nil
			models[model] = true
		}
		for model, h := range w.PendingHeld {
			p, ok := pb.Lookup(model)
			if !ok {
				continue
			}
			cost, _ := h.bill(p, mult)
			w.PendingUSD += cost
			if cost != 0 {
				if w.PendingByModel == nil {
					w.PendingByModel = map[string]float64{}
				}
				w.PendingByModel[model] += cost
			}
			delete(w.PendingHeld, model)
			models[model] = true
		}
		if len(w.PendingHeld) == 0 {
			w.PendingHeld = nil
		}
		w.releaseHeld(pb, mult, models, now)
	}
	for _, b := range acct.Hours {
		if b == nil {
			continue
		}
		for model, h := range b.Held {
			p, ok := pb.Lookup(model)
			if !ok {
				continue
			}
			cost, _ := h.bill(p, mult)
			b.USD += cost
			acct.TotalUSD += cost
			requests += h.Requests
			delete(b.Held, model)
			models[model] = true
		}
		if len(b.Held) == 0 {
			b.Held = nil
		}
	}
	return requests
}

// releaseHeld bills the held samples and moves each one the price book can now
// price in full into the calibration, in time order, where it would have been
// all along. One still short of a price for any model stays held.
func (w *Window) releaseHeld(pb *priceBook, mult float64, models map[string]bool, now time.Time) {
	if len(w.Held) == 0 {
		return
	}
	kept := w.Held[:0]
	released := false
	for _, s := range w.Held {
		for model, h := range s.Held {
			p, ok := pb.Lookup(model)
			if !ok {
				continue
			}
			cost, _ := h.bill(p, mult)
			s.USD += cost
			if cost != 0 {
				if s.ByModel == nil {
					s.ByModel = map[string]float64{}
				}
				s.ByModel[model] += cost
			}
			delete(s.Held, model)
			models[model] = true
		}
		if len(s.Held) > 0 {
			kept = append(kept, s)
			continue
		}
		s.Held = nil
		// A price of nothing leaves nothing to divide the step by.
		if s.USD > 0 {
			w.Samples = append(w.Samples, s)
			released = true
		}
	}
	w.Held = kept
	if released {
		sort.SliceStable(w.Samples, func(i, j int) bool { return w.Samples[i].TS < w.Samples[j].TS })
	}
	w.pruneSamples(now)
}

// heldThroughFile holds the time of the last save by a build that keeps held
// spend. Every request logged up to then is in that save, billed or held; one
// logged later, if an older build wrote the state since, is only a count.
const heldThroughFile = "held-through"

// recoverHeld rebuilds the held ledgers for a state written before they existed.
//
// Those builds billed a request on a model with no price at nothing and kept
// only the count, so a model that launched before the catalog listed it read
// "no public price" for the rest of the window - long after the catalog had
// one - and its first requests were never counted. The event log has each of
// them with its tokens, and with the reading it arrived with, which says
// whether the meter has moved since - whether it is still pending.
//
// Only models a window still counts as unpriced are looked for, and each
// window takes no more requests than its count. The log says "unpriced" about
// a request for good, billed or not, so it is read only from where the last
// held-aware save left off (heldThroughFile): after a downgrade to an older
// build and back, what came before that was billed the first time round.
//
// Without the log, a model whose every request in the cycle went unpriced is
// still recoverable: all of its tokens in the window are the unpriced ones.
// Runs once, from loadState, before anything else touches the accounts.
func (a *App) recoverHeld() {
	var short []*ModelAgg
	want := map[string]map[string]bool{}
	for key, acct := range a.accounts {
		if acct == nil {
			continue
		}
		for _, w := range acct.Windows {
			if w == nil {
				continue
			}
			for m, agg := range w.ByModel {
				if agg != nil && agg.Unpriced > 0 && agg.Held == nil {
					short = append(short, agg)
					if want[key] == nil {
						want[key] = map[string]bool{}
					}
					want[key][m] = true
				}
			}
		}
	}
	if len(short) == 0 {
		return
	}

	found := a.recoverHeldFromLog(want)

	fallback := 0
	for _, agg := range short {
		if agg.Held == nil && agg.USD == 0 {
			agg.Held = &heldSpend{Requests: agg.Unpriced, Tokens: agg.Tokens}
			fallback++
		}
	}
	if found > 0 || fallback > 0 {
		hostLog("info", fmt.Sprintf("recovered unpriced spend kept only as a count by an older build: "+
			"%d request(s) from the event log, %d model row(s) from their own tokens", found, fallback))
	}
}

// recoverHeldFromLog is recoverHeld's pass over the event log, for the models
// want names per credential. It reports how many logged requests it recovered.
func (a *App) recoverHeldFromLog(want map[string]map[string]bool) int {
	paths, err := filepath.Glob(filepath.Join(a.cfg.DataDir, "events", "*.jsonl"))
	if err != nil || len(paths) == 0 {
		return 0
	}
	sort.Strings(paths)
	// Nothing older than the hourly series can be put anywhere. A day of
	// margin for the file name, which is the date the batch was written.
	cutoff := time.Now().Add(-time.Duration(hourRetention+24) * time.Hour)
	first := cutoff.UTC().Format("2006-01-02")
	threshold := a.cfg.LongContextThreshold
	var through int64
	if raw, errRead := os.ReadFile(filepath.Join(a.cfg.DataDir, heldThroughFile)); errRead == nil {
		through, _ = strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	}

	type event struct {
		TS     string `json:"ts"`
		Auth   string `json:"auth_id"`
		Model  string `json:"model"`
		Priced bool   `json:"priced"`
		Failed bool   `json:"failed"`
		In     int64  `json:"in"`
		Out    int64  `json:"out"`
		Reason int64  `json:"reason"`
		CacheR int64  `json:"cache_r"`
		CacheW int64  `json:"cache_w"`
	}
	found := 0
	for _, path := range paths {
		if strings.TrimSuffix(filepath.Base(path), ".jsonl") < first {
			continue
		}
		fh, errOpen := os.Open(path)
		if errOpen != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.Contains(line, []byte(`"priced":false`)) {
				continue
			}
			var e event
			if json.Unmarshal(line, &e) != nil || e.Priced || e.Failed || !want[e.Auth][e.Model] {
				continue
			}
			acct := a.accounts[e.Auth]
			ts, errTS := time.Parse(time.RFC3339, e.TS)
			if acct == nil || errTS != nil || ts.Unix() <= through {
				continue
			}
			d := Tokens{Input: e.In, Output: e.Out, Reasoning: e.Reason, CacheRead: e.CacheR, CacheWrite: e.CacheW}
			long := threshold > 0 && e.In > threshold
			var raw map[string]json.RawMessage
			_ = json.Unmarshal(line, &raw)
			took := false
			if b := acct.Hours[strconv.FormatInt(ts.Unix()/3600, 10)]; b != nil {
				b.Held = holdIn(b.Held, e.Model, d, long)
				took = true
			}
			for minutes, w := range acct.Windows {
				if w == nil || ts.Unix() < w.Start {
					continue
				}
				agg := w.ByModel[e.Model]
				if agg == nil || agg.Unpriced <= 0 || (agg.Held != nil && agg.Held.Requests >= agg.Unpriced) {
					continue
				}
				if agg.Held == nil {
					agg.Held = &heldSpend{}
				}
				agg.Held.add(d, long)
				// A request is pending while the meter has not moved past the
				// reading it came with: its share is not in the percentage yet,
				// so it must not be in the spend divided by it either.
				var pct float64
				if v, ok := raw["pct_"+minutes]; ok && json.Unmarshal(v, &pct) == nil &&
					w.HasLast && w.LastPercent <= pct+0.001 {
					w.PendingHeld = holdIn(w.PendingHeld, e.Model, d, long)
				}
				took = true
			}
			if took {
				found++
			}
		}
		fh.Close()
	}
	return found
}

// eventPrice prices a logged request as HandleUsage would price it now. The
// ratio replay uses it for requests logged before their model had a price,
// which otherwise drop out and leave their share of the meter to be credited
// to whatever priced spend sits next to them.
func (a *App) eventPrice() func(model string, d Tokens) (float64, bool) {
	a.mu.Lock()
	threshold, mult := a.cfg.LongContextThreshold, a.cfg.LongContextMultiplier
	a.mu.Unlock()
	pb := a.prices
	return func(model string, d Tokens) (float64, bool) {
		p, ok := pb.Lookup(model)
		if !ok {
			return 0, false
		}
		return p.Cost(d, threshold, mult), true
	}
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
