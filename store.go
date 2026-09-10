package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tokens mirrors pluginapi.UsageDetail. The host marshals that struct without
// json tags, so the wire names are the Go field names.
type Tokens struct {
	Input     int64 `json:"InputTokens"`
	Output    int64 `json:"OutputTokens"`
	Reasoning int64 `json:"ReasoningTokens"`
	Cached    int64 `json:"CachedTokens"`
	CacheRead int64 `json:"CacheReadTokens"`
	// CacheWrite is CacheCreationTokens on the wire.
	CacheWrite int64 `json:"CacheCreationTokens"`
	Total      int64 `json:"TotalTokens"`
}

func (t *Tokens) add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.Reasoning += o.Reasoning
	t.Cached += o.Cached
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.Total += o.Total
}

// usageRecord mirrors the fields of pluginapi.UsageRecord this plugin reads.
type usageRecord struct {
	Provider     string              `json:"Provider"`
	ExecutorType string              `json:"ExecutorType"`
	Model        string              `json:"Model"`
	Alias        string              `json:"Alias"`
	APIKey       string              `json:"APIKey"`
	AuthID       string              `json:"AuthID"`
	AuthIndex    string              `json:"AuthIndex"`
	AuthType     string              `json:"AuthType"`
	RequestedAt  time.Time           `json:"RequestedAt"`
	Failed       bool                `json:"Failed"`
	Failure      usageFailure        `json:"Failure"`
	Detail       Tokens              `json:"Detail"`
	Headers      map[string][]string `json:"ResponseHeaders"`
}

// usageFailure is the host's account of why a request failed.
//
// It has been arriving on every failed request since the ABI gained it and was
// simply not declared here, so the status code was discarded. That mattered:
// 401 and 403 are the one failure a quota reading can never describe, because a
// refused request carries no quota headers at all.
type usageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

// rejected reports an authentication failure - upstream has stopped accepting
// this credential outright, as opposed to refusing this one request.
func (f usageFailure) rejected() bool {
	return f.StatusCode == 401 || f.StatusCode == 403
}

// reason extracts upstream's own name for the refusal, so the panel can say
// "token_revoked" rather than just "401". The body is small and JSON; anything
// unexpected falls back to the status code.
func (f usageFailure) reason() string {
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(f.Body), &parsed) == nil {
		if code := strings.TrimSpace(parsed.Error.Code); code != "" {
			return code
		}
	}
	return fmt.Sprintf("http_%d", f.StatusCode)
}

// windowReading is one quota window as reported on a single upstream response.
//
// Upstream labels its windows "primary" and "secondary", but those labels are
// not stable: before OpenAI reinstated the 5-hour limit, primary was the weekly
// window and secondary was absent; afterwards primary became the 5-hour window
// and the weekly one moved to secondary. Windows are therefore keyed by their
// length, never by the label they arrived under.
type windowReading struct {
	Minutes    int
	Percent    float64
	ResetAt    int64
	ResetAfter int64
}

// creditInfo captures the separate credit balance, which can exhaust and
// produce a 429 while the percentage counters are still well under 100.
type creditInfo struct {
	HasCredits   bool   `json:"has_credits"`
	Unlimited    bool   `json:"unlimited"`
	Balance      string `json:"balance,omitempty"`
	LimitReached string `json:"limit_reached_type,omitempty"`
}

type rateLimit struct {
	Windows     []windowReading
	PlanType    string
	ActiveLimit string
	Credits     creditInfo
	Found       bool
}

// ModelAgg accumulates one model's contribution inside a window.
type ModelAgg struct {
	Requests int64   `json:"requests"`
	Failed   int64   `json:"failed"`
	Tokens   Tokens  `json:"tokens"`
	USD      float64 `json:"usd"`
	SavedUSD float64 `json:"saved_usd"`
	Unpriced int64   `json:"unpriced_requests"`
}

// HourAgg is one hour of the rolling time series. Keeping the series in memory
// and in state.json is far cheaper than re-parsing the event log on every page
// load, and it survives restarts.
type HourAgg struct {
	Requests  int64   `json:"r"`
	Failed    int64   `json:"f"`
	USD       float64 `json:"u"`
	Percent   float64 `json:"p"`
	In        int64   `json:"i"`
	Out       int64   `json:"o"`
	CacheRead int64   `json:"c"`
}

// rollingHours is both the span the fleet chart covers and the trailing window
// its line sums: seven days, matching the window the weekly quota is
// denominated in.
const rollingHours = 168

// hourRetention is deliberately twice the chart span. A point can only carry a
// complete trailing total if a full week of buckets sits behind it, so keeping
// only as much history as the chart shows would leave the whole chart
// provisional - retaining a second week is what makes every visible point a
// real figure. Buckets are seven numbers each, so the extra week costs
// nothing worth counting.
const hourRetention = rollingHours * 2

// Sample is one calibration observation: a percentage step and the spend that
// produced it.
type Sample struct {
	DP  float64 `json:"dp"`
	USD float64 `json:"usd"`
	TS  int64   `json:"ts"`
	// Cycle is the window cycle this sample was measured in, so evidence can
	// be told apart by which quota it priced. A timestamp cannot do this job:
	// a sample recorded either side of a boundary is the same instant, and the
	// boundary itself is upstream's clock, not ours.
	Cycle int64 `json:"cycle,omitempty"`
	// ByModel splits USD across the models that produced this step. A window
	// is one pool of quota, but a dollar does not buy the same share of it in
	// every model - measured on this fleet, a dollar of gpt-6-astra consumes
	// about 1.4x the quota a dollar of gpt-5.6-sol does. Without the split a
	// sample cannot say which of those it was measuring.
	ByModel map[string]float64 `json:"by_model,omitempty"`
}

const (
	// maxSamples bounds the calibration so it tracks the quota as it is now.
	// An unbounded sum cannot follow a plan change, a window-semantics change,
	// or a re-priced model: old evidence would outvote new evidence forever and
	// confidence would ratchet up while accuracy fell.
	maxSamples = 160
	// sampleMaxAgeDays drops evidence that is too old to describe the present.
	sampleMaxAgeDays = 21
	// minCycleEvidence is how many percentage points of movement the current
	// cycle has to have produced before it can be priced on its own evidence.
	// Below that the ratio is dominated by the 1-point rounding of the quota
	// headers, so the estimate reaches back into earlier cycles - but only far
	// enough to clear this bar, and newest first.
	minCycleEvidence = 5
)

// Window is one quota window's accounting and calibration, keyed by length.
type Window struct {
	Minutes int `json:"minutes"`

	Percent    float64 `json:"used_percent"`
	ResetAt    int64   `json:"reset_at"`
	ObservedAt int64   `json:"observed_at"`

	// Current cycle. Key is the reset timestamp that identifies the cycle.
	Key          int64                `json:"key"`
	Start        int64                `json:"start"`
	FullCoverage bool                 `json:"full_coverage"`
	USD          float64              `json:"usd"`
	Requests     int64                `json:"requests"`
	Failed       int64                `json:"failed"`
	SavedUSD     float64              `json:"saved_usd"`
	Tokens       Tokens               `json:"tokens"`
	ByModel      map[string]*ModelAgg `json:"by_model"`

	// Calibration, bounded in both count and age.
	LastPercent    float64            `json:"last_percent"`
	HasLast        bool               `json:"has_last"`
	PendingUSD     float64            `json:"pending_usd"`
	PendingByModel map[string]float64 `json:"pending_by_model,omitempty"`
	Samples        []Sample           `json:"samples"`

	// Diagnostics.
	Cycles         int64   `json:"cycles"`
	GrantedResets  int64   `json:"granted_resets"`
	UnexplainedPct float64 `json:"unexplained_percent"`

	// resumed marks a window restored from disk whose first fresh reading has
	// not been seen yet. Deliberately not persisted.
	resumed bool
}

// Account is the per-credential accumulator.
type Account struct {
	AuthID      string     `json:"auth_id"`
	AuthIndex   string     `json:"auth_index"`
	PlanType    string     `json:"plan_type"`
	ActiveLimit string     `json:"active_limit"`
	Provider    string     `json:"provider"`
	Credits     creditInfo `json:"credits"`

	// Windows is keyed by window length in minutes, rendered as a string
	// because JSON object keys must be strings.
	Windows map[string]*Window `json:"windows"`

	// Models is this credential's standing on each model it has served. The
	// proxy cools a credential down per model, so availability cannot be
	// answered from the windows above: they are shared by every model.
	Models map[string]*ModelHealth `json:"models,omitempty"`

	Hours map[string]*HourAgg `json:"hours"`

	UnpricedReqs int64    `json:"unpriced_requests"`
	UnpricedList []string `json:"unpriced_models,omitempty"`

	FirstSeen int64   `json:"first_seen"`
	TotalUSD  float64 `json:"total_usd"`
	TotalReqs int64   `json:"total_requests"`

	// Legacy flat fields, read once for migration and then left empty.
	LegacyWindowMin int     `json:"window_minutes,omitempty"`
	LegacyPercent   float64 `json:"used_percent,omitempty"`
}

type stateFile struct {
	Version  int                 `json:"version"`
	SavedAt  time.Time           `json:"saved_at"`
	Accounts map[string]*Account `json:"accounts"`
	Rotator  RotatorState        `json:"rotator,omitempty"`
}

// stateVersion 2 introduced per-length windows and bounded calibration;
// 3 added the per-model health ledger, which is purely additive - a version 2
// file loads unchanged and starts collecting model health from the next
// request.
const stateVersion = 3

// App owns all mutable plugin state.
type App struct {
	mu       sync.Mutex
	cfg      Config
	accounts map[string]*Account
	prices   *priceBook

	rot *rotator

	// bootWeights are the model burn ratios recovered from the event log at
	// startup, used only until live calibration can measure them.
	bootWeights ModelWeights

	dirty     bool
	events    []json.RawMessage
	authCache []authEntry
	authFetch time.Time
	stopOnce  sync.Once
	stop      chan struct{}
	started   bool
	startedAt time.Time
}

type authEntry struct {
	ID          string `json:"id"`
	AuthIndex   string `json:"auth_index"`
	Name        string `json:"name"`
	Label       string `json:"label"`
	Email       string `json:"email"`
	Status      string `json:"status"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	Type        string `json:"type"`
}

func newApp(cfg Config) (*App, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", cfg.DataDir, err)
	}
	app := &App{
		cfg:       cfg,
		accounts:  make(map[string]*Account),
		prices:    newPriceBook(cfg.DataDir),
		stop:      make(chan struct{}),
		startedAt: time.Now(),
	}
	app.rot = newRotator(app)
	app.prices.setOverrides(cfg.PriceOverrides)
	app.loadState()
	app.start()
	return app, nil
}

// Reconfigure swaps the config on a live app without losing accumulated state.
func (a *App) Reconfigure(cfg Config) {
	a.mu.Lock()
	oldDir := a.cfg.DataDir
	a.cfg = cfg
	a.mu.Unlock()

	if cfg.DataDir != oldDir {
		if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
			hostLog("warn", "create data dir failed: "+err.Error())
		}
	}
	a.prices.setOverrides(cfg.PriceOverrides)
}

func (a *App) start() {
	if a.started {
		return
	}
	a.started = true
	go a.loop()
	go a.rot.loop(a.stop)
}

func (a *App) loop() {
	defer func() { _ = recover() }()

	a.mu.Lock()
	flushEvery := time.Duration(a.cfg.FlushSeconds) * time.Second
	url := a.cfg.PriceSourceURL
	refresh := a.cfg.priceRefresh()
	a.mu.Unlock()

	if a.prices.stale(refresh) {
		a.prices.refresh(url)
	}
	a.pruneEventLogs()

	flush := time.NewTicker(flushEvery)
	price := time.NewTicker(time.Hour)
	defer flush.Stop()
	defer price.Stop()

	for {
		select {
		case <-a.stop:
			a.Flush()
			return
		case <-flush.C:
			a.Flush()
		case <-price.C:
			a.mu.Lock()
			url, refresh = a.cfg.PriceSourceURL, a.cfg.priceRefresh()
			a.mu.Unlock()
			if a.prices.stale(refresh) {
				a.prices.refresh(url)
				a.pruneEventLogs()
			}
		}
	}
}

func (a *App) Shutdown() {
	a.stopOnce.Do(func() {
		close(a.stop)
	})
	a.Flush()
}

func (acct *Account) window(minutes int) *Window {
	if acct.Windows == nil {
		acct.Windows = map[string]*Window{}
	}
	key := strconv.Itoa(minutes)
	w := acct.Windows[key]
	if w == nil {
		w = &Window{Minutes: minutes, ByModel: map[string]*ModelAgg{}}
		acct.Windows[key] = w
	}
	if w.ByModel == nil {
		w.ByModel = map[string]*ModelAgg{}
	}
	return w
}

// HandleUsage is the hot path. It runs inline with request completion, so it
// only touches memory: disk writes are left to the flush ticker.
func (a *App) HandleUsage(payload []byte) {
	defer func() {
		if r := recover(); r != nil {
			hostLog("warn", fmt.Sprintf("usage.handle recovered: %v", r))
		}
	}()

	var rec usageRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return
	}
	key := strings.TrimSpace(rec.AuthID)
	if key == "" {
		key = strings.TrimSpace(rec.AuthIndex)
	}
	if key == "" {
		return
	}

	model := strings.TrimSpace(rec.Model)
	if model == "" {
		model = strings.TrimSpace(rec.Alias)
	}
	rl := parseRateLimit(rec.Headers)

	a.mu.Lock()
	cfg := a.cfg
	acct := a.accounts[key]
	if acct == nil {
		acct = &Account{
			AuthID:    rec.AuthID,
			AuthIndex: rec.AuthIndex,
			FirstSeen: time.Now().Unix(),
			Windows:   map[string]*Window{},
			Hours:     map[string]*HourAgg{},
		}
		a.accounts[key] = acct
	}
	a.mu.Unlock()

	price, priced := a.prices.Lookup(model)
	cost, saved := 0.0, 0.0
	if priced && !rec.Failed {
		cost = price.Cost(rec.Detail, cfg.LongContextThreshold, cfg.LongContextMultiplier)
		// What prompt caching is worth: the same tokens at the full input rate
		// minus what they actually cost at the cache-read rate.
		if price.CacheRead > 0 && price.CacheRead < price.Input {
			saved = float64(rec.Detail.CacheRead) * (price.Input - price.CacheRead) / 1e6
		}
	}

	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()

	if rec.AuthIndex != "" {
		acct.AuthIndex = rec.AuthIndex
	}
	if rec.Provider != "" {
		acct.Provider = rec.Provider
	}
	if rl.Found {
		if rl.PlanType != "" {
			acct.PlanType = rl.PlanType
		}
		if rl.ActiveLimit != "" {
			acct.ActiveLimit = rl.ActiveLimit
		}
		acct.Credits = rl.Credits
	}

	// Every window reported on this response is advanced and billed. The same
	// dollars count against both the 5-hour and the weekly limit, so each window
	// keeps its own ledger rather than sharing one.
	for _, wr := range rl.Windows {
		w := acct.window(wr.Minutes)
		w.advance(wr, now)
		w.bill(cost, saved, rec, model, priced)
	}

	// Availability is tracked per model because the proxy's cooldown is: the
	// windows above are shared by every model, so they cannot say which model
	// is currently refused.
	acct.recordHealth(model, rec, rl, now, cfg.ModelHealthDays)

	acct.TotalUSD += cost
	acct.TotalReqs++
	if !priced && !rec.Failed {
		acct.UnpricedReqs++
		acct.noteUnpriced(model)
	}
	acct.recordHour(now, cost, rec, rl)

	a.dirty = true
	if cfg.EventLog {
		a.appendEvent(rec, model, cost, priced, rl, now)
	}
}

// advance folds a fresh reading into one window, opening a new cycle when the
// old one ended and calibrating from the percentage step.
func (w *Window) advance(r windowReading, now time.Time) {
	switch {
	case w.Key == 0 || (r.ResetAt > 0 && absInt64(r.ResetAt-w.Key) > 120):
		// The reset timestamp moved: the cycle rolled over normally.
		w.startCycle(r, now, false)

	case w.HasLast && r.Percent < w.LastPercent-0.001:
		// The percentage fell while the reset timestamp stayed put. Upstream
		// granted a mid-cycle reset. Without this branch the old cycle's spend
		// would be divided by the new small percentage and the quota estimate
		// would explode.
		w.startCycle(r, now, true)
	}

	// First reading after a restart. Traffic served while the plugin was down is
	// inside the percentage but not inside our ledger, so anything that depends
	// on having watched every dollar has to be invalidated.
	if w.resumed {
		w.resumed = false
		if w.HasLast && r.Percent > w.LastPercent+0.001 {
			w.FullCoverage = false
			w.clearPending()
			w.LastPercent = r.Percent
		}
	}

	if w.HasLast && r.Percent > w.LastPercent {
		dp := r.Percent - w.LastPercent
		if w.PendingUSD > 0 {
			w.Samples = append(w.Samples, Sample{DP: dp, USD: w.PendingUSD, TS: now.Unix(),
				Cycle: w.Key, ByModel: copyUSD(w.PendingByModel)})
			w.pruneSamples(now)
		} else {
			// The percentage moved but we billed nothing for it: another client
			// is spending this credential's quota. Worth surfacing rather than
			// silently skipping, because it also means our ledger undercounts.
			w.UnexplainedPct += dp
		}
		w.clearPending()
	}

	w.LastPercent = r.Percent
	w.HasLast = true
	w.Percent = r.Percent
	w.ResetAt = r.ResetAt
	w.ObservedAt = now.Unix()
}

// startCycle resets per-cycle accounting. Calibration samples survive the roll,
// but they no longer all count: freshEvidence prefers the ones this cycle
// produced, because a rollover demonstrably can change the size of the quota.
func (w *Window) startCycle(r windowReading, now time.Time, granted bool) {
	w.Key = r.ResetAt
	if r.ResetAt > 0 && w.Minutes > 0 {
		w.Start = r.ResetAt - int64(w.Minutes)*60
	} else {
		w.Start = now.Unix()
	}
	// Full coverage means the first reading of this cycle showed nothing spent,
	// so every dollar since then is money this plugin actually saw.
	w.FullCoverage = r.Percent <= 0
	w.USD = 0
	w.Requests = 0
	w.Failed = 0
	w.SavedUSD = 0
	w.Tokens = Tokens{}
	w.ByModel = map[string]*ModelAgg{}
	// Pending spend belonged to the cycle that just closed and can no longer be
	// attributed to a percentage move, so it is dropped rather than mixed in.
	w.clearPending()
	w.HasLast = false
	w.LastPercent = 0
	w.Cycles++
	if granted {
		w.GrantedResets++
	}
}

func (w *Window) bill(cost, saved float64, rec usageRecord, model string, priced bool) {
	w.PendingUSD += cost
	if cost != 0 {
		if w.PendingByModel == nil {
			w.PendingByModel = map[string]float64{}
		}
		w.PendingByModel[model] += cost
	}
	w.USD += cost
	w.SavedUSD += saved
	w.Requests++
	w.Tokens.add(rec.Detail)
	if rec.Failed {
		w.Failed++
	}

	agg := w.ByModel[model]
	if agg == nil {
		agg = &ModelAgg{}
		w.ByModel[model] = agg
	}
	agg.Requests++
	agg.Tokens.add(rec.Detail)
	agg.USD += cost
	agg.SavedUSD += saved
	if rec.Failed {
		agg.Failed++
	}
	if !priced && !rec.Failed {
		agg.Unpriced++
	}
}

// pruneSamples keeps calibration bounded in both count and age so the estimate
// follows the quota as it is now rather than as it was.
func (w *Window) pruneSamples(now time.Time) {
	cutoff := now.AddDate(0, 0, -sampleMaxAgeDays).Unix()
	kept := w.Samples[:0]
	for _, s := range w.Samples {
		if s.TS >= cutoff {
			kept = append(kept, s)
		}
	}
	w.Samples = kept
	if len(w.Samples) > maxSamples {
		w.Samples = append([]Sample(nil), w.Samples[len(w.Samples)-maxSamples:]...)
	}
}

// recordHour folds one request into the rolling hourly series and drops buckets
// that have aged past the retention window.
func (acct *Account) recordHour(now time.Time, cost float64, rec usageRecord, rl rateLimit) {
	if acct.Hours == nil {
		acct.Hours = map[string]*HourAgg{}
	}
	hour := now.Unix() / 3600
	key := strconv.FormatInt(hour, 10)
	bucket := acct.Hours[key]
	if bucket == nil {
		bucket = &HourAgg{}
		acct.Hours[key] = bucket
		// A new hour is the cheapest moment to drop what has aged out, and it
		// happens once an hour rather than once a request.
		acct.pruneHours(hour)
	}
	bucket.Requests++
	bucket.USD += cost
	bucket.In += rec.Detail.Input
	bucket.Out += rec.Detail.Output
	bucket.CacheRead += rec.Detail.CacheRead
	if rec.Failed {
		bucket.Failed++
	}
	// The series tracks the longest window, which is the one whose curve spans
	// enough hours to be worth drawing.
	if w := longestReading(rl); w != nil {
		bucket.Percent = w.Percent
	}

}

// pruneHours drops buckets older than the retention window.
//
// This used to be guarded by the size of the map, which did not bound age at
// all: most hours carry no traffic, so the map stayed well under the limit
// while the series stretched far past the retention it claimed to enforce -
// eighteen days of chart against a ten-day bound, measured in production.
func (acct *Account) pruneHours(hour int64) {
	cutoff := hour - hourRetention
	for k := range acct.Hours {
		if h, err := strconv.ParseInt(k, 10, 64); err == nil && h < cutoff {
			delete(acct.Hours, k)
		}
	}
}

func longestReading(rl rateLimit) *windowReading {
	var best *windowReading
	for i := range rl.Windows {
		if best == nil || rl.Windows[i].Minutes > best.Minutes {
			best = &rl.Windows[i]
		}
	}
	return best
}

func (acct *Account) noteUnpriced(model string) {
	for _, m := range acct.UnpricedList {
		if m == model {
			return
		}
	}
	if len(acct.UnpricedList) < 20 {
		acct.UnpricedList = append(acct.UnpricedList, model)
	}
}

// Estimate is the derived view of one window.
type Estimate struct {
	QuotaUSD      float64 `json:"quota_usd"`
	SpentUSD      float64 `json:"spent_usd"`
	RemainingUSD  float64 `json:"remaining_usd"`
	QuotaByWindow float64 `json:"quota_usd_by_window"`
	QuotaByDelta  float64 `json:"quota_usd_by_delta"`
	AttributedUSD float64 `json:"attributed_usd"`
	Method        string  `json:"method"`
	Confidence    string  `json:"confidence"`
	Evidence      float64 `json:"evidence_percent"`
	Samples       int     `json:"samples"`

	// Per-model view. The window is one pool of quota; what differs is how
	// much of it a dollar buys in each model. QuotaByModel answers "how many
	// dollars of THIS model would consume the whole window", which is the only
	// form of the number that stays true when the model mix changes.
	QuotaByModel     map[string]float64 `json:"quota_usd_by_model,omitempty"`
	RemainingByModel map[string]float64 `json:"remaining_usd_by_model,omitempty"`
	ModelSource      map[string]string  `json:"quota_model_source,omitempty"`
	DominantModel    string             `json:"dominant_model,omitempty"`
}

// ModelWeights is how fast each model burns a window per dollar spent, as
// ratios between models. The scale is arbitrary and never reported: only
// weight[a]/weight[b] is ever used, which is what makes the numbers comparable
// across accounts whose quotas are different sizes.
type ModelWeights map[string]float64

// dominantShare is how much of a sample's spend must belong to one model
// before that sample is allowed to price it. Below this the sample is a blend
// and says nothing about either model on its own.
const dominantShare = 0.95

// localQuota prices the window in each model this window's own evidence can
// speak for. It needs no weights: a sample where one model produced 95% of the
// spend measures that model directly.
func (w *Window) localQuota() map[string]float64 {
	type acc struct{ dp, usd float64 }
	per := map[string]*acc{}
	fold := func(s Sample) {
		if s.DP <= 0 || s.USD <= 0 || len(s.ByModel) == 0 {
			return
		}
		for m, usd := range s.ByModel {
			if usd/s.USD < dominantShare {
				continue
			}
			a := per[m]
			if a == nil {
				a = &acc{}
				per[m] = a
			}
			a.dp += s.DP
			a.usd += s.USD
			return
		}
	}
	for _, s := range w.Samples {
		if s.Cycle == w.Key {
			fold(s)
		}
	}
	// Reach back exactly as freshEvidence does, and for the same reason: a
	// cycle that has just rolled has not moved enough to be divided by.
	for i := len(w.Samples) - 1; i >= 0; i-- {
		s := w.Samples[i]
		if s.Cycle == w.Key {
			continue
		}
		short := false
		for _, a := range per {
			if a.dp < minCycleEvidence {
				short = true
			}
		}
		if len(per) > 0 && !short {
			break
		}
		fold(s)
	}
	out := map[string]float64{}
	for m, a := range per {
		if a.dp >= minCycleEvidence && a.usd > 0 {
			out[m] = a.usd / (a.dp / 100)
		}
	}
	return out
}

func (w *Window) Estimate() Estimate { return w.EstimateWith(nil) }

func (w *Window) EstimateWith(weights ModelWeights) Estimate {
	var e Estimate

	// Spend that the latest percentage reading has actually had a chance to
	// account for. PendingUSD is everything billed after that reading, so
	// dividing the raw cycle total by the percentage would overstate the quota.
	e.AttributedUSD = w.USD - w.PendingUSD
	if e.AttributedUSD < 0 {
		e.AttributedUSD = 0
	}

	sumDP, sumUSD, used := w.freshEvidence()
	e.Samples = used
	if sumDP > 0 && sumUSD > 0 {
		e.QuotaByDelta = sumUSD / (sumDP / 100)
	}
	if w.FullCoverage && w.Percent > 0 && e.AttributedUSD > 0 {
		e.QuotaByWindow = e.AttributedUSD / (w.Percent / 100)
	}

	// A fully observed cycle is the stronger measurement because it prices the
	// whole cycle against the whole percentage, with no attribution guesswork.
	switch {
	case e.QuotaByWindow > 0 && w.Percent >= 3:
		e.QuotaUSD, e.Method = e.QuotaByWindow, "window"
		e.Evidence = w.Percent
	case e.QuotaByDelta > 0:
		e.QuotaUSD, e.Method = e.QuotaByDelta, "delta"
		e.Evidence = sumDP
	case e.QuotaByWindow > 0:
		e.QuotaUSD, e.Method = e.QuotaByWindow, "window"
		e.Evidence = w.Percent
	default:
		e.Method = "none"
	}

	if e.QuotaUSD > 0 {
		e.SpentUSD = e.QuotaUSD * w.Percent / 100
		e.RemainingUSD = e.QuotaUSD - e.SpentUSD
	}

	switch {
	case e.Method == "none":
		e.Confidence = "none"
	case e.Evidence >= 25:
		e.Confidence = "high"
	case e.Evidence >= 8:
		e.Confidence = "medium"
	default:
		e.Confidence = "low"
	}

	e.fillModels(w, weights)
	return e
}

// fillModels prices the window in every model worth reporting: directly where
// this window has watched that model, and otherwise by carrying a model it has
// watched across the fleet-wide ratio between the two.
//
// The second path is what makes the number usable. A credential that has only
// ever served sol still has a real answer to "how much astra would this window
// buy", and it is not the sol figure - it is about 30% less. Answering with the
// sol figure is what the single blended quota did, and it is why a credential
// looked like it could serve for hours longer than it could the moment traffic
// moved to a more expensive model.
func (e *Estimate) fillModels(w *Window, weights ModelWeights) {
	local := w.localQuota()

	// What this cycle is actually being spent on, which is the figure the
	// headline number and everything derived from it describes.
	best := 0.0
	for m, agg := range w.ByModel {
		if agg != nil && agg.USD > best {
			e.DominantModel, best = m, agg.USD
		}
	}

	if len(local) == 0 && (e.QuotaUSD <= 0 || e.DominantModel == "") {
		return
	}
	quota := map[string]float64{}
	source := map[string]string{}
	for m, q := range local {
		quota[m], source[m] = q, "measured"
	}
	// The headline figure prices whatever mix this cycle ran. When that mix was
	// essentially one model, it is a direct reading of that model and worth
	// keeping - by share, not by count, because a cycle that spent $37 of one
	// model and one cent of another is not a blend of anything, and demanding
	// a single entry threw away the reading over that cent.
	if _, ok := quota[e.DominantModel]; !ok && e.QuotaUSD > 0 && best > 0 {
		total := 0.0
		for _, agg := range w.ByModel {
			if agg != nil {
				total += agg.USD
			}
		}
		if total > 0 && best/total >= dominantShare {
			quota[e.DominantModel], source[e.DominantModel] = e.QuotaUSD, "measured"
		}
	}

	// Carry to the models this window has no evidence for. The anchor is the
	// best-evidenced model we do have a figure for.
	if len(weights) > 0 && len(quota) > 0 {
		// The model taking the traffic anchors the carry when we have priced it,
		// because that is the figure the rest of the panel is describing. Map
		// order is random, so the fallback is by name rather than by luck: two
		// renders of the same state must not disagree.
		anchor, anchorQ := "", 0.0
		for m, q := range quota {
			if weights[m] <= 0 {
				continue
			}
			if m == e.DominantModel {
				anchor, anchorQ = m, q
				break
			}
			if anchor == "" || m < anchor {
				anchor, anchorQ = m, q
			}
		}
		if anchor != "" {
			for m, wm := range weights {
				if _, ok := quota[m]; ok || wm <= 0 {
					continue
				}
				quota[m] = anchorQ * weights[anchor] / wm
				source[m] = "carried from " + anchor
			}
		}
	}
	if len(quota) == 0 {
		return
	}
	e.QuotaByModel = quota
	e.ModelSource = source
	e.RemainingByModel = map[string]float64{}
	for m, q := range quota {
		r := q * (1 - w.Percent/100)
		if r < 0 {
			r = 0
		}
		e.RemainingByModel[m] = r
	}
}

// parseRateLimit reads the Codex quota headers. Lookup is case-insensitive
// because the host canonicalises header names.
func parseRateLimit(headers map[string][]string) rateLimit {
	var rl rateLimit
	if len(headers) == 0 {
		return rl
	}
	lower := make(map[string]string, len(headers))
	for k, v := range headers {
		if len(v) > 0 {
			lower[strings.ToLower(k)] = strings.TrimSpace(v[0])
		}
	}
	get := func(name string) string { return lower[name] }

	// Both slots are read the same way and stored under their length. Upstream
	// has already swapped which slot carries which window once.
	for _, slot := range []string{"primary", "secondary"} {
		pct, okPct := parseFloat(get("x-codex-" + slot + "-used-percent"))
		win, okWin := parseInt(get("x-codex-" + slot + "-window-minutes"))
		if !okPct || !okWin || win <= 0 {
			continue
		}
		r := windowReading{Minutes: int(win), Percent: clampPercent(pct)}
		if v, ok := parseInt(get("x-codex-" + slot + "-reset-at")); ok {
			r.ResetAt = v
		}
		if v, ok := parseInt(get("x-codex-" + slot + "-reset-after-seconds")); ok {
			r.ResetAfter = v
			if r.ResetAt == 0 {
				r.ResetAt = time.Now().Unix() + v
			}
		}
		rl.Windows = append(rl.Windows, r)
		rl.Found = true
	}
	if !rl.Found {
		return rl
	}
	sort.Slice(rl.Windows, func(i, j int) bool { return rl.Windows[i].Minutes < rl.Windows[j].Minutes })

	rl.PlanType = get("x-codex-plan-type")
	rl.ActiveLimit = get("x-codex-active-limit")
	rl.Credits = creditInfo{
		HasCredits:   strings.EqualFold(get("x-codex-credits-has-credits"), "true"),
		Unlimited:    strings.EqualFold(get("x-codex-credits-unlimited"), "true"),
		Balance:      get("x-codex-credits-balance"),
		LimitReached: get("x-codex-rate-limit-reached-type"),
	}
	return rl
}

func (a *App) loadState() {
	raw, err := os.ReadFile(filepath.Join(a.cfg.DataDir, "state.json"))
	if err != nil {
		return
	}
	var sf stateFile
	if err = json.Unmarshal(raw, &sf); err != nil {
		hostLog("warn", "state.json is unreadable, starting fresh: "+err.Error())
		return
	}
	migrated := 0
	for k, v := range sf.Accounts {
		if v == nil {
			continue
		}
		if v.Hours == nil {
			v.Hours = map[string]*HourAgg{}
		}
		// Prune on load as well as on the hour. Otherwise a file written by a
		// build that bounded the series by map size keeps its overlong history
		// until the clock happens to tick into a new hour, and the chart goes
		// on covering weeks in the meantime.
		v.pruneHours(time.Now().Unix() / 3600)
		if v.Models == nil {
			v.Models = map[string]*ModelHealth{}
		}
		if len(v.Windows) == 0 && v.LegacyWindowMin > 0 {
			// Version 1 tracked a single window and summed calibration across
			// every cycle forever. Those sums mixed percentage steps from two
			// differently sized windows once upstream swapped primary and
			// secondary, and a 1% step is worth wildly different money in a
			// 5-hour window than in a weekly one. The cycle counters carry over;
			// the calibration cannot and is dropped rather than kept wrong.
			w := &Window{Minutes: v.LegacyWindowMin, Percent: v.LegacyPercent, ByModel: map[string]*ModelAgg{}}
			v.Windows = map[string]*Window{strconv.Itoa(v.LegacyWindowMin): w}
			migrated++
		}
		v.LegacyWindowMin, v.LegacyPercent = 0, 0
		for _, w := range v.Windows {
			if w == nil {
				continue
			}
			if w.ByModel == nil {
				w.ByModel = map[string]*ModelAgg{}
			}
			w.resumed = true
		}
		a.accounts[k] = v
	}
	if migrated > 0 {
		hostLog("info", fmt.Sprintf("migrated %d credential(s) to per-length windows; prior calibration discarded because it mixed window sizes", migrated))
	}
	a.rot.restore(sf.Rotator)

	// Recover the model ratios from history. Stored calibration carries no
	// model split before this version, so without this the ratios cannot be
	// learned until some window happens to serve two models - which on a fleet
	// that has moved wholesale to one model may be a long wait.
	if a.bootWeights = a.bootstrapModelWeights(); len(a.bootWeights) > 1 {
		parts := make([]string, 0, len(a.bootWeights))
		for m, w := range a.bootWeights {
			parts = append(parts, fmt.Sprintf("%s=%.2f", m, w))
		}
		sort.Strings(parts)
		hostLog("info", "model burn ratios recovered from the event log: "+strings.Join(parts, " "))
	}
}

// markDirty flags the state for the next flush without holding any other lock,
// so callers that own a different lock can use it safely.
func (a *App) markDirty() {
	a.mu.Lock()
	a.dirty = true
	a.mu.Unlock()
}

// Flush writes the state snapshot and drains the buffered event log.
func (a *App) Flush() {
	a.mu.Lock()
	if !a.dirty && len(a.events) == 0 {
		a.mu.Unlock()
		return
	}
	snapshot := stateFile{Version: stateVersion, SavedAt: time.Now().UTC(), Accounts: make(map[string]*Account, len(a.accounts)), Rotator: a.rot.snapshot()}
	for k, v := range a.accounts {
		snapshot.Accounts[k] = v
	}
	events := a.events
	a.events = nil
	a.dirty = false
	dir := a.cfg.DataDir
	a.mu.Unlock()

	if body, err := json.MarshalIndent(snapshot, "", "  "); err == nil {
		if err = writeFileAtomic(filepath.Join(dir, "state.json"), body); err != nil {
			hostLog("warn", "state write failed: "+err.Error())
		}
	}

	if len(events) > 0 {
		if err := appendEventLines(dir, events); err != nil {
			hostLog("warn", "event log write failed: "+err.Error())
		}
	}
}

func (a *App) appendEvent(rec usageRecord, model string, cost float64, priced bool, rl rateLimit, now time.Time) {
	ev := map[string]any{
		"ts":      now.UTC().Format(time.RFC3339),
		"auth_id": rec.AuthID,
		"model":   model,
		"usd":     round6(cost),
		"priced":  priced,
		"failed":  rec.Failed,
		"in":      rec.Detail.Input,
		"out":     rec.Detail.Output,
		"reason":  rec.Detail.Reasoning,
		"cache_r": rec.Detail.CacheRead,
		"cache_w": rec.Detail.CacheWrite,
	}
	for _, w := range rl.Windows {
		ev["pct_"+strconv.Itoa(w.Minutes)] = w.Percent
	}
	if rl.Credits.LimitReached != "" {
		ev["limit_reached"] = rl.Credits.LimitReached
	}
	if raw, err := json.Marshal(ev); err == nil {
		a.events = append(a.events, raw)
		if len(a.events) > 20000 {
			a.events = a.events[len(a.events)-20000:]
		}
	}
}

func (a *App) pruneEventLogs() {
	a.mu.Lock()
	dir := filepath.Join(a.cfg.DataDir, "events")
	keep := a.cfg.EventLogKeepDays
	a.mu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -keep)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		stamp := strings.TrimSuffix(entry.Name(), ".jsonl")
		day, errParse := time.Parse("2006-01-02", stamp)
		if errParse != nil || !day.Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

// authMetadata resolves credential labels via the host, cached briefly so the
// dashboard does not hammer the auth manager on every refresh.
func (a *App) authMetadata() []authEntry {
	a.mu.Lock()
	if time.Since(a.authFetch) < time.Minute && a.authCache != nil {
		cached := a.authCache
		a.mu.Unlock()
		return cached
	}
	a.mu.Unlock()

	result, err := hostCall("host.auth.list", []byte("{}"))
	if err != nil {
		return nil
	}
	var resp struct {
		Files []authEntry `json:"files"`
	}
	if err = json.Unmarshal(result, &resp); err != nil {
		return nil
	}

	a.mu.Lock()
	a.authCache = resp.Files
	a.authFetch = time.Now()
	a.mu.Unlock()
	return resp.Files
}

// lookupAuth resolves one credential against a metadata list already fetched
// by the caller. It deliberately does not fetch: callers hold the state lock,
// and authMetadata takes it.
// invalidateAuthCache drops the cached credential list. The rotator calls it
// after changing one: the cache is a minute long, and a recount taken against
// the stale view sees a credential it just switched off as still enabled, and
// switches it off again - a duplicate write that also spends a change against
// the daily budget.
func (a *App) invalidateAuthCache() {
	a.mu.Lock()
	a.authCache = nil
	a.authFetch = time.Time{}
	a.mu.Unlock()
}

func lookupAuth(entries []authEntry, acct *Account) (authEntry, bool) {
	for _, entry := range entries {
		switch {
		case entry.ID != "" && entry.ID == acct.AuthID,
			entry.AuthIndex != "" && entry.AuthIndex == acct.AuthIndex,
			entry.Name != "" && entry.Name == acct.AuthID:
			return entry, true
		}
	}
	return authEntry{}, false
}

// Report builds the JSON payload the dashboard renders.
//
// It holds the state lock for the whole build. The accounts it walks are the
// live ones, and the request path inserts into and deletes from their maps -
// the hourly buckets roll over every hour, a model health record appears the
// first time a credential serves a model. Iterating a map while another
// goroutine writes to it is a fatal runtime error, not a recoverable panic, and
// in a c-shared library that takes the whole proxy down with it. Credential
// metadata is fetched before the lock is taken because that path locks too.
// config returns a copy of the live configuration without holding the state
// lock for longer than the read.
func (a *App) config() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *App) Report() map[string]any {
	now := time.Now()
	entries := a.authMetadata()

	// Worked out before the state lock, because deriving it reads the same
	// accounting state this function is about to hold: taking that lock twice
	// deadlocks. It is derived per request rather than served from the last
	// decision so that refreshing the panel actually refreshes something.
	rotatorBoard := a.rot.board(entries, a.config().Rotator, now)

	a.mu.Lock()
	defer a.mu.Unlock()

	// One pass over the whole fleet before anything is priced: the ratio
	// between two models can only be measured where a single window has
	// watched both, and every window's estimate then gets to use it.
	weights := a.deriveModelWeights()

	accounts := make([]*Account, 0, len(a.accounts))
	for _, acct := range a.accounts {
		accounts = append(accounts, acct)
	}
	cfg := a.cfg
	staleAfter := time.Duration(cfg.StaleAfterMinutes) * time.Minute

	rows := make([]map[string]any, 0, len(accounts))
	// Accounts whose credential the host no longer lists. Kept apart rather
	// than dropped so the panel can still account for what they spent.
	removed := make([]map[string]any, 0)
	// Warnings are structured rather than prose: the panel renders them in the
	// language the operator picked, so the text cannot live here.
	warnings := make([]map[string]any, 0)
	totals := map[int]*windowTotals{}
	var totalWindowUSD, totalSaved float64
	var totalReqs, totalFailed int64

	for _, acct := range accounts {
		windows := make([]map[string]any, 0, len(acct.Windows))
		lengths := make([]int, 0, len(acct.Windows))
		for _, w := range acct.Windows {
			if w != nil {
				lengths = append(lengths, w.Minutes)
			}
		}
		sort.Ints(lengths)

		var binding map[string]any
		var bindingPct float64 = -1
		for _, m := range lengths {
			w := acct.Windows[strconv.Itoa(m)]
			est := w.EstimateWith(weights)
			entry := map[string]any{
				"minutes":             w.Minutes,
				"label":               windowLabel(w.Minutes),
				"used_percent":        round2(w.Percent),
				"usd_observed":        round4(w.USD),
				"requests":            w.Requests,
				"failed":              w.Failed,
				"saved_usd":           round4(w.SavedUSD),
				"tokens":              w.Tokens,
				"full_coverage":       w.FullCoverage,
				"cycles":              w.Cycles,
				"granted_resets":      w.GrantedResets,
				"unexplained_percent": round2(w.UnexplainedPct),
				"estimate":            est,
				"by_model":            modelRows(a, w),
			}
			if w.ResetAt > 0 {
				// Roll a boundary that has already gone past forward by whole
				// periods. Without this the countdown clamps to zero and stays
				// there for as long as the credential is idle, so the panel
				// shows a deadline that expired hours ago next to a percentage
				// that stopped being true at the same moment. The percentage is
				// left exactly as upstream last reported it - inventing a reset
				// value would be worse than admitting the reading is old - and
				// flagged, so the panel can say it is believed rather than seen.
				reset := time.Unix(w.ResetAt, 0)
				if w.Minutes > 0 && !reset.After(now) {
					period := int64(w.Minutes) * 60
					elapsed := now.Unix() - w.ResetAt
					reset = time.Unix(w.ResetAt+(elapsed/period+1)*period, 0)
					entry["reset_inferred"] = true
					entry["used_percent_stale"] = true
				}
				entry["reset_at"] = reset.UTC().Format(time.RFC3339)
				entry["reset_in_seconds"] = int64(math.Max(0, time.Until(reset).Seconds()))
			}
			if w.Start > 0 && w.Minutes > 0 {
				total := float64(w.Minutes) * 60
				timePct := clampPercent(now.Sub(time.Unix(w.Start, 0)).Seconds() / total * 100)
				entry["time_progress_percent"] = round2(timePct)
				if timePct > 1 {
					entry["pace_ratio"] = round2(w.Percent / timePct)
				}
			}
			if est.QuotaUSD > 0 && w.Start > 0 {
				elapsed := now.Sub(time.Unix(w.Start, 0)).Hours()
				if elapsed > 0.05 {
					perDay := est.SpentUSD / (elapsed / 24)
					entry["burn_usd_per_day"] = round2(perDay)
					if perDay > 0 {
						daysLeft := est.RemainingUSD / perDay
						entry["runway_days"] = round4(daysLeft)
						entry["will_exhaust_before_reset"] = daysLeft < time.Until(time.Unix(w.ResetAt, 0)).Hours()/24
					}
				}
			}
			if w.Requests > 0 {
				entry["avg_usd_per_request"] = round6(w.USD / float64(w.Requests))
				entry["failure_rate"] = round2(float64(w.Failed) / float64(w.Requests) * 100)
			}
			if w.Tokens.Input > 0 {
				entry["cache_hit_rate"] = round2(float64(w.Tokens.CacheRead) / float64(w.Tokens.Input) * 100)
			}

			t := totals[w.Minutes]
			if t == nil {
				t = &windowTotals{Minutes: w.Minutes}
				totals[w.Minutes] = t
			}
			t.Credentials++
			t.QuotaUSD += est.QuotaUSD
			t.SpentUSD += est.SpentUSD
			t.RemainingUSD += est.RemainingUSD
			if est.Method != "none" {
				t.Estimated++
			}
			if w.Percent >= 90 {
				t.AtRisk++
			}
			if w.UnexplainedPct > 1 {
				warnings = append(warnings, map[string]any{
					"code":       "external_usage",
					"credential": displayName(entries, acct),
					"window":     windowLabel(w.Minutes),
					"percent":    round2(w.UnexplainedPct),
				})
			}

			if w.Percent > bindingPct {
				bindingPct, binding = w.Percent, entry
			}
			windows = append(windows, entry)
		}

		// The longest window is the headline figure: it is the one that decides
		// how much a credential is worth over a full billing cycle.
		var longest map[string]any
		if len(windows) > 0 {
			longest = windows[len(windows)-1]
			totalWindowUSD += toF(longest["usd_observed"])
			totalSaved += toF(longest["saved_usd"])
			totalReqs += toI(longest["requests"])
			totalFailed += toI(longest["failed"])
		}

		row := map[string]any{
			"auth_id":           acct.AuthID,
			"provider":          acct.Provider,
			"plan_type":         acct.PlanType,
			"active_limit":      acct.ActiveLimit,
			"credits":           acct.Credits,
			"has_quota_data":    len(windows) > 0,
			"windows":           windows,
			"binding":           binding,
			"longest":           longest,
			"total_usd":         round4(acct.TotalUSD),
			"total_requests":    acct.TotalReqs,
			"unpriced_requests": acct.UnpricedReqs,
			"series":            seriesOf(acct, now),
		}
		if len(acct.UnpricedList) > 0 {
			row["unpriced_models"] = acct.UnpricedList
			warnings = append(warnings, map[string]any{
				"code":       "unpriced_models",
				"credential": displayName(entries, acct),
				"models":     acct.UnpricedList,
			})
		}
		if acct.Credits.LimitReached != "" {
			row["limit_reached_type"] = acct.Credits.LimitReached
		}

		// Staleness: a reading from days ago must not be presented as current.
		var newest int64
		for _, w := range acct.Windows {
			if w != nil && w.ObservedAt > newest {
				newest = w.ObservedAt
			}
		}
		if newest > 0 {
			age := int64(now.Sub(time.Unix(newest, 0)).Seconds())
			row["observed_at"] = time.Unix(newest, 0).UTC().Format(time.RFC3339)
			row["observed_age_seconds"] = age
			row["stale"] = staleAfter > 0 && time.Duration(age)*time.Second > staleAfter
		}

		if entry, ok := lookupAuth(entries, acct); ok {
			row["label"] = firstNonEmpty(entry.Label, entry.Email, entry.Name)
			row["email"] = entry.Email
			row["disabled"] = entry.Disabled
			row["status"] = entry.Status
			row["unavailable"] = entry.Unavailable
		} else if len(entries) > 0 {
			// The host no longer lists this credential: the auth file was
			// deleted. Its accounting is real history and stays in state.json,
			// but showing it beside live credentials invites reading a deleted
			// account as a working one - and it cannot be acted on, since there
			// is nothing left to enable. The guard on len(entries) matters: an
			// auth list that came back empty is a failure to ask, not evidence
			// that every credential was deleted.
			removed = append(removed, map[string]any{
				"auth_id":        acct.AuthID,
				"total_usd":      round4(acct.TotalUSD),
				"total_requests": acct.TotalReqs,
				"last_seen":      row["observed_at"],
			})
			continue
		} else {
			row["label"] = acct.AuthID
		}

		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		return longestQuota(rows[i]) > longestQuota(rows[j])
	})

	lengths := make([]int, 0, len(totals))
	for m := range totals {
		lengths = append(lengths, m)
	}
	sort.Ints(lengths)
	windowTotalRows := make([]map[string]any, 0, len(lengths))
	for _, m := range lengths {
		t := totals[m]
		windowTotalRows = append(windowTotalRows, map[string]any{
			"minutes":       m,
			"label":         windowLabel(m),
			"credentials":   t.Credentials,
			"estimated":     t.Estimated,
			"at_risk":       t.AtRisk,
			"quota_usd":     round2(t.QuotaUSD),
			"spent_usd":     round2(t.SpentUSD),
			"remaining_usd": round2(t.RemainingUSD),
		})
	}

	sort.Slice(removed, func(i, j int) bool {
		return fmt.Sprint(removed[i]["auth_id"]) < fmt.Sprint(removed[j]["auth_id"])
	})

	priceSnapshot := a.prices.Snapshot()
	if msg, _ := priceSnapshot["last_error"].(string); msg != "" {
		warnings = append(warnings, map[string]any{"code": "price_refresh_failed", "message": msg})
	}

	// A dead model is the most urgent thing the panel can say, so its warnings
	// go in front of the accounting ones.
	models, modelWarnings := modelHealth(entries, accounts, now)
	warnings = append(modelWarnings, warnings...)
	rotatorReport := a.rot.Report(cfg.Rotator, rotatorBoard)
	if extra, _ := rotatorReport["warnings"].([]map[string]any); len(extra) > 0 {
		warnings = append(extra, warnings...)
	}

	return map[string]any{
		"generated_at":    now.UTC().Format(time.RFC3339),
		"plugin":          pluginID,
		"plugin_version":  pluginVersion,
		"price_source":    priceSnapshot["source"],
		"price_transport": priceSnapshot["transport"],
		"price_fetched":   priceSnapshot["fetched_at"],
		"data_dir":        cfg.DataDir,
		"accounts":        rows,
		"removed":         removed,
		"warnings":        warnings,
		"models":          models,
		"rotator":         rotatorReport,
		"fleet_series":    fleetSeries(accounts, now),
		"window_totals":   windowTotalRows,
		"totals": map[string]any{
			"credentials":         len(rows),
			"window_usd_observed": round4(totalWindowUSD),
			"cache_saved_usd":     round4(totalSaved),
			"requests":            totalReqs,
			"failed":              totalFailed,
		},
	}
}

type windowTotals struct {
	Minutes                          int
	Credentials, Estimated, AtRisk   int
	QuotaUSD, SpentUSD, RemainingUSD float64
}

func modelRows(a *App, w *Window) []map[string]any {
	models := make([]map[string]any, 0, len(w.ByModel))
	for name, agg := range w.ByModel {
		entry := map[string]any{
			"model":     name,
			"requests":  agg.Requests,
			"failed":    agg.Failed,
			"usd":       round4(agg.USD),
			"saved_usd": round4(agg.SavedUSD),
			"tokens":    agg.Tokens,
			"unpriced":  agg.Unpriced,
		}
		if agg.Requests > 0 {
			entry["avg_usd"] = round6(agg.USD / float64(agg.Requests))
		}
		if agg.Tokens.Input > 0 {
			entry["cache_hit_rate"] = round2(float64(agg.Tokens.CacheRead) / float64(agg.Tokens.Input) * 100)
		}
		if price, ok := a.prices.Lookup(name); ok {
			entry["price"] = price
		}
		models = append(models, entry)
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i]["usd"].(float64) > models[j]["usd"].(float64)
	})
	return models
}

func longestQuota(row map[string]any) float64 {
	longest, _ := row["longest"].(map[string]any)
	if longest == nil {
		return 0
	}
	if est, ok := longest["estimate"].(Estimate); ok {
		return est.QuotaUSD
	}
	return 0
}

func toF(v any) float64 {
	f, _ := v.(float64)
	return f
}

func toI(v any) int64 {
	i, _ := v.(int64)
	return i
}

func displayName(entries []authEntry, acct *Account) string {
	if entry, ok := lookupAuth(entries, acct); ok {
		return firstNonEmpty(entry.Label, entry.Email, entry.Name)
	}
	return acct.AuthID
}

func windowLabel(minutes int) string {
	switch {
	case minutes <= 0:
		return ""
	case minutes%(60*24) == 0:
		return strconv.Itoa(minutes/(60*24)) + "d"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "h"
	default:
		return strconv.Itoa(minutes) + "m"
	}
}

// seriesOf flattens the hourly buckets into an ascending series the dashboard
// can draw directly.
func seriesOf(acct *Account, now time.Time) []map[string]any {
	if len(acct.Hours) == 0 {
		return nil
	}
	hours := make([]int64, 0, len(acct.Hours))
	for k := range acct.Hours {
		if h, err := strconv.ParseInt(k, 10, 64); err == nil {
			hours = append(hours, h)
		}
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })

	nowHour := now.Unix() / 3600
	out := make([]map[string]any, 0, len(hours))
	cumulative := 0.0
	for _, h := range hours {
		bucket := acct.Hours[strconv.FormatInt(h, 10)]
		if bucket == nil {
			continue
		}
		cumulative += bucket.USD
		point := map[string]any{
			"ago":      nowHour - h,
			"usd":      round6(bucket.USD),
			"cum_usd":  round6(cumulative),
			"requests": bucket.Requests,
			"failed":   bucket.Failed,
		}
		if bucket.Percent > 0 {
			point["percent"] = bucket.Percent
		}
		out = append(out, point)
	}
	return out
}

// fleetSeries sums spend across every credential into one hourly series.
func fleetSeries(accounts []*Account, now time.Time) []map[string]any {
	merged := map[int64]*HourAgg{}
	for _, acct := range accounts {
		for k, bucket := range acct.Hours {
			h, err := strconv.ParseInt(k, 10, 64)
			if err != nil || bucket == nil {
				continue
			}
			target := merged[h]
			if target == nil {
				target = &HourAgg{}
				merged[h] = target
			}
			target.Requests += bucket.Requests
			target.Failed += bucket.Failed
			target.USD += bucket.USD
		}
	}
	if len(merged) == 0 {
		return nil
	}
	hours := make([]int64, 0, len(merged))
	for h := range merged {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })

	nowHour := now.Unix() / 3600
	oldest := hours[0]
	out := make([]map[string]any, 0, len(hours))
	for _, h := range hours {
		// The chart covers a week. Older buckets are still retained, because
		// the trailing totals of the points that are shown are summed out of
		// them.
		if nowHour-h >= rollingHours {
			continue
		}
		bucket := merged[h]
		// A trailing seven-day total rather than a running one. Spend
		// accumulated since the chart began only ever rises, so the line says
		// nothing except that time has passed; a rolling window is stationary,
		// so it is flat while load is steady, climbs when load actually grows,
		// and falls when it eases. Seven days because that is the window the
		// quota itself is denominated in.
		var rolling float64
		for x := h - rollingHours + 1; x <= h; x++ {
			if b := merged[x]; b != nil {
				rolling += b.USD
			}
		}
		point := map[string]any{
			"ago":         nowHour - h,
			"usd":         round6(bucket.USD),
			"requests":    bucket.Requests,
			"failed":      bucket.Failed,
			"rolling_usd": round6(rolling),
		}
		// Before this point the seven days reach back past anything retained,
		// so the total is short by an unknown amount rather than genuinely
		// lower. Saying so lets the panel draw it as provisional instead of
		// presenting a ramp that is an artefact of when recording started.
		if h-rollingHours+1 < oldest {
			point["rolling_partial"] = true
		}
		out = append(out, point)
	}
	return out
}

func parseFloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func parseInt(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ok := parseFloat(s)
		if !ok {
			return 0, false
		}
		return int64(f), true
	}
	return v, true
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func writeFileAtomic(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendEventLines(dir string, events []json.RawMessage) error {
	eventsDir := filepath.Join(dir, "events")
	if err := os.MkdirAll(eventsDir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(eventsDir, time.Now().UTC().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	var buf strings.Builder
	for _, ev := range events {
		buf.Write(ev)
		buf.WriteByte('\n')
	}
	_, err = f.WriteString(buf.String())
	return err
}

// freshEvidence chooses which calibration samples still describe the quota.
//
// They do not all describe the same thing. Measured across the fleet over nine
// days, the dollars behind one percentage point fell by about a third on every
// account independently - so evidence from last week prices a quota that is no
// longer the one being spent, and pooling it held the estimate 20-47% above
// what the current cycle was actually measuring. The old code pooled everything
// within 21 days on the reasoning that "a rollover does not change the size of
// the quota". The measurements say otherwise.
//
// So: the current cycle's own samples, and earlier cycles' only to the extent
// that this one has not yet moved enough to be divided by - newest first,
// stopping the moment there is enough. A fresh cycle still gets an estimate
// instead of a blank, and a cycle that has been running for a while is priced
// entirely on itself.
//
// A window with no reset information has no cycles to tell apart: every sample
// carries key 0, matches, and the estimate pools them exactly as before.
func (w *Window) freshEvidence() (sumDP, sumUSD float64, used int) {
	for _, s := range w.Samples {
		if s.Cycle == w.Key {
			sumDP += s.DP
			sumUSD += s.USD
			used++
		}
	}
	// Samples are appended in time order, so walking backwards is newest first.
	for i := len(w.Samples) - 1; i >= 0 && sumDP < minCycleEvidence; i-- {
		if s := w.Samples[i]; s.Cycle != w.Key {
			sumDP += s.DP
			sumUSD += s.USD
			used++
		}
	}
	return sumDP, sumUSD, used
}

// clearPending drops the unattributed spend and its model split together. They
// describe the same dollars, so anything that forgets one must forget the other.
func (w *Window) clearPending() {
	w.PendingUSD = 0
	w.PendingByModel = nil
}

func copyUSD(in map[string]float64) map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// deriveModelWeights learns how much faster one model burns a window than
// another, per dollar spent.
//
// It is measured as a ratio inside a single window, never across windows or
// accounts. That is the whole trick: two accounts have different quotas and a
// weekly window is not a five-hour one, so absolute figures are not comparable
// - but the ratio between two models measured against the same pool cancels
// the pool out, and those ratios are comparable everywhere.
//
// Fleet-wide is also the only scale at which this can be learned. A credential
// that has only ever served one model cannot price a second one from its own
// evidence, and that is exactly the credential the rotator is about to promote
// into traffic it has never seen.
//
// Caller holds a.mu.
func (a *App) deriveModelWeights() ModelWeights {
	// Every window that priced two or more models on its own evidence
	// contributes one ratio per pair.
	ratios := map[string]map[string][]float64{}
	seen := map[string]int{}
	for _, acct := range a.accounts {
		if acct == nil {
			continue
		}
		for _, w := range acct.Windows {
			if w == nil {
				continue
			}
			local := w.localQuota()
			for m := range local {
				seen[m]++
			}
			for m, qm := range local {
				for n, qn := range local {
					if m == n || qm <= 0 || qn <= 0 {
						continue
					}
					if ratios[m] == nil {
						ratios[m] = map[string][]float64{}
					}
					// quota is dollars per window, so it moves opposite to the
					// burn rate: the cheaper-per-dollar model buys more window.
					ratios[m][n] = append(ratios[m][n], qn/qm)
				}
			}
		}
	}
	// weight[m]/weight[anchor] = quota[anchor]/quota[m]: quota is dollars per
	// window, so it moves opposite to the burn rate.
	live := weightsFrom(seen, ratios)
	if len(live) >= 2 {
		return live
	}
	// Nothing has watched two models against one pool yet. The event log has,
	// and it is the same evidence read the same way.
	return a.bootWeights
}

func median(in []float64) float64 {
	if len(in) == 0 {
		return 0
	}
	cp := append([]float64(nil), in...)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

// bootstrapModelWeights recovers the model ratios from the event log.
//
// Calibration samples only started carrying a model split in this version, so
// on the day it ships no window can price a second model and the ratios cannot
// be learned - on a fleet that has already moved wholesale to one model, that
// could last for weeks. The event log has been recording the model and the
// cost of every request all along, next to the percentage upstream reported at
// the time, which is everything the ratio needs.
//
// This reads the same evidence the live path does and pairs it the same way:
// the quota header describes the state before the request it arrives with, so
// spend accumulates until the percentage moves and is then attributed to that
// move. Run once at startup; the live samples take over as they accumulate.
func (a *App) bootstrapModelWeights() ModelWeights {
	paths, err := filepath.Glob(filepath.Join(a.cfg.DataDir, "events", "*.jsonl"))
	if err != nil || len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)

	type ev struct {
		Auth   string             `json:"auth_id"`
		Model  string             `json:"model"`
		USD    float64            `json:"usd"`
		Priced bool               `json:"priced"`
		Failed bool               `json:"failed"`
		Pct    map[string]float64 `json:"-"`
	}
	// One pool is one credential's one window; nothing is ever compared across
	// pools, because two accounts have different quotas and a weekly window is
	// not a five-hour one.
	type poolKey struct{ auth, window string }
	type acc struct{ dp, usd float64 }
	pools := map[poolKey]map[string]*acc{}
	// Replay state per credential+window.
	type run struct {
		last    float64
		hasLast bool
		pend    map[string]float64
	}
	runs := map[poolKey]*run{}

	for _, path := range paths {
		fh, errOpen := os.Open(path)
		if errOpen != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var raw map[string]json.RawMessage
			if json.Unmarshal(sc.Bytes(), &raw) != nil {
				continue
			}
			var e ev
			if json.Unmarshal(sc.Bytes(), &e) != nil || !e.Priced || e.Failed || e.Model == "" {
				continue
			}
			for key, val := range raw {
				if !strings.HasPrefix(key, "pct_") {
					continue
				}
				var pct float64
				if json.Unmarshal(val, &pct) != nil {
					continue
				}
				id := poolKey{e.Auth, key}
				r := runs[id]
				if r == nil {
					r = &run{pend: map[string]float64{}}
					runs[id] = r
				}
				switch {
				case r.hasLast && pct < r.last-0.001:
					// Cycle rolled: the spend in hand can no longer be paired.
					r.hasLast, r.pend = false, map[string]float64{}
				case r.hasLast && pct > r.last:
					total := 0.0
					for _, v := range r.pend {
						total += v
					}
					if total > 0 {
						for m, v := range r.pend {
							if v/total < dominantShare {
								continue
							}
							pool := pools[id]
							if pool == nil {
								pool = map[string]*acc{}
								pools[id] = pool
							}
							if pool[m] == nil {
								pool[m] = &acc{}
							}
							pool[m].dp += pct - r.last
							pool[m].usd += total
							break
						}
					}
					r.pend = map[string]float64{}
				}
				r.last, r.hasLast = pct, true
				r.pend[e.Model] += e.USD
			}
		}
		fh.Close()
	}

	ratios := map[string]map[string][]float64{}
	seen := map[string]int{}
	for _, pool := range pools {
		local := map[string]float64{}
		for m, a := range pool {
			if a.dp >= minCycleEvidence && a.usd > 0 {
				local[m] = a.usd / (a.dp / 100)
			}
		}
		for m := range local {
			seen[m]++
		}
		for m, qm := range local {
			for n, qn := range local {
				if m == n {
					continue
				}
				if ratios[m] == nil {
					ratios[m] = map[string][]float64{}
				}
				ratios[m][n] = append(ratios[m][n], qn/qm)
			}
		}
	}
	return weightsFrom(seen, ratios)
}

// weightsFrom turns per-window model ratios into one comparable set, anchored
// on whichever model the most windows have priced.
func weightsFrom(seen map[string]int, ratios map[string]map[string][]float64) ModelWeights {
	anchor := ""
	for m, n := range seen {
		if anchor == "" || n > seen[anchor] || (n == seen[anchor] && m < anchor) {
			anchor = m
		}
	}
	if anchor == "" {
		return nil
	}
	out := ModelWeights{anchor: 1}
	for m := range seen {
		if m == anchor {
			continue
		}
		if rs := ratios[m][anchor]; len(rs) > 0 {
			out[m] = median(rs)
		}
	}
	if len(out) < 2 {
		return nil
	}
	return out
}
