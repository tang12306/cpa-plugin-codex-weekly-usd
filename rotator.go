package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// The rotator keeps a small pool of credentials enabled and replaces a member
// before it runs out, rather than after.
//
// Why a pool rather than a single credential: the proxy's fill-first selector
// uses the first available credential and, when one is refused, retries the
// same request on the next candidate. So a second enabled credential makes the
// handover invisible to callers - the switch costs nothing as long as the
// replacement is already in the pool when the incumbent runs dry.
//
// Three facts from upstream shape the rules here, all of them measured rather
// than assumed:
//
//   - A minimal request is a free quota reading. Two back to back both report
//     the same percentage, so probing costs nothing worth counting - but the
//     reading only exists on a request that upstream accepts. A rejected one
//     (bad model, malformed body) carries no quota headers at all.
//   - A refusal still carries them. A 429 reports the percentages that caused
//     it, so an exhausted credential is readable rather than a blind spot.
//   - A disabled credential is not frozen. Credentials disabled here have been
//     observed consuming quota anyway, which means something outside this proxy
//     uses them. Stored percentages are therefore evidence, not fact, and a
//     switch decision is made on a fresh reading.

const (
	probeURL       = "https://chatgpt.com/backend-api/codex/responses"
	probeUserAgent = "codex_cli_rs/0.144.1"
	// switchLogLimit bounds the audit trail kept in state.json.
	switchLogLimit = 100
	// probeMinRelief treats a window that refills within this long as available:
	// a five-hour window that resets in two minutes is not a reason to reject a
	// candidate.
	probeMinRelief = 5 * time.Minute
)

// Reasons a candidate was passed over, reported on the panel so a decision can
// be argued with rather than just trusted.
const (
	skipDeadToken   = "dead_token"
	skipExhausted   = "exhausted"
	skipTooLow      = "below_floor"
	skipProbeFailed = "probe_failed"
	skipExcluded    = "excluded_by_config"
	skipRecent      = "recently_rotated"
)

// probeResult is one credential's standing, read straight from upstream.
type probeResult struct {
	File       string          `json:"file"`
	Index      string          `json:"auth_index"`
	At         int64           `json:"at"`
	StatusCode int             `json:"status_code"`
	Windows    []windowReading `json:"-"`
	PlanType   string          `json:"plan_type,omitempty"`
	Credits    creditInfo      `json:"credits"`
	Error      string          `json:"error,omitempty"`
}

// alive reports whether this reading shows a credential that can serve. A 429
// is alive - a working credential with no quota left, which is a completely
// different thing from a broken one.
func (p probeResult) alive() bool {
	return p.Error == "" && !p.rejected()
}

// rejected is the narrower question: did upstream actually refuse the
// credential? Only a 401 or 403 answers yes. alive() is false for a probe that
// merely failed to complete, and retiring a credential on that would let one
// network blip cost real capacity - so retirement uses this, not alive().
func (p probeResult) rejected() bool {
	return p.StatusCode == 401 || p.StatusCode == 403
}

// candidate is one credential scored for the pool.
type candidate struct {
	File      string  `json:"file"`
	Index     string  `json:"auth_index"`
	Label     string  `json:"credential"`
	Enabled   bool    `json:"enabled"`
	Headroom  float64 `json:"headroom_percent"`
	Binding   int     `json:"binding_window_minutes,omitempty"`
	ExpiresIn float64 `json:"binding_reset_in_hours,omitempty"`
	ServiceH  float64 `json:"service_hours,omitempty"`
	Safe      bool    `json:"safe"`
	Skip      string  `json:"skipped,omitempty"`
	Status    int     `json:"status_code,omitempty"`
	Plan      string  `json:"plan_type,omitempty"`
}

type switchRecord struct {
	At     int64  `json:"at"`
	Action string `json:"action"`
	File   string `json:"file"`
	Reason string `json:"reason"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// RotatorState is the part of the rotator that outlives a restart.
type RotatorState struct {
	LastSweep   int64          `json:"last_sweep,omitempty"`
	LastSwitch  int64          `json:"last_switch,omitempty"`
	DayKey      string         `json:"day_key,omitempty"`
	DayCount    int            `json:"day_count,omitempty"`
	Log         []switchRecord `json:"log,omitempty"`
	LastReason  string         `json:"last_reason,omitempty"`
	LastCandids []candidate    `json:"last_candidates,omitempty"`
	// HealthyEnabled and RecoversAt describe the last shortfall: how many pool
	// members could still serve, and when the first passed-over candidate is due
	// back. They are what separates "degraded" from "down".
	HealthyEnabled int   `json:"healthy_enabled,omitempty"`
	RecoversAt     int64 `json:"recovers_at,omitempty"`
}

type rotator struct {
	app *App

	mu    sync.Mutex
	state RotatorState
	// probes caches the newest reading per credential file, from a probe or
	// from live traffic.
	probes map[string]probeResult
}

func newRotator(app *App) *rotator {
	return &rotator{app: app, probes: map[string]probeResult{}}
}

func (r *rotator) loop(stop <-chan struct{}) {
	defer func() {
		if rec := recover(); rec != nil {
			hostLog("warn", fmt.Sprintf("rotator recovered: %v", rec))
		}
	}()

	cfg := r.config()
	// One sweep at startup answers "where does the fleet actually stand", which
	// is otherwise unknowable for a credential no traffic has touched.
	time.Sleep(20 * time.Second)
	r.tick(true)

	ticker := time.NewTicker(time.Duration(cfg.CheckIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			r.tick(false)
		}
	}
}

func (r *rotator) config() RotatorConfig {
	r.app.mu.Lock()
	defer r.app.mu.Unlock()
	return r.app.cfg.Rotator
}

// tick is the whole decision. It is deliberately cheap when nothing is wrong:
// no upstream request is made until the pool is actually short of a healthy
// member, which is what "only probe when something is running out" means.
func (r *rotator) tick(force bool) {
	defer func() {
		if rec := recover(); rec != nil {
			hostLog("warn", fmt.Sprintf("rotator tick recovered: %v", rec))
		}
	}()

	cfg := r.config()
	if !cfg.Enabled {
		return
	}
	entries := r.app.authMetadata()
	if len(entries) == 0 {
		return
	}

	enabled, standby := splitPool(entries, cfg)
	short, why := r.poolShortfall(enabled, cfg)
	if !force && short <= 0 {
		r.note(why)
		return
	}

	// The circuit breaker is honoured even when a human pressed the button: it
	// exists to stop the rotator doing damage quickly, and a manual trigger is
	// not evidence that this time is different.
	now := time.Now()
	if !r.mayAct(now, cfg) {
		return
	}

	pool := append(append([]authEntry{}, enabled...), standby...)
	results := r.sweep(pool, cfg)
	candidates := r.rank(results, pool, cfg)

	r.mu.Lock()
	r.state.LastSweep = now.Unix()
	r.state.LastCandids = candidates
	r.mu.Unlock()

	// A credential upstream has refused contributes nothing, so switching it off
	// cannot reduce capacity - and it has to happen here rather than after a
	// promotion, because the "nothing qualifies" path below returns early and
	// would otherwise leave a refused credential in the pool indefinitely,
	// costing a wasted attempt on every request.
	touched := map[string]bool{}
	if cfg.DisableDeadTokens {
		touched = r.retireRejected(results, pool, cfg)
	}

	// Recount with fresh readings: the cached picture that triggered the sweep
	// may have been stale, and a sweep that finds everything healthy should end
	// without touching anything.
	enabled, standby = splitPool(r.app.authMetadata(), cfg)
	short, why = r.poolShortfall(enabled, cfg)
	if short <= 0 {
		r.note(why)
		return
	}

	picks := make([]candidate, 0, short)
	for _, c := range candidates {
		if len(picks) >= short {
			break
		}
		if c.Enabled || c.Skip != "" {
			continue
		}
		picks = append(picks, c)
	}
	if len(picks) == 0 {
		// Never empty the pool because there is nothing to replace it with.
		// Whatever is enabled stays enabled, and the panel says why.
		healthy := cfg.KeepEnabled - short
		soonest := soonestRecovery(candidates)
		r.mu.Lock()
		r.state.HealthyEnabled = healthy
		r.state.RecoversAt = soonest
		r.mu.Unlock()

		// Being one short of the target while still serving is not the same
		// situation as having nothing that can serve, and logging them the same
		// way trains the operator to ignore both.
		reason := "no_standby_available"
		if healthy <= 0 {
			reason = "pool_has_nothing_serving"
		}
		if r.note(reason) {
			msg := fmt.Sprintf("rotator: pool is %d short of %d and no standby qualifies; leaving it untouched",
				short, cfg.KeepEnabled)
			if soonest > 0 {
				msg += fmt.Sprintf(" (soonest candidate recovers in %s)", time.Until(time.Unix(soonest, 0)).Truncate(time.Minute))
			}
			if healthy <= 0 {
				hostLog("error", msg+" - NOTHING in the pool can serve")
			} else {
				hostLog("info", msg+fmt.Sprintf(" - %d member(s) still serving", healthy))
			}
		}
		return
	}

	// Enable before disabling, always: a moment with an empty pool is a total
	// outage, and there is no ordering where disabling first is safer.
	promoted := make([]candidate, 0, len(picks))
	for _, c := range picks {
		if err := r.setDisabled(c, false, "promoted: "+r.pickReason(c), cfg); err != nil {
			hostLog("warn", "rotator: could not enable "+c.File+": "+err.Error())
			continue
		}
		promoted = append(promoted, c)
	}
	if len(promoted) == 0 {
		r.note("promotion_failed")
		return
	}

	r.retireDrained(enabled, cfg, len(promoted), touched)
	r.note(fmt.Sprintf("rotated: promoted %d", len(promoted)))
	r.mu.Lock()
	r.state.LastSwitch = now.Unix()
	r.mu.Unlock()
}

// poolShortfall counts how many healthy members the pool is missing.
func (r *rotator) poolShortfall(enabled []authEntry, cfg RotatorConfig) (int, string) {
	healthy := 0
	for _, e := range enabled {
		if state, _ := r.memberState(e, cfg); state == "healthy" {
			healthy++
		}
	}
	if healthy >= cfg.KeepEnabled {
		return 0, "pool_healthy"
	}
	if len(enabled) == 0 {
		return cfg.KeepEnabled, "pool_empty"
	}
	return cfg.KeepEnabled - healthy, "pool_short"
}

// windowView is one window of one credential as the rotator sees it, with the
// burn rate that belongs to that window and no other. Pairing one window's
// headroom with another window's rate produces a projection wrong by more than
// an order of magnitude: a weekly allowance and a five-hour one move at
// completely different speeds against the same traffic.
type windowView struct {
	Minutes   int
	Headroom  float64
	ReliefMin float64 // minutes until this window resets; negative when unknown
	Burn      float64 // percentage points per hour
}

func (r *rotator) viewOf(e authEntry) []windowView {
	rates := r.app.burnRates(e)
	windows := r.windowsOf(e)
	out := make([]windowView, 0, len(windows))
	for _, w := range windows {
		v := windowView{Minutes: w.Minutes, Headroom: 100 - w.Percent, ReliefMin: -1, Burn: rates[w.Minutes]}
		if w.ResetAt > 0 {
			mins := time.Until(time.Unix(w.ResetAt, 0)).Minutes()
			if mins <= 0 {
				// The reading describes a cycle that has since closed, so the
				// allowance is back whatever the stored percentage says.
				v.Headroom, v.ReliefMin = 100, 0
			} else {
				v.ReliefMin = mins
			}
		}
		out = append(out, v)
	}
	return out
}

// memberState judges one enabled credential from whatever reading is newest -
// live traffic if it has been serving, otherwise the last probe.
func (r *rotator) memberState(e authEntry, cfg RotatorConfig) (string, float64) {
	// A credential upstream has stopped accepting contributes nothing, and it
	// has no windows to judge, so without this it would read as healthy and the
	// pool would never look short enough to replace it.
	r.mu.Lock()
	probe, probed := r.probes[e.Name]
	r.mu.Unlock()
	if probed && !probe.alive() {
		return "dead", 0
	}

	views := r.viewOf(e)
	if len(views) == 0 {
		// Never observed and never probed. Treated as healthy so an idle
		// standby does not trigger a sweep on its own; a real shortfall shows
		// up as soon as it starts serving.
		return "healthy", 100
	}

	worst := 100.0
	draining := false
	for _, v := range views {
		if v.Headroom < worst {
			worst = v.Headroom
		}
		if v.Headroom <= 0 {
			// An empty window that refills in a moment is not an outage.
			if v.ReliefMin >= 0 && v.ReliefMin <= probeMinRelief.Minutes() {
				continue
			}
			return "exhausted", v.Headroom
		}
		if v.Headroom <= cfg.SwitchAtPercent {
			draining = true
			continue
		}
		if v.Burn <= 0 {
			continue
		}
		// Project the current burn forward. Without this a five-minute check
		// steps straight over the floor: a credential seen today went from 84%
		// to 99% inside forty-five minutes, which is less than two checks of
		// warning.
		minutesLeft := v.Headroom / v.Burn * 60
		if minutesLeft >= float64(cfg.LeadTimeMinutes) {
			continue
		}
		// A window that refills before it empties needs no replacement:
		// switching then throws away the tail of a credential that was going to
		// recover on its own.
		if v.ReliefMin >= 0 && v.ReliefMin < minutesLeft {
			continue
		}
		draining = true
	}
	if draining {
		return "draining", worst
	}
	return "healthy", worst
}

// windowsOf prefers a fresh probe and falls back to what live traffic recorded.
func (r *rotator) windowsOf(e authEntry) []windowReading {
	r.mu.Lock()
	probe, ok := r.probes[e.Name]
	r.mu.Unlock()

	fromTraffic, trafficAt := r.app.observedWindows(e)
	if ok && len(probe.Windows) > 0 && (trafficAt == 0 || probe.At >= trafficAt) {
		return probe.Windows
	}
	return fromTraffic
}

// sweep probes every credential once. Errors are recorded rather than raised:
// one unreachable account must not stop the rotation.
func (r *rotator) sweep(entries []authEntry, cfg RotatorConfig) map[string]probeResult {
	out := make(map[string]probeResult, len(entries))
	for _, e := range entries {
		res := r.probe(e, cfg)
		out[e.Name] = res
		r.mu.Lock()
		r.probes[e.Name] = res
		r.mu.Unlock()
	}
	return out
}

// probe asks upstream for one credential's quota. The request is the smallest
// accepted shape: upstream only reports quota on a request it accepts, so there
// is no cheaper reading to be had.
func (r *rotator) probe(e authEntry, cfg RotatorConfig) probeResult {
	res := probeResult{File: e.Name, Index: e.AuthIndex, At: time.Now().Unix()}

	token, account, err := r.credential(e)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	body, _ := json.Marshal(map[string]any{
		"model":  cfg.ProbeModel,
		"store":  false,
		"stream": true,
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": "hi"}},
		}},
	})
	headers := map[string][]string{
		"authorization":       {"Bearer " + token},
		"chatgpt-account-id":  {account},
		"openai-beta":         {"responses=experimental"},
		"originator":          {"codex_cli_rs"},
		"user-agent":          {probeUserAgent},
		"content-type":        {"application/json"},
	}
	status, respHeaders, err := hostHTTPDo("POST", probeURL, headers, body)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.StatusCode = status

	rl := parseRateLimit(respHeaders)
	res.Windows = rl.Windows
	res.PlanType = rl.PlanType
	res.Credits = rl.Credits
	if !rl.Found && status/100 == 2 {
		// A 2xx with no quota headers means upstream changed shape; say so
		// rather than treating an unknown as full.
		res.Error = "no quota headers on a successful probe"
	}
	return res
}

// credential pulls the access token out of the auth file through the host.
func (r *rotator) credential(e authEntry) (token, account string, err error) {
	if strings.TrimSpace(e.AuthIndex) == "" {
		return "", "", fmt.Errorf("no auth index for %s", e.Name)
	}
	payload, _ := json.Marshal(map[string]any{"auth_index": e.AuthIndex})
	result, errCall := hostCall("host.auth.get", payload)
	if errCall != nil {
		return "", "", errCall
	}
	var resp struct {
		JSON json.RawMessage `json:"json"`
	}
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return "", "", errUnmarshal
	}
	var doc struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	}
	if errUnmarshal := json.Unmarshal(resp.JSON, &doc); errUnmarshal != nil {
		return "", "", errUnmarshal
	}
	if doc.AccessToken == "" {
		return "", "", fmt.Errorf("no access token in %s", e.Name)
	}
	return doc.AccessToken, doc.AccountID, nil
}

// rank scores every credential and explains every rejection.
func (r *rotator) rank(results map[string]probeResult, entries []authEntry, cfg RotatorConfig) []candidate {
	now := time.Now()
	usdPerHour := r.app.fleetUSDPerHour()
	out := make([]candidate, 0, len(entries))

	for _, e := range entries {
		res, probed := results[e.Name]
		c := candidate{
			File:    e.Name,
			Index:   e.AuthIndex,
			Label:   firstNonEmpty(e.Label, e.Email, e.Name),
			Enabled: !e.Disabled,
			Plan:    res.PlanType,
			Status:  res.StatusCode,
		}
		switch {
		case contains(cfg.NeverEnable, e.Name):
			c.Skip = skipExcluded
		case !probed:
			c.Skip = skipProbeFailed
		case res.Error != "" && !res.alive():
			c.Skip = skipDeadToken
		case !res.alive():
			c.Skip = skipDeadToken
		case res.Error != "":
			c.Skip = skipProbeFailed
		}

		if len(res.Windows) > 0 {
			c.Headroom, c.Binding, c.ExpiresIn = bindingWindow(res.Windows, now)
			c.ServiceH = serviceHours(r.app, e, c, usdPerHour)
			c.Safe = c.ServiceH >= cfg.HorizonHours ||
				(c.ExpiresIn > 0 && c.ExpiresIn <= c.ServiceH)
		}
		if c.Skip == "" {
			switch {
			case c.Headroom <= 0 && !reliefSoon(res.Windows, now):
				c.Skip = skipExhausted
			case c.Headroom <= cfg.SwitchAtPercent:
				// Promoting something already at the floor buys one tick of
				// relief and then asks for another switch.
				c.Skip = skipTooLow
			case r.rotatedRecently(e.Name, res.Windows, now, cfg):
				c.Skip = skipRecent
			}
		}
		out = append(out, c)
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Skip == "") != (b.Skip == "") {
			return a.Skip == ""
		}
		if a.Safe != b.Safe {
			return a.Safe
		}
		if a.Safe && b.Safe {
			// Both will carry the horizon, so prefer the quota that would
			// otherwise be thrown away first: an allowance resetting in nine
			// hours is use-it-or-lose-it, one resetting in a week is not.
			if a.ExpiresIn != b.ExpiresIn {
				return a.ExpiresIn < b.ExpiresIn
			}
		}
		if a.ServiceH != b.ServiceH {
			return a.ServiceH > b.ServiceH
		}
		// No dollar estimate for either - a fresh install, or credentials no
		// traffic has priced yet. Headroom is a worse measure because a percent
		// is worth different money on different plans, but it beats leaving the
		// order to chance.
		return a.Headroom > b.Headroom
	})
	return out
}

func (r *rotator) pickReason(c candidate) string {
	if c.Safe {
		return fmt.Sprintf("headroom %.0f%%, covers %.1fh, binding window resets in %.1fh",
			c.Headroom, c.ServiceH, c.ExpiresIn)
	}
	return fmt.Sprintf("headroom %.0f%%, best available (covers %.1fh)", c.Headroom, c.ServiceH)
}

// rotatedRecently keeps the rotator from handing work back to a credential it
// just took it away from, unless that credential's window has since reset.
func (r *rotator) rotatedRecently(file string, windows []windowReading, now time.Time, cfg RotatorConfig) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.Add(-time.Duration(cfg.MinSwitchGapMinutes) * time.Minute).Unix()
	for i := len(r.state.Log) - 1; i >= 0; i-- {
		rec := r.state.Log[i]
		if rec.At < cutoff {
			break
		}
		if rec.File == file && rec.Action == "disable" {
			for _, w := range windows {
				if w.ResetAt > 0 && time.Unix(w.ResetAt, 0).After(now) && w.Percent < 1 {
					return false // it reset; it is a fresh credential again
				}
			}
			return true
		}
	}
	return false
}

// retireRejected disables credentials upstream has refused outright. Leaving
// one enabled costs a wasted attempt on every request, and disabling is the
// safe direction: it removes capacity that was not working anyway.
// The returned set is what this tick has already switched off. Re-reading the
// credential list is not enough to know that: the host applies a write through
// a file watcher, so for a moment after the write it still reports the old
// state, and a recount taken in that window would switch the same credential
// off a second time.
func (r *rotator) retireRejected(results map[string]probeResult, entries []authEntry, cfg RotatorConfig) map[string]bool {
	touched := map[string]bool{}
	for _, e := range entries {
		if e.Disabled || contains(cfg.NeverDisable, e.Name) {
			continue
		}
		res, ok := results[e.Name]
		if !ok || !res.rejected() {
			continue
		}
		c := candidate{File: e.Name, Index: e.AuthIndex, Label: firstNonEmpty(e.Label, e.Email, e.Name)}
		if err := r.setDisabled(c, true, fmt.Sprintf("token rejected upstream (HTTP %d)", res.StatusCode), cfg); err != nil {
			hostLog("warn", "rotator: could not disable rejected credential "+e.Name+": "+err.Error())
			continue
		}
		touched[e.Name] = true
	}
	return touched
}

// retireDrained removes spent members once replacements are in place, never
// dropping the pool below its target and never removing the last healthy one.
func (r *rotator) retireDrained(enabled []authEntry, cfg RotatorConfig, promoted int, touched map[string]bool) {
	live := promoted
	for _, e := range enabled {
		if !touched[e.Name] {
			live++
		}
	}
	for _, e := range enabled {
		if contains(cfg.NeverDisable, e.Name) || touched[e.Name] {
			continue
		}
		state, headroom := r.memberState(e, cfg)
		if state == "healthy" {
			continue
		}
		if live <= cfg.KeepEnabled {
			return
		}
		c := candidate{File: e.Name, Index: e.AuthIndex, Label: firstNonEmpty(e.Label, e.Email, e.Name)}
		reason := fmt.Sprintf("drained: %.0f%% headroom left", headroom)
		if err := r.setDisabled(c, true, reason, cfg); err != nil {
			hostLog("warn", "rotator: could not disable "+e.Name+": "+err.Error())
			continue
		}
		live--
	}
}

// setDisabled flips one credential's disabled flag through the host, preserving
// every other field byte for byte: these files hold refresh tokens, and a lossy
// round trip through a typed struct would quietly drop whatever this plugin
// does not know about.
func (r *rotator) setDisabled(c candidate, disabled bool, reason string, cfg RotatorConfig) error {
	action := "enable"
	if disabled {
		action = "disable"
	}
	r.record(switchRecord{At: time.Now().Unix(), Action: action, File: c.File, Reason: reason, DryRun: cfg.DryRun})
	hostLog("info", fmt.Sprintf("rotator: %s %s (%s)%s", action, c.File, reason, dryRunSuffix(cfg.DryRun)))
	if cfg.DryRun {
		return nil
	}
	if strings.TrimSpace(c.Index) == "" {
		return fmt.Errorf("no auth index for %s", c.File)
	}

	payload, _ := json.Marshal(map[string]any{"auth_index": c.Index})
	result, err := hostCall("host.auth.get", payload)
	if err != nil {
		return err
	}
	var got struct {
		Name string          `json:"name"`
		JSON json.RawMessage `json:"json"`
	}
	if err = json.Unmarshal(result, &got); err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(got.JSON, &doc); err != nil {
		return err
	}
	if doc == nil {
		return fmt.Errorf("empty credential document for %s", c.File)
	}
	if _, hasToken := doc["refresh_token"]; !hasToken {
		// A document without the field that makes it a credential is not one
		// this plugin is going to write back.
		return fmt.Errorf("refusing to write %s: not a credential document", c.File)
	}
	doc["disabled"] = json.RawMessage(fmt.Sprintf("%t", disabled))

	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	name := firstNonEmpty(got.Name, c.File)
	save, _ := json.Marshal(map[string]any{"name": name, "json": json.RawMessage(body)})
	if _, err = hostCall("host.auth.save", save); err != nil {
		return err
	}
	// The cached credential list is now a minute out of date on the one thing
	// that just changed, and the recount below depends on it.
	r.app.invalidateAuthCache()
	return nil
}

// record appends to the audit trail.
//
// It takes the two locks in sequence, never nested. Flush and Report both hold
// the app lock and then take the rotator's, so reaching for the app lock while
// holding the rotator's would close an ABBA deadlock around the request path.
func (r *rotator) record(rec switchRecord) {
	r.mu.Lock()
	day := time.Unix(rec.At, 0).UTC().Format("2006-01-02")
	if r.state.DayKey != day {
		r.state.DayKey, r.state.DayCount = day, 0
	}
	r.state.DayCount++
	r.state.Log = append(r.state.Log, rec)
	if len(r.state.Log) > switchLogLimit {
		r.state.Log = r.state.Log[len(r.state.Log)-switchLogLimit:]
	}
	r.mu.Unlock()

	r.app.markDirty()
}

// snapshot and restore carry the audit trail and the circuit breaker across a
// restart, so a proxy that bounces cannot spend its daily switch budget twice.
// Both are called with the app lock already held.
func (r *rotator) snapshot() RotatorState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.state
	out.Log = append([]switchRecord(nil), r.state.Log...)
	out.LastCandids = append([]candidate(nil), r.state.LastCandids...)
	return out
}

func (r *rotator) restore(state RotatorState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = state
}

// mayAct is the circuit breaker. A rotator that can thrash is worse than one
// that occasionally rotates late.
func (r *rotator) mayAct(now time.Time, cfg RotatorConfig) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.LastSwitch > 0 &&
		now.Sub(time.Unix(r.state.LastSwitch, 0)) < time.Duration(cfg.MinSwitchGapMinutes)*time.Minute {
		return false
	}
	if r.state.DayKey == now.UTC().Format("2006-01-02") && r.state.DayCount >= cfg.MaxChangesPerDay {
		r.state.LastReason = "daily_limit_reached"
		return false
	}
	return true
}

// note records why the evaluation ended and reports whether that is a change.
// The rotator runs every couple of minutes; a log line each time would bury the
// transition, which is the only part worth reading.
func (r *rotator) note(reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := r.state.LastReason != reason
	r.state.LastReason = reason
	return changed
}

// soonestRecovery is when the first passed-over candidate gets its allowance
// back. Without it "nothing qualifies" is a dead end rather than a wait.
func soonestRecovery(candidates []candidate) int64 {
	best := int64(0)
	now := time.Now()
	for _, c := range candidates {
		if c.Skip == "" || c.Skip == skipDeadToken || c.Skip == skipExcluded {
			continue
		}
		if c.ExpiresIn <= 0 {
			continue
		}
		at := now.Add(time.Duration(c.ExpiresIn * float64(time.Hour))).Unix()
		if best == 0 || at < best {
			best = at
		}
	}
	return best
}

// Report renders the rotator for the panel.
func (r *rotator) Report(cfg RotatorConfig) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]any{
		"enabled":           cfg.Enabled,
		"dry_run":           cfg.DryRun,
		"keep_enabled":      cfg.KeepEnabled,
		"switch_at_percent": cfg.SwitchAtPercent,
		"last_reason":       r.state.LastReason,
		"changes_today":     r.state.DayCount,
		"max_changes_daily": cfg.MaxChangesPerDay,
	}
	if r.state.LastSweep > 0 {
		out["last_sweep"] = time.Unix(r.state.LastSweep, 0).UTC().Format(time.RFC3339)
		out["last_sweep_age_seconds"] = ageSeconds(r.state.LastSweep, time.Now())
	}
	if len(r.state.LastCandids) > 0 {
		out["candidates"] = r.state.LastCandids
	}
	warnings := make([]map[string]any, 0, 2)
	if cfg.Enabled && r.state.RecoversAt > 0 {
		out["recovers_at"] = time.Unix(r.state.RecoversAt, 0).UTC().Format(time.RFC3339)
		out["recovers_in_seconds"] = int64(math.Max(0, time.Until(time.Unix(r.state.RecoversAt, 0)).Seconds()))
	}
	if cfg.Enabled && r.state.LastReason == "pool_has_nothing_serving" {
		w := map[string]any{"code": "rotator_stuck"}
		if v, ok := out["recovers_in_seconds"]; ok {
			w["in_seconds"] = v
		}
		warnings = append(warnings, w)
	} else if cfg.Enabled && r.state.LastReason == "no_standby_available" {
		w := map[string]any{"code": "rotator_degraded", "serving": r.state.HealthyEnabled,
			"target": cfg.KeepEnabled}
		if v, ok := out["recovers_in_seconds"]; ok {
			w["in_seconds"] = v
		}
		warnings = append(warnings, w)
	}
	if cfg.Enabled && cfg.DryRun {
		warnings = append(warnings, map[string]any{"code": "rotator_dry_run"})
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}

	if len(r.state.Log) > 0 {
		log := r.state.Log
		if len(log) > 20 {
			log = log[len(log)-20:]
		}
		rows := make([]map[string]any, 0, len(log))
		for i := len(log) - 1; i >= 0; i-- {
			rows = append(rows, map[string]any{
				"at":      time.Unix(log[i].At, 0).UTC().Format(time.RFC3339),
				"action":  log[i].Action,
				"file":    log[i].File,
				"reason":  log[i].Reason,
				"dry_run": log[i].DryRun,
			})
		}
		out["log"] = rows
	}
	return out
}

// bindingWindow picks the window that decides availability: the one with the
// least headroom, counting a window that has already rolled over as full.
func bindingWindow(windows []windowReading, now time.Time) (headroom float64, minutes int, resetInHours float64) {
	headroom = 100
	for _, w := range windows {
		head := 100 - w.Percent
		if w.ResetAt > 0 && time.Unix(w.ResetAt, 0).Before(now) {
			head = 100
		}
		if head <= headroom {
			headroom = head
			minutes = w.Minutes
			if w.ResetAt > 0 {
				resetInHours = math.Max(0, time.Until(time.Unix(w.ResetAt, 0)).Hours())
			} else {
				resetInHours = 0
			}
		}
	}
	return headroom, minutes, resetInHours
}

// reliefSoon reports whether an empty window is about to refill anyway.
func reliefSoon(windows []windowReading, now time.Time) bool {
	for _, w := range windows {
		if 100-w.Percent > 0 {
			continue
		}
		if w.ResetAt > 0 && time.Until(time.Unix(w.ResetAt, 0)) <= probeMinRelief {
			return true
		}
	}
	return false
}

// serviceHours is how long this credential would carry the current demand.
// Percentages are not comparable across accounts - a percent of a Plus plan is
// worth far less than a percent of a Team one - so the arithmetic goes through
// money, which is the one figure this plugin already estimates per credential.
func serviceHours(app *App, e authEntry, c candidate, usdPerHour float64) float64 {
	if usdPerHour <= 0 {
		return 0
	}
	quota := app.quotaUSDFor(e, c.Binding)
	if quota <= 0 {
		return 0
	}
	return quota * c.Headroom / 100 / usdPerHour
}

func contains(list []string, name string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), name) {
			return true
		}
	}
	return false
}

func dryRunSuffix(dry bool) string {
	if dry {
		return " [dry run]"
	}
	return ""
}

// splitPool separates the enabled members from the standbys, dropping the ones
// configuration says never to touch.
func splitPool(entries []authEntry, cfg RotatorConfig) (enabled, standby []authEntry) {
	for _, e := range entries {
		if cfg.Provider != "" && e.Type != "" && !strings.EqualFold(e.Type, cfg.Provider) {
			continue
		}
		if e.Disabled {
			if !contains(cfg.NeverEnable, e.Name) {
				standby = append(standby, e)
			}
			continue
		}
		enabled = append(enabled, e)
	}
	return enabled, standby
}

// hostHTTPDo issues one request through the proxy's HTTP stack and hands back
// the response headers, which is where the quota lives.
func hostHTTPDo(method, url string, headers map[string][]string, body []byte) (int, map[string][]string, error) {
	payload, err := json.Marshal(map[string]any{
		"Method":  method,
		"URL":     url,
		"Headers": headers,
		"Body":    body,
	})
	if err != nil {
		return 0, nil, err
	}
	result, err := hostCall("host.http.do", payload)
	if err != nil {
		return 0, nil, err
	}
	var resp struct {
		StatusCode int                 `json:"StatusCode"`
		Headers    map[string][]string `json:"Headers"`
	}
	if err = json.Unmarshal(result, &resp); err != nil {
		return 0, nil, fmt.Errorf("decode host http response: %w", err)
	}
	return resp.StatusCode, resp.Headers, nil
}
