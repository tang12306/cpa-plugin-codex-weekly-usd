package main

// Starting a window's clock after a reset.
//
// A quota window does not start counting down when it is reset. It starts at
// the first request after that. Measured on 2026-09-12, after upstream reset
// every account: each idle credential read 0% with the whole 168 hours still to
// run, and went on reading exactly that for as long as nothing was sent through
// it. A reset that lands on a standby is a week that has not begun, and every
// hour it waits pushes that credential's next refill an hour later. One minimal
// request starts it - after a single "你好" the same credentials read 604797 of
// 604800 seconds left - and costs about twenty tokens.
//
// So when a reading shows the longest window reset and not yet started, the
// rotator sends one. The readings come from everywhere the plugin already
// reads: the panel being opened, the re-read at a window boundary, and a sweep
// of idle credentials - hourly as a backstop, and at once when live traffic
// shows a reset arriving early, which is what upstream resetting every account
// looks like from the one credential that happens to be serving.

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const (
	// kickMessage is what a kick-start says. Anything upstream accepts would
	// do; this is the shortest thing a person would plausibly type.
	kickMessage = "你好"
	// kickGap and kickDailyCap bound retries of a kick that did not take. One
	// that does take needs no second attempt: the window is running, and the
	// next reading that shows it unstarted can only follow another reset.
	kickGap      = 30 * time.Minute
	kickDailyCap = 3
	// kickBodyLimit bounds how much of the reply is read. The reply to a
	// greeting is a few kilobytes; it is read to the end so the request
	// completes normally rather than being cut off mid-stream.
	kickBodyLimit = 256 << 10
	// kickSettle is how long to wait before reading a greeted window back.
	// Upstream counts the time left in whole seconds, so for the rest of the
	// second it starts in, a started window still reads the whole period and
	// cannot be told from one the greeting missed.
	kickSettle = 3 * time.Second
)

// kickMemo is what the last kick-start of one credential did.
type kickMemo struct {
	At       int64  `json:"at"`
	DayKey   string `json:"day_key,omitempty"`
	DayCount int    `json:"day_count,omitempty"`
	Result   string `json:"result,omitempty"`
}

// unstarted reports whether the longest window a reading covers has been reset
// and not yet started: nothing used, and the whole period still to run. Only a
// read reports it this way - the usage endpoint answers with the full period
// for as long as the window waits - and the second of tolerance absorbs
// rounding at the moment a window does start.
func unstarted(windows []windowReading) bool {
	longest := -1
	for i, w := range windows {
		if longest < 0 || w.Minutes > windows[longest].Minutes {
			longest = i
		}
	}
	if longest < 0 || windows[longest].Minutes <= 0 {
		return false
	}
	w := windows[longest]
	return w.Percent <= 0 && w.ResetAfter >= int64(w.Minutes)*60-1
}

// modelRequest is the smallest request upstream accepts, saying text.
func modelRequest(token, account, text string, cfg RotatorConfig) ([]byte, map[string][]string) {
	body, _ := json.Marshal(map[string]any{
		"model":  cfg.ProbeModel,
		"store":  false,
		"stream": true,
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": text}},
		}},
	})
	headers := map[string][]string{
		"authorization":      {"Bearer " + token},
		"chatgpt-account-id": {account},
		"openai-beta":        {"responses=experimental"},
		"originator":         {"codex_cli_rs"},
		"user-agent":         {probeUserAgent},
		"content-type":       {"application/json"},
	}
	return body, headers
}

// markKickDue notes that a credential's longest window read as reset and not
// started. The mark is persisted, so a restart between the reading and the
// kick does not lose it.
func (r *rotator) markKickDue(file string, now time.Time) {
	r.mu.Lock()
	if r.state.KickDue == nil {
		r.state.KickDue = map[string]int64{}
	}
	if _, already := r.state.KickDue[file]; !already {
		r.state.KickDue[file] = now.Unix()
	}
	r.mu.Unlock()
}

func (r *rotator) kickDue(file string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, due := r.state.KickDue[file]
	return due
}

func (r *rotator) clearKickDue(file string) {
	r.mu.Lock()
	delete(r.state.KickDue, file)
	r.mu.Unlock()
}

// kickstart is the rotator's pass over reset windows: find the ones nobody
// has looked at, greet every one found waiting, and read them back.
func (r *rotator) kickstart(pool []authEntry, cfg RotatorConfig, now time.Time) {
	if since, due := r.sweepDue(cfg, now); due {
		r.sweepIdle(pool, cfg, now, since)
	}
	var sent []greeted
	for _, e := range pool {
		if !r.kickDue(e.Name) {
			continue
		}
		if g, ok := r.greet(e, cfg, now); ok {
			sent = append(sent, g)
		}
	}
	if len(sent) == 0 {
		return
	}
	// Once for the whole batch: upstream resetting every account queues them
	// all at the same moment.
	time.Sleep(kickSettle)
	for _, g := range sent {
		r.confirm(g, cfg, now)
	}
}

// greeted is a greeting sent and not yet read back.
type greeted struct {
	e     authEntry
	memo  kickMemo
	label string
}

// sweepDue says whether idle credentials should be read now, and from what
// moment a reading counts as current. Live traffic showing an early reset
// makes every reading taken before it out of date, whatever its age.
func (r *rotator) sweepDue(cfg RotatorConfig, now time.Time) (int64, bool) {
	r.mu.Lock()
	last := r.state.IdleSweepAt
	r.mu.Unlock()
	interval := time.Duration(cfg.KickstartSweepMinutes) * time.Minute
	since := now.Add(-interval).Unix()
	if cfg.KickstartSweepMinutes <= 0 {
		since = 0
	}
	if early := r.app.earlyResetAt(); early > last {
		if early > since {
			since = early
		}
		return since, true
	}
	if cfg.KickstartSweepMinutes <= 0 {
		return 0, false
	}
	return since, now.Sub(time.Unix(last, 0)) >= interval
}

// sweepIdle reads every credential with no reading since `since`. A credential
// that is serving always has one - each response it produces carries it - so
// what this reads is exactly the idle ones, which are the only ones a reset
// can leave waiting.
func (r *rotator) sweepIdle(pool []authEntry, cfg RotatorConfig, now time.Time, since int64) {
	r.mu.Lock()
	r.state.IdleSweepAt = now.Unix()
	r.mu.Unlock()
	r.app.markDirty()

	var idle []authEntry
	for _, e := range pool {
		if allowed, why := r.probeAllowed(e, probeResult{}, cfg, now, true); !allowed && why != skipProbeBudget {
			continue
		}
		if memo, ok := r.memo(e.Name); ok && now.Sub(time.Unix(memo.At, 0)) < refreshGap {
			continue
		}
		if _, seen := r.app.observedWindows(e); seen >= since && seen > 0 {
			continue
		}
		idle = append(idle, e)
	}
	if len(idle) == 0 {
		return
	}
	r.readMany(idle, cfg, now, "reading idle credentials for a reset")
	waiting := 0
	for _, e := range idle {
		if r.kickDue(e.Name) {
			waiting++
		}
	}
	if waiting > 0 {
		hostLog("info", fmt.Sprintf("kickstart: read %d idle credential(s); %d reset and waiting to start", len(idle), waiting))
	}
}

// greet sends one credential's reset window its first request, after looking
// again that it is still waiting. It reports whether a greeting went out that
// now wants reading back; anything else is settled here.
func (r *rotator) greet(e authEntry, cfg RotatorConfig, now time.Time) (greeted, bool) {
	if contains(cfg.NeverEnable, e.Name) {
		r.clearKickDue(e.Name)
		return greeted{}, false
	}
	if memo, ok := r.memo(e.Name); ok && memo.Rejected && r.rejectionStands(e, memo) {
		r.clearKickDue(e.Name)
		return greeted{}, false
	}
	today := now.UTC().Format("2006-01-02")
	km := r.lastKick(e.Name)
	if km.At > 0 && now.Sub(time.Unix(km.At, 0)) < kickGap {
		return greeted{}, false
	}
	if km.DayKey == today && km.DayCount >= kickDailyCap {
		return greeted{}, false
	}

	// The reading that queued this may be minutes old, and traffic may have
	// started the window since. The usage endpoint is free; a greeting sent
	// to a window that is already running is waste.
	before := r.settle(e, probeResult{}, r.look(e, cfg), now, "checking a reset window before starting it", false)
	if before.Error != "" {
		return greeted{}, false
	}
	if !unstarted(before.Windows) {
		r.clearKickDue(e.Name)
		return greeted{}, false
	}

	g := greeted{e: e, label: firstNonEmpty(e.Label, e.Email, e.Name),
		memo: kickMemo{At: now.Unix(), DayKey: today, DayCount: 1}}
	if km.DayKey == today {
		g.memo.DayCount = km.DayCount + 1
	}

	if cfg.DryRun {
		g.memo.Result = "dry_run"
		r.saveKick(e.Name, g.memo)
		r.clearKickDue(e.Name)
		r.logKick(switchRecord{At: now.Unix(), Action: "kickstart", File: e.Name, DryRun: true,
			Reason: "would start the reset window's countdown"})
		hostLog("info", "kickstart (dry run): would start "+g.label+"'s reset window")
		return greeted{}, false
	}

	fail := func(result, reason string) (greeted, bool) {
		g.memo.Result = result
		r.saveKick(e.Name, g.memo)
		r.logKick(switchRecord{At: now.Unix(), Action: "kickstart", File: e.Name, Reason: reason})
		hostLog("warn", "kickstart: "+g.label+": "+reason)
		return greeted{}, false
	}
	token, account, proxyURL, err := r.credential(e)
	if err != nil {
		return fail("no_credential", "could not read the credential: "+err.Error())
	}
	body, headers := modelRequest(token, account, kickMessage, cfg)
	status, _, _, errSend := probeDo(proxyURL, "POST", firstNonEmpty(cfg.ProbeURL, probeURL), headers, body, kickBodyLimit)
	switch {
	case errSend != nil:
		return fail("send_failed", "could not send: "+errSend.Error())
	case status/100 != 2:
		return fail(fmt.Sprintf("http_%d", status), fmt.Sprintf("upstream answered HTTP %d", status))
	}
	return g, true
}

// confirm reads a greeted window back and records what happened to it.
func (r *rotator) confirm(g greeted, cfg RotatorConfig, now time.Time) {
	after := r.settle(g.e, probeResult{}, r.look(g.e, cfg), now, "confirming a kick-started window", false)
	var reason string
	switch {
	case after.Error != "":
		g.memo.Result = "unconfirmed"
		reason = "sent; could not read it back: " + after.Error
	case unstarted(after.Windows):
		g.memo.Result = "did_not_start"
		reason = "sent, but the window still reads as not started"
	default:
		g.memo.Result = "started"
		r.clearKickDue(g.e.Name)
		reason = "reset window started" + resetNote(after.Windows, now)
	}
	r.saveKick(g.e.Name, g.memo)
	r.logKick(switchRecord{At: now.Unix(), Action: "kickstart", File: g.e.Name, Reason: reason})
	level := "info"
	if g.memo.Result != "started" {
		level = "warn"
	}
	hostLog(level, "kickstart: "+g.label+": "+reason)
}

// resetNote names when the longest window will next refill.
func resetNote(windows []windowReading, now time.Time) string {
	longest := -1
	for i, w := range windows {
		if longest < 0 || w.Minutes > windows[longest].Minutes {
			longest = i
		}
	}
	if longest < 0 || windows[longest].ResetAt <= 0 {
		return ""
	}
	w := windows[longest]
	return fmt.Sprintf("; %s window refills %s UTC", windowLabel(w.Minutes),
		time.Unix(w.ResetAt, 0).UTC().Format("01-02 15:04"))
}

func (r *rotator) lastKick(file string) kickMemo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Kicks[file]
}

func (r *rotator) saveKick(file string, memo kickMemo) {
	r.mu.Lock()
	if r.state.Kicks == nil {
		r.state.Kicks = map[string]kickMemo{}
	}
	r.state.Kicks[file] = memo
	r.mu.Unlock()
	r.app.markDirty()
}

// logKick adds a kick-start to the audit trail. Unlike record it does not
// count against the circuit breaker: a kick changes no credential's state, and
// letting a reset of every account spend the day's rotation budget would leave
// the pool unable to rotate afterwards.
func (r *rotator) logKick(rec switchRecord) {
	r.mu.Lock()
	r.state.Log = append(r.state.Log, rec)
	if len(r.state.Log) > switchLogLimit {
		r.state.Log = r.state.Log[len(r.state.Log)-switchLogLimit:]
	}
	r.mu.Unlock()
	r.app.markDirty()
}

// kicksToday counts kick-starts sent today, for the panel. Caller holds r.mu.
func (r *rotator) kicksToday(now time.Time) int {
	today := now.UTC().Format("2006-01-02")
	n := 0
	for _, m := range r.state.Kicks {
		if m.DayKey == today && m.Result != "dry_run" {
			n += m.DayCount
		}
	}
	return n
}

// waitingToStart lists the credentials read as reset and not yet started.
// Caller holds r.mu.
func (r *rotator) waitingToStart() []string {
	out := make([]string, 0, len(r.state.KickDue))
	for f := range r.state.KickDue {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
