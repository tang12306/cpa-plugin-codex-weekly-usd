package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Config mirrors the plugins.configs.codex-weekly-usd block in config.yaml.
// The host hands the block over as YAML on plugin.register and again on every
// plugin.reconfigure, so this is the only configuration surface the plugin has.
type Config struct {
	Enabled  bool `yaml:"enabled"`
	Priority int  `yaml:"priority"`

	// DataDir holds state.json, the price cache and the event log.
	DataDir string `yaml:"data_dir"`

	// PriceSourceURL is a models.dev-shaped catalog used to refresh pricing.
	// Set it empty to freeze pricing at the built-in table plus overrides.
	PriceSourceURL string `yaml:"price_source_url"`

	// PriceRefreshHours controls how often the catalog is re-fetched.
	PriceRefreshHours int `yaml:"price_refresh_hours"`

	// PriceOverrides wins over both the catalog and the built-in table. Values
	// are USD per one million tokens.
	PriceOverrides map[string]Price `yaml:"price_overrides"`

	// LongContextThreshold enables the OpenAI long-context surcharge above the
	// given prompt size. Zero disables it.
	LongContextThreshold int64   `yaml:"long_context_threshold"`
	LongContextMultiplier float64 `yaml:"long_context_multiplier"`

	// EventLog writes one JSON line per request for auditing the estimate.
	EventLog        bool `yaml:"event_log"`
	EventLogKeepDays int  `yaml:"event_log_keep_days"`

	// FlushSeconds debounces state writes so the hot path stays in memory.
	FlushSeconds int `yaml:"flush_seconds"`

	// StaleAfterMinutes marks a credential whose last quota reading is older
	// than this, so a days-old percentage is never shown as if it were current.
	StaleAfterMinutes int `yaml:"stale_after_minutes"`

	// ModelHealthDays is how long a (credential, model) availability record is
	// kept after its last request. It also decides how long a model a
	// credential has stopped serving keeps counting towards that model's
	// capacity, so it wants to be comfortably longer than the longest quota
	// window and comfortably shorter than "we retired that credential".
	ModelHealthDays int `yaml:"model_health_days"`

	// Rotator keeps a pool of credentials enabled and replaces a member before
	// it runs out. It is the only part of this plugin that writes anything
	// outside its own data directory, so it is off unless asked for.
	Rotator RotatorConfig `yaml:"rotator"`
}

// RotatorConfig governs the credential pool. Every default here is chosen to
// fail towards doing nothing: an unset field must never be the reason a
// credential gets switched off.
type RotatorConfig struct {
	Enabled bool `yaml:"enabled"`
	// DryRun decides everything and writes nothing, logging each action it
	// would have taken. Worth a day of running before it is trusted.
	DryRun bool `yaml:"dry_run"`
	// Provider limits the pool to one credential type.
	Provider string `yaml:"provider"`
	// KeepEnabled is how many healthy credentials to hold in the pool. Two is
	// the useful minimum: the proxy retries a refused request on the next
	// enabled credential, so a second member makes a handover invisible to
	// callers, while a pool of one cannot hand over at all.
	KeepEnabled int `yaml:"keep_enabled"`
	// SwitchAtPercent is the headroom floor, in percentage points of the
	// tightest window.
	SwitchAtPercent float64 `yaml:"switch_at_percent"`
	// LeadTimeMinutes replaces a member whose measured burn would exhaust it
	// within this long, which catches a burst that would step over the floor
	// between two checks.
	LeadTimeMinutes int `yaml:"lead_time_minutes"`
	// HorizonHours is how far ahead a replacement must be able to carry the
	// current demand to count as a safe choice.
	HorizonHours float64 `yaml:"horizon_hours"`
	// CheckIntervalSeconds is how often the pool is judged. The check itself
	// costs nothing: no upstream request is made until the pool is short.
	CheckIntervalSeconds int `yaml:"check_interval_seconds"`
	// MinSwitchGapMinutes and MaxSwitchesPerDay are the circuit breaker. A
	// rotator that can thrash is worse than one that rotates late.
	MinSwitchGapMinutes int `yaml:"min_switch_gap_minutes"`
	// MaxChangesPerDay counts credential state changes, not rotations: one
	// rotation enables a replacement and disables the incumbent, so it spends
	// two.
	MaxChangesPerDay int `yaml:"max_changes_per_day"`
	// ProbeModel is the model named in the probe request. It must be one the
	// account can actually call, or every probe reads as a failure.
	ProbeModel string `yaml:"probe_model"`
	// DisableDeadTokens switches off a credential upstream has stopped
	// accepting. Disabling is the safe direction: it removes capacity that was
	// not working anyway.
	DisableDeadTokens bool `yaml:"disable_dead_tokens"`
	// NeverEnable and NeverDisable are the manual override, by file name.
	NeverEnable  []string `yaml:"never_enable"`
	NeverDisable []string `yaml:"never_disable"`
}

func defaultConfig() Config {
	return Config{
		DataDir:               "data/" + pluginID,
		PriceSourceURL:        "https://models.dev/api.json",
		PriceRefreshHours:     24,
		LongContextThreshold:  0,
		LongContextMultiplier: 2,
		EventLog:              true,
		EventLogKeepDays:      60,
		FlushSeconds:          15,
		StaleAfterMinutes:     360,
		ModelHealthDays:       14,
		Rotator: RotatorConfig{
			Enabled:              false,
			Provider:             "codex",
			KeepEnabled:          2,
			SwitchAtPercent:      10,
			LeadTimeMinutes:      15,
			HorizonHours:         4,
			CheckIntervalSeconds: 60,
			MinSwitchGapMinutes:  10,
			MaxChangesPerDay:     20,
			ProbeModel:           "gpt-5.6-sol",
			DisableDeadTokens:    true,
		},
	}
}

func (c *Config) normalize() {
	def := defaultConfig()
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = def.DataDir
	}
	if !filepath.IsAbs(c.DataDir) {
		if wd, err := os.Getwd(); err == nil {
			c.DataDir = filepath.Join(wd, c.DataDir)
		}
	}
	if c.PriceRefreshHours <= 0 {
		c.PriceRefreshHours = def.PriceRefreshHours
	}
	if c.LongContextMultiplier <= 0 {
		c.LongContextMultiplier = def.LongContextMultiplier
	}
	if c.EventLogKeepDays <= 0 {
		c.EventLogKeepDays = def.EventLogKeepDays
	}
	if c.FlushSeconds <= 0 {
		c.FlushSeconds = def.FlushSeconds
	}
	if c.StaleAfterMinutes < 0 {
		c.StaleAfterMinutes = 0
	} else if c.StaleAfterMinutes == 0 {
		c.StaleAfterMinutes = def.StaleAfterMinutes
	}
	if c.ModelHealthDays <= 0 {
		c.ModelHealthDays = def.ModelHealthDays
	}
	c.Rotator.normalize(def.Rotator)
}

// normalize refuses to run the rotator on nonsense. A zero pool size would ask
// it to disable everything, and a floor at or above 100 would ask it to rotate
// on every check, so both fall back to the default rather than being obeyed.
func (rc *RotatorConfig) normalize(def RotatorConfig) {
	if rc.KeepEnabled <= 0 {
		rc.KeepEnabled = def.KeepEnabled
	}
	if rc.SwitchAtPercent <= 0 || rc.SwitchAtPercent >= 100 {
		rc.SwitchAtPercent = def.SwitchAtPercent
	}
	if rc.LeadTimeMinutes < 0 {
		rc.LeadTimeMinutes = def.LeadTimeMinutes
	}
	if rc.HorizonHours <= 0 {
		rc.HorizonHours = def.HorizonHours
	}
	if rc.CheckIntervalSeconds < 10 {
		rc.CheckIntervalSeconds = def.CheckIntervalSeconds
	}
	if rc.MinSwitchGapMinutes < 0 {
		rc.MinSwitchGapMinutes = def.MinSwitchGapMinutes
	}
	if rc.MaxChangesPerDay <= 0 {
		rc.MaxChangesPerDay = def.MaxChangesPerDay
	}
	if strings.TrimSpace(rc.ProbeModel) == "" {
		rc.ProbeModel = def.ProbeModel
	}
	if strings.TrimSpace(rc.Provider) == "" {
		rc.Provider = def.Provider
	}
}

func (c Config) priceRefresh() time.Duration {
	return time.Duration(c.PriceRefreshHours) * time.Hour
}

// configFields describes the block to the management UI so the panel can render
// a form instead of making the operator hand-edit YAML.
func configFields() []map[string]any {
	return []map[string]any{
		{"Name": "data_dir", "Type": "string", "Description": "Directory for state.json, the price cache and the event log."},
		{"Name": "price_source_url", "Type": "string", "Description": "models.dev-shaped catalog URL. Empty freezes pricing at the built-in table."},
		{"Name": "price_refresh_hours", "Type": "integer", "Description": "Hours between price catalog refreshes."},
		{"Name": "price_overrides", "Type": "object", "Description": "Manual USD per 1M tokens: {model: {input, output, cache_read, cache_write}}."},
		{"Name": "long_context_threshold", "Type": "integer", "Description": "Apply the long-context surcharge above this prompt size. 0 disables."},
		{"Name": "long_context_multiplier", "Type": "number", "Description": "Multiplier applied above long_context_threshold."},
		{"Name": "event_log", "Type": "boolean", "Description": "Write one JSON line per request for auditing the estimate."},
		{"Name": "event_log_keep_days", "Type": "integer", "Description": "Days of event log to retain."},
		{"Name": "flush_seconds", "Type": "integer", "Description": "Seconds between state snapshots to disk."},
		{"Name": "stale_after_minutes", "Type": "integer", "Description": "Flag a credential whose newest quota reading is older than this."},
		{"Name": "model_health_days", "Type": "integer", "Description": "Days a (credential, model) availability record is kept after its last request."},
		{"Name": "rotator", "Type": "object", "Description": "Credential pool: {enabled, dry_run, keep_enabled, switch_at_percent, lead_time_minutes, horizon_hours, probe_model, never_enable, never_disable}. Off by default; the only part that writes auth files."},
	}
}

var (
	appMu   sync.RWMutex
	appInst *App
)

func current() *App {
	appMu.RLock()
	defer appMu.RUnlock()
	return appInst
}

// configure (re)builds the running app from a YAML block. It is called on both
// plugin.register and plugin.reconfigure, so it must be safe to run repeatedly:
// an existing app keeps its accumulated state and only swaps the config in.
func configure(raw []byte) {
	cfg := defaultConfig()
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			hostLog("warn", "config parse failed, falling back to defaults: "+err.Error())
			cfg = defaultConfig()
		}
	}
	cfg.normalize()

	appMu.Lock()
	defer appMu.Unlock()
	if appInst != nil {
		appInst.Reconfigure(cfg)
		return
	}
	app, err := newApp(cfg)
	if err != nil {
		hostLog("error", "initialisation failed: "+err.Error())
		return
	}
	appInst = app
}
