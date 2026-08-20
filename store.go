package main

import (
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
	AuthID       string              `json:"AuthID"`
	AuthIndex    string              `json:"AuthIndex"`
	AuthType     string              `json:"AuthType"`
	RequestedAt  time.Time           `json:"RequestedAt"`
	Failed       bool                `json:"Failed"`
	Detail       Tokens              `json:"Detail"`
	Headers      map[string][]string `json:"ResponseHeaders"`
}

// rateLimit is the Codex quota snapshot carried on every upstream response.
type rateLimit struct {
	Percent      float64
	WindowMin    int
	ResetAt      int64
	ResetAfter   int64
	SecPercent   float64
	SecWindowMin int
	PlanType     string
	ActiveLimit  string
	Found        bool
}

// ModelAgg accumulates one model's contribution inside the current window.
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

// hourRetention bounds the series at ten days, comfortably more than the seven
// day quota window.
const hourRetention = 240

// Account is the per-credential accumulator. Everything the estimate needs
// survives a restart, so the plugin does not have to re-learn a whole window.
type Account struct {
	AuthID      string `json:"auth_id"`
	AuthIndex   string `json:"auth_index"`
	PlanType    string `json:"plan_type"`
	ActiveLimit string `json:"active_limit"`
	Provider    string `json:"provider"`

	// Latest quota snapshot from the upstream headers.
	Percent      float64 `json:"used_percent"`
	WindowMin    int     `json:"window_minutes"`
	ResetAt      int64   `json:"reset_at"`
	ObservedAt   int64   `json:"observed_at"`
	SecPercent   float64 `json:"secondary_used_percent"`
	SecWindowMin int     `json:"secondary_window_minutes"`
	HasQuota     bool    `json:"has_quota_data"`

	// Current window accounting.
	WindowKey      int64                `json:"window_key"`
	WindowStart    int64                `json:"window_start"`
	WindowFirstPct float64              `json:"window_first_percent"`
	WindowFullCov  bool                 `json:"window_full_coverage"`
	WindowUSD      float64              `json:"window_usd"`
	WindowReqs     int64                `json:"window_requests"`
	WindowFailed   int64                `json:"window_failed"`
	WindowSavedUSD float64              `json:"window_saved_usd"`
	WindowTokens   Tokens               `json:"window_tokens"`
	ByModel        map[string]*ModelAgg `json:"by_model"`

	// Hours is the rolling hourly series, keyed by unix hour as a string
	// because JSON object keys must be strings.
	Hours map[string]*HourAgg `json:"hours"`

	// Delta calibration. Kept across windows: a quota is a quota.
	LastPercent float64 `json:"last_percent"`
	HasLast     bool    `json:"has_last"`
	PendingUSD  float64 `json:"pending_usd"`
	CalUSD      float64 `json:"cal_usd"`
	CalPct      float64 `json:"cal_percent"`
	CalSamples  int     `json:"cal_samples"`

	UnpricedReqs int64    `json:"unpriced_requests"`
	UnpricedList []string `json:"unpriced_models,omitempty"`

	FirstSeen int64   `json:"first_seen"`
	TotalUSD  float64 `json:"total_usd"`
	TotalReqs int64   `json:"total_requests"`

	// resumed marks an account restored from disk whose first fresh reading has
	// not been seen yet. Deliberately not persisted.
	resumed bool
}

type stateFile struct {
	Version  int                 `json:"version"`
	SavedAt  time.Time           `json:"saved_at"`
	Accounts map[string]*Account `json:"accounts"`
}

// App owns all mutable plugin state.
type App struct {
	mu       sync.Mutex
	cfg      Config
	accounts map[string]*Account
	prices   *priceBook

	dirty      bool
	events     []json.RawMessage
	authCache  []authEntry
	authFetch  time.Time
	stopOnce   sync.Once
	stop       chan struct{}
	started    bool
	startedAt  time.Time
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
		acct = &Account{AuthID: rec.AuthID, AuthIndex: rec.AuthIndex, FirstSeen: time.Now().Unix(), ByModel: map[string]*ModelAgg{}}
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

	if acct.ByModel == nil {
		acct.ByModel = map[string]*ModelAgg{}
	}
	if rec.AuthIndex != "" {
		acct.AuthIndex = rec.AuthIndex
	}
	if rec.Provider != "" {
		acct.Provider = rec.Provider
	}

	if rl.Found {
		// A changed reset timestamp means the quota window rolled over.
		if acct.WindowKey == 0 || absInt64(rl.ResetAt-acct.WindowKey) > 120 {
			acct.startWindow(rl, now)
		}
		a.observePercent(acct, rl, now)
	}

	// Bill this request after the percent observation: the headers are emitted
	// when the upstream stream opens, so they describe the state *before* this
	// request was counted.
	acct.PendingUSD += cost
	acct.WindowUSD += cost
	acct.WindowSavedUSD += saved
	acct.WindowReqs++
	acct.WindowTokens.add(rec.Detail)
	acct.TotalUSD += cost
	acct.TotalReqs++
	if rec.Failed {
		acct.WindowFailed++
	}

	agg := acct.ByModel[model]
	if agg == nil {
		agg = &ModelAgg{}
		acct.ByModel[model] = agg
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
		acct.UnpricedReqs++
		acct.noteUnpriced(model)
	}

	acct.recordHour(now, cost, rec, rl)

	a.dirty = true
	if cfg.EventLog {
		a.appendEvent(rec, model, cost, priced, rl, now)
	}
}

// observePercent folds a fresh quota reading into the delta calibration.
//
// Between two consecutive readings the percentage moved by dp, and the money
// that moved it is exactly the spend accumulated since the previous reading.
// Summing both sides across many readings yields a quota estimate that does not
// care when the plugin was installed, which is what makes it usable on day one.
func (a *App) observePercent(acct *Account, rl rateLimit, now time.Time) {
	// First reading after a restart. Traffic served while the plugin was down is
	// inside the percentage but not inside our ledger, so anything that depends
	// on having watched every dollar has to be invalidated.
	if acct.resumed {
		acct.resumed = false
		if acct.HasLast && rl.Percent > acct.LastPercent+0.001 {
			acct.WindowFullCov = false
			// Stale pending spend would otherwise be paired with a percentage
			// step that also contains unobserved downtime spend, which would
			// quietly bias the calibration low.
			acct.PendingUSD = 0
			acct.LastPercent = rl.Percent
		}
	}

	if acct.HasLast && rl.Percent > acct.LastPercent {
		dp := rl.Percent - acct.LastPercent
		if acct.PendingUSD > 0 {
			acct.CalPct += dp
			acct.CalUSD += acct.PendingUSD
			acct.CalSamples++
		}
		acct.PendingUSD = 0
	}

	acct.LastPercent = rl.Percent
	acct.HasLast = true
	acct.Percent = rl.Percent
	acct.WindowMin = rl.WindowMin
	acct.ResetAt = rl.ResetAt
	acct.ObservedAt = now.Unix()
	acct.SecPercent = rl.SecPercent
	acct.SecWindowMin = rl.SecWindowMin
	acct.HasQuota = true
	if rl.PlanType != "" {
		acct.PlanType = rl.PlanType
	}
	if rl.ActiveLimit != "" {
		acct.ActiveLimit = rl.ActiveLimit
	}
}

// startWindow resets per-window accounting. Calibration state deliberately
// survives, because the quota it measures is a property of the plan.
func (acct *Account) startWindow(rl rateLimit, now time.Time) {
	acct.WindowKey = rl.ResetAt
	acct.WindowMin = rl.WindowMin
	if rl.WindowMin > 0 && rl.ResetAt > 0 {
		acct.WindowStart = rl.ResetAt - int64(rl.WindowMin)*60
	} else {
		acct.WindowStart = now.Unix()
	}
	acct.WindowFirstPct = rl.Percent
	// Full coverage means the first reading of this window showed nothing
	// spent, so every dollar since then is money this plugin actually saw.
	acct.WindowFullCov = rl.Percent <= 0
	acct.WindowUSD = 0
	acct.WindowReqs = 0
	acct.WindowFailed = 0
	acct.WindowSavedUSD = 0
	acct.WindowTokens = Tokens{}
	acct.ByModel = map[string]*ModelAgg{}
	// Hours is a rolling time series, not window state, so it is not reset.
	// Pending spend belonged to the window that just closed and can no longer
	// be attributed to a percentage move, so it is dropped rather than mixed in.
	acct.PendingUSD = 0
	acct.HasLast = false
	acct.LastPercent = 0
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
	}
	bucket.Requests++
	bucket.USD += cost
	bucket.In += rec.Detail.Input
	bucket.Out += rec.Detail.Output
	bucket.CacheRead += rec.Detail.CacheRead
	if rec.Failed {
		bucket.Failed++
	}
	if rl.Found {
		bucket.Percent = rl.Percent
	}

	if len(acct.Hours) > hourRetention+24 {
		cutoff := hour - hourRetention
		for k := range acct.Hours {
			if h, err := strconv.ParseInt(k, 10, 64); err == nil && h < cutoff {
				delete(acct.Hours, k)
			}
		}
	}
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

// Estimate is the derived view of one account.
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
}

func (acct *Account) Estimate() Estimate {
	var e Estimate

	// Spend that the latest percentage reading has actually had a chance to
	// account for. PendingUSD is everything billed after that reading, so
	// dividing the raw window total by the percentage would overstate the quota.
	e.AttributedUSD = acct.WindowUSD - acct.PendingUSD
	if e.AttributedUSD < 0 {
		e.AttributedUSD = 0
	}

	if acct.CalPct > 0 && acct.CalUSD > 0 {
		e.QuotaByDelta = acct.CalUSD / (acct.CalPct / 100)
	}
	if acct.WindowFullCov && acct.Percent > 0 && e.AttributedUSD > 0 {
		e.QuotaByWindow = e.AttributedUSD / (acct.Percent / 100)
	}

	// A fully observed window is the stronger measurement because it prices the
	// whole window against the whole percentage, with no attribution guesswork.
	switch {
	case e.QuotaByWindow > 0 && acct.Percent >= 3:
		e.QuotaUSD, e.Method = e.QuotaByWindow, "window"
		e.Evidence = acct.Percent
	case e.QuotaByDelta > 0:
		e.QuotaUSD, e.Method = e.QuotaByDelta, "delta"
		e.Evidence = acct.CalPct
	case e.QuotaByWindow > 0:
		e.QuotaUSD, e.Method = e.QuotaByWindow, "window"
		e.Evidence = acct.Percent
	default:
		e.Method = "none"
	}

	if e.QuotaUSD > 0 {
		e.SpentUSD = e.QuotaUSD * acct.Percent / 100
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
	return e
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

	pct, okPct := parseFloat(get("x-codex-primary-used-percent"))
	win, okWin := parseInt(get("x-codex-primary-window-minutes"))
	if !okPct || !okWin || win <= 0 {
		return rl
	}
	rl.Found = true
	rl.Percent = clampPercent(pct)
	rl.WindowMin = int(win)
	if v, ok := parseInt(get("x-codex-primary-reset-at")); ok {
		rl.ResetAt = v
	}
	if v, ok := parseInt(get("x-codex-primary-reset-after-seconds")); ok {
		rl.ResetAfter = v
		if rl.ResetAt == 0 {
			rl.ResetAt = time.Now().Unix() + v
		}
	}
	if v, ok := parseFloat(get("x-codex-secondary-used-percent")); ok {
		rl.SecPercent = clampPercent(v)
	}
	if v, ok := parseInt(get("x-codex-secondary-window-minutes")); ok {
		rl.SecWindowMin = int(v)
	}
	rl.PlanType = get("x-codex-plan-type")
	rl.ActiveLimit = get("x-codex-active-limit")
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
	for k, v := range sf.Accounts {
		if v == nil {
			continue
		}
		if v.ByModel == nil {
			v.ByModel = map[string]*ModelAgg{}
		}
		v.resumed = true
		a.accounts[k] = v
	}
}

// Flush writes the state snapshot and drains the buffered event log.
func (a *App) Flush() {
	a.mu.Lock()
	if !a.dirty && len(a.events) == 0 {
		a.mu.Unlock()
		return
	}
	snapshot := stateFile{Version: 1, SavedAt: time.Now().UTC(), Accounts: make(map[string]*Account, len(a.accounts))}
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
		"ts":       now.UTC().Format(time.RFC3339),
		"auth_id":  rec.AuthID,
		"model":    model,
		"usd":      round6(cost),
		"priced":   priced,
		"failed":   rec.Failed,
		"in":       rec.Detail.Input,
		"out":      rec.Detail.Output,
		"reason":   rec.Detail.Reasoning,
		"cache_r":  rec.Detail.CacheRead,
		"cache_w":  rec.Detail.CacheWrite,
	}
	if rl.Found {
		ev["pct"] = rl.Percent
		ev["reset_at"] = rl.ResetAt
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

func (a *App) lookupAuth(acct *Account) (authEntry, bool) {
	for _, entry := range a.authMetadata() {
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
func (a *App) Report() map[string]any {
	now := time.Now()

	a.mu.Lock()
	accounts := make([]*Account, 0, len(a.accounts))
	for _, acct := range a.accounts {
		accounts = append(accounts, acct)
	}
	cfg := a.cfg
	a.mu.Unlock()

	rows := make([]map[string]any, 0, len(accounts))
	var totalQuota, totalSpent, totalRemaining, totalWindowUSD, totalSaved float64
	var totalReqs, totalFailed int64
	estimated, atRisk := 0, 0
	warnings := make([]string, 0)

	for _, acct := range accounts {
		est := acct.Estimate()
		if est.Method != "none" {
			estimated++
		}

		row := map[string]any{
			"auth_id":                acct.AuthID,
			"provider":               acct.Provider,
			"plan_type":              acct.PlanType,
			"active_limit":           acct.ActiveLimit,
			"has_quota_data":         acct.HasQuota,
			"used_percent":           round2(acct.Percent),
			"secondary_used_percent": round2(acct.SecPercent),
			"window_minutes":         acct.WindowMin,
			"window_label":           windowLabel(acct.WindowMin),
			"window_usd_observed":    round4(acct.WindowUSD),
			"window_requests":        acct.WindowReqs,
			"window_failed":          acct.WindowFailed,
			"window_saved_usd":       round4(acct.WindowSavedUSD),
			"window_tokens":          acct.WindowTokens,
			"window_full_coverage":   acct.WindowFullCov,
			"total_usd":              round4(acct.TotalUSD),
			"total_requests":         acct.TotalReqs,
			"unpriced_requests":      acct.UnpricedReqs,
			"estimate":               est,
			"calibration": map[string]any{
				"samples": acct.CalSamples,
				"percent": round2(acct.CalPct),
				"usd":     round4(acct.CalUSD),
			},
		}
		if len(acct.UnpricedList) > 0 {
			row["unpriced_models"] = acct.UnpricedList
			warnings = append(warnings, fmt.Sprintf("no public price for %s (credential %s); its spend is missing from the estimate", strings.Join(acct.UnpricedList, ", "), displayName(acct, a)))
		}

		if entry, ok := a.lookupAuth(acct); ok {
			row["label"] = firstNonEmpty(entry.Label, entry.Email, entry.Name)
			row["email"] = entry.Email
			row["disabled"] = entry.Disabled
			row["status"] = entry.Status
			row["unavailable"] = entry.Unavailable
		} else {
			row["label"] = acct.AuthID
		}

		if acct.ResetAt > 0 {
			reset := time.Unix(acct.ResetAt, 0)
			row["reset_at"] = reset.UTC().Format(time.RFC3339)
			row["reset_in_seconds"] = int64(math.Max(0, time.Until(reset).Seconds()))
		}
		if acct.ObservedAt > 0 {
			row["observed_at"] = time.Unix(acct.ObservedAt, 0).UTC().Format(time.RFC3339)
			row["observed_age_seconds"] = int64(now.Sub(time.Unix(acct.ObservedAt, 0)).Seconds())
		}

		// How far through the window the clock is. Comparing it against the
		// percentage consumed is the fastest way to see who is burning too fast.
		if acct.WindowStart > 0 && acct.WindowMin > 0 {
			total := float64(acct.WindowMin) * 60
			elapsedSec := now.Sub(time.Unix(acct.WindowStart, 0)).Seconds()
			timePct := clampPercent(elapsedSec / total * 100)
			row["time_progress_percent"] = round2(timePct)
			if timePct > 1 {
				// >1 means the quota is being spent faster than the clock runs.
				row["pace_ratio"] = round2(acct.Percent / timePct)
			}
		}

		// Burn rate and runway, derived from the portion of the window elapsed.
		if est.QuotaUSD > 0 && acct.WindowStart > 0 && acct.WindowMin > 0 {
			elapsed := now.Sub(time.Unix(acct.WindowStart, 0)).Hours()
			if elapsed > 0.25 {
				perDay := est.SpentUSD / (elapsed / 24)
				row["burn_usd_per_day"] = round2(perDay)
				if perDay > 0 {
					daysLeft := est.RemainingUSD / perDay
					row["runway_days"] = round2(daysLeft)
					windowDaysLeft := time.Until(time.Unix(acct.ResetAt, 0)).Hours() / 24
					row["will_exhaust_before_reset"] = daysLeft < windowDaysLeft
				}
			}
		}

		if acct.WindowReqs > 0 {
			row["avg_usd_per_request"] = round6(acct.WindowUSD / float64(acct.WindowReqs))
			row["failure_rate"] = round2(float64(acct.WindowFailed) / float64(acct.WindowReqs) * 100)
		}
		if acct.WindowTokens.Input > 0 {
			row["cache_hit_rate"] = round2(float64(acct.WindowTokens.CacheRead) / float64(acct.WindowTokens.Input) * 100)
		}
		row["series"] = seriesOf(acct, now)

		models := make([]map[string]any, 0, len(acct.ByModel))
		for name, agg := range acct.ByModel {
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
		row["by_model"] = models

		totalQuota += est.QuotaUSD
		totalSpent += est.SpentUSD
		totalRemaining += est.RemainingUSD
		totalWindowUSD += acct.WindowUSD
		totalSaved += acct.WindowSavedUSD
		totalReqs += acct.WindowReqs
		totalFailed += acct.WindowFailed
		if est.QuotaUSD > 0 && acct.Percent >= 90 {
			atRisk++
		}
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		return quotaOf(rows[i]) > quotaOf(rows[j])
	})

	priceSnapshot := a.prices.Snapshot()
	if msg, _ := priceSnapshot["last_error"].(string); msg != "" {
		warnings = append(warnings, "price catalog refresh is failing: "+msg)
	}

	return map[string]any{
		"generated_at":   now.UTC().Format(time.RFC3339),
		"plugin":         pluginID,
		"plugin_version": pluginVersion,
		"price_source":    priceSnapshot["source"],
		"price_transport": priceSnapshot["transport"],
		"price_fetched":   priceSnapshot["fetched_at"],
		"data_dir":       cfg.DataDir,
		"accounts":       rows,
		"warnings":       warnings,
		"fleet_series":   fleetSeries(accounts, now),
		"totals": map[string]any{
			"credentials":         len(rows),
			"estimated":           estimated,
			"at_risk":             atRisk,
			"quota_usd":           round2(totalQuota),
			"spent_usd":           round2(totalSpent),
			"remaining_usd":       round2(totalRemaining),
			"window_usd_observed": round4(totalWindowUSD),
			"cache_saved_usd":     round4(totalSaved),
			"window_requests":     totalReqs,
			"window_failed":       totalFailed,
		},
	}
}

// seriesOf flattens the hourly buckets into an ascending series the dashboard
// can draw directly: hours ago, spend in that hour, cumulative spend, and the
// quota percentage as last seen in that hour.
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
	out := make([]map[string]any, 0, len(hours))
	for _, h := range hours {
		bucket := merged[h]
		out = append(out, map[string]any{
			"ago":      nowHour - h,
			"usd":      round6(bucket.USD),
			"requests": bucket.Requests,
			"failed":   bucket.Failed,
		})
	}
	return out
}

func quotaOf(row map[string]any) float64 {
	if est, ok := row["estimate"].(Estimate); ok {
		return est.QuotaUSD
	}
	return 0
}

func displayName(acct *Account, a *App) string {
	if entry, ok := a.lookupAuth(acct); ok {
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
