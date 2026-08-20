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
