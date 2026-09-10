package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Price is the public OpenAI API rate card for one model, in USD per one
// million tokens.
type Price struct {
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CacheRead  float64 `yaml:"cache_read" json:"cache_read"`
	CacheWrite float64 `yaml:"cache_write" json:"cache_write"`
}

// builtinPrices is a snapshot of the models.dev OpenAI catalog. It exists so a
// fresh install produces correct numbers before the first catalog fetch lands,
// and so the plugin keeps working when models.dev is unreachable. The live
// catalog overwrites every entry it knows about.
var builtinPrices = map[string]Price{
	"gpt-5":               {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-5-mini":          {Input: 0.25, Output: 2, CacheRead: 0.025},
	"gpt-5-nano":          {Input: 0.05, Output: 0.4, CacheRead: 0.005},
	"gpt-5-pro":           {Input: 15, Output: 120},
	"gpt-5.1":             {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-5.2":             {Input: 1.75, Output: 14, CacheRead: 0.175},
	"gpt-5.2-pro":         {Input: 21, Output: 168},
	"gpt-5.3-codex":       {Input: 1.75, Output: 14, CacheRead: 0.175},
	"gpt-5.3-codex-spark": {Input: 1.75, Output: 14, CacheRead: 0.175},
	"gpt-5.4":             {Input: 2.5, Output: 15, CacheRead: 0.25},
	"gpt-5.4-mini":        {Input: 0.75, Output: 4.5, CacheRead: 0.075},
	"gpt-5.4-nano":        {Input: 0.2, Output: 1.25, CacheRead: 0.02},
	"gpt-5.4-pro":         {Input: 30, Output: 180},
	"gpt-5.5":             {Input: 5, Output: 30, CacheRead: 0.5},
	"gpt-5.5-pro":         {Input: 30, Output: 180},
	"gpt-5.6":             {Input: 4, Output: 20, CacheRead: 0.4, CacheWrite: 5},
	"gpt-5.6-sol":         {Input: 4, Output: 20, CacheRead: 0.4, CacheWrite: 5},
	"gpt-5.6-terra":       {Input: 2, Output: 12, CacheRead: 0.2, CacheWrite: 2.5},
	"gpt-5.6-luna":        {Input: 0.2, Output: 1.2, CacheRead: 0.02, CacheWrite: 0.25},
	"gpt-6-astra":         {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
}

// effortSuffixes are reasoning-effort decorations that gateways append to a
// model name. They never change the rate card.
var effortSuffixes = []string{"-minimal", "-none", "-low", "-medium", "-high", "-xhigh", "-max", "-thinking"}

type priceBook struct {
	mu        sync.RWMutex
	catalog   map[string]Price
	overrides map[string]Price
	source    string
	transport string
	fetchedAt time.Time
	lastError string
	path      string
}

type priceCacheFile struct {
	Source    string           `json:"source"`
	Transport string           `json:"transport,omitempty"`
	FetchedAt time.Time        `json:"fetched_at"`
	Models    map[string]Price `json:"models"`
}

func newPriceBook(dataDir string) *priceBook {
	pb := &priceBook{
		catalog: make(map[string]Price, len(builtinPrices)),
		path:    filepath.Join(dataDir, "prices.json"),
		source:  "builtin",
	}
	for k, v := range builtinPrices {
		pb.catalog[k] = v
	}
	pb.loadCache()
	return pb
}

func (pb *priceBook) setOverrides(overrides map[string]Price) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.overrides = make(map[string]Price, len(overrides))
	for k, v := range overrides {
		pb.overrides[normalizeModel(k)] = v
	}
}

func (pb *priceBook) loadCache() {
	raw, err := os.ReadFile(pb.path)
	if err != nil {
		return
	}
	var cached priceCacheFile
	if err = json.Unmarshal(raw, &cached); err != nil || len(cached.Models) == 0 {
		return
	}
	pb.mu.Lock()
	defer pb.mu.Unlock()
	for k, v := range cached.Models {
		pb.catalog[k] = v
	}
	pb.source = cached.Source
	pb.transport = cached.Transport
	pb.fetchedAt = cached.FetchedAt
}

func (pb *priceBook) stale(every time.Duration) bool {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	return time.Since(pb.fetchedAt) >= every
}

// refresh pulls a models.dev-shaped catalog. The document is
// {provider: {models: {name: {cost: {input, output, cache_read, cache_write}}}}}
// and only the openai provider is read.
func (pb *priceBook) refresh(url string) {
	if strings.TrimSpace(url) == "" {
		return
	}
	raw, via, err := fetchCatalog(url)
	if err != nil {
		pb.recordError(err.Error())
		return
	}

	var doc map[string]struct {
		Models map[string]struct {
			Cost struct {
				Input      *float64 `json:"input"`
				Output     *float64 `json:"output"`
				CacheRead  *float64 `json:"cache_read"`
				CacheWrite *float64 `json:"cache_write"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		pb.recordError("decode catalog: " + err.Error())
		return
	}
	openai, ok := doc["openai"]
	if !ok || len(openai.Models) == 0 {
		pb.recordError("catalog has no openai provider")
		return
	}

	fetched := make(map[string]Price, len(openai.Models))
	for name, entry := range openai.Models {
		if entry.Cost.Input == nil && entry.Cost.Output == nil {
			continue
		}
		fetched[normalizeModel(name)] = Price{
			Input:      deref(entry.Cost.Input),
			Output:     deref(entry.Cost.Output),
			CacheRead:  deref(entry.Cost.CacheRead),
			CacheWrite: deref(entry.Cost.CacheWrite),
		}
	}
	if len(fetched) == 0 {
		pb.recordError("catalog produced no priced models")
		return
	}

	pb.mu.Lock()
	for k, v := range fetched {
		pb.catalog[k] = v
	}
	pb.source = url
	pb.transport = via
	pb.fetchedAt = time.Now()
	pb.lastError = ""
	snapshot := priceCacheFile{Source: pb.source, Transport: pb.transport, FetchedAt: pb.fetchedAt, Models: pb.catalog}
	path := pb.path
	pb.mu.Unlock()

	if body, err := json.MarshalIndent(snapshot, "", "  "); err == nil {
		_ = writeFileAtomic(path, body)
	}
}

// fetchCatalog pulls the catalog, preferring the host's HTTP stack so that an
// operator behind a restricted network configures a proxy once in
// CLIProxyAPI and this inherits it. A direct request is the fallback for when
// the host declines the callback, which is also what happens under the test
// harness. A non-200 answer from the host is reported rather than retried
// directly: the host reached the target and the target said no.
func fetchCatalog(url string) ([]byte, string, error) {
	if status, body, errHost := hostHTTPGet(url); errHost == nil {
		if status == http.StatusOK && len(body) > 0 {
			return body, "host", nil
		}
		if status != 0 && status != http.StatusOK {
			return nil, "", fmt.Errorf("catalog returned HTTP %d via host", status)
		}
	}

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("catalog returned HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", err
	}
	return body, "direct", nil
}

func (pb *priceBook) recordError(msg string) {
	pb.mu.Lock()
	pb.lastError = msg
	pb.mu.Unlock()
	hostLog("warn", "price refresh failed: "+msg)
}

// Lookup resolves a model name to a rate card. It tries the exact name, then
// the name with a reasoning-effort suffix stripped, then the longest known
// prefix, so that "gpt-5.6-sol-high" and "gpt-5.6-sol-2026-08-01" both price as
// "gpt-5.6-sol" instead of silently costing nothing.
func (pb *priceBook) Lookup(model string) (Price, bool) {
	name := normalizeModel(model)
	if name == "" {
		return Price{}, false
	}
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	if p, ok := pb.overrides[name]; ok {
		return p, true
	}
	if p, ok := pb.catalog[name]; ok {
		return p, true
	}

	trimmed := name
	for _, suffix := range effortSuffixes {
		if strings.HasSuffix(trimmed, suffix) {
			trimmed = strings.TrimSuffix(trimmed, suffix)
			break
		}
	}
	if trimmed != name {
		if p, ok := pb.overrides[trimmed]; ok {
			return p, true
		}
		if p, ok := pb.catalog[trimmed]; ok {
			return p, true
		}
	}

	best, bestLen := Price{}, 0
	for candidate, p := range pb.catalog {
		if len(candidate) > bestLen && strings.HasPrefix(trimmed, candidate) {
			best, bestLen = p, len(candidate)
		}
	}
	for candidate, p := range pb.overrides {
		if len(candidate) > bestLen && strings.HasPrefix(trimmed, candidate) {
			best, bestLen = p, len(candidate)
		}
	}
	if bestLen > 0 {
		return best, true
	}
	return Price{}, false
}

func (pb *priceBook) Snapshot() map[string]any {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	names := make([]string, 0, len(pb.catalog))
	for k := range pb.catalog {
		names = append(names, k)
	}
	sort.Strings(names)
	models := make([]map[string]any, 0, len(names))
	for _, name := range names {
		p := pb.catalog[name]
		entry := map[string]any{"model": name, "input": p.Input, "output": p.Output, "cache_read": p.CacheRead, "cache_write": p.CacheWrite}
		if ov, ok := pb.overrides[name]; ok {
			entry["overridden"] = true
			entry["input"], entry["output"] = ov.Input, ov.Output
			entry["cache_read"], entry["cache_write"] = ov.CacheRead, ov.CacheWrite
		}
		models = append(models, entry)
	}
	out := map[string]any{
		"source":     pb.source,
		"transport":  pb.transport,
		"unit":       "USD per 1M tokens",
		"models":     models,
		"last_error": pb.lastError,
	}
	if !pb.fetchedAt.IsZero() {
		out["fetched_at"] = pb.fetchedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// Cost prices one request.
//
// The host documents the OpenAI token counters as subsets: cache-read and
// cache-write tokens are already inside InputTokens, and reasoning tokens are
// already inside OutputTokens. Billing them again would inflate every estimate,
// so cached tokens are carved out of the input bucket and reasoning tokens are
// never added on top of output.
func (p Price) Cost(d Tokens, longThreshold int64, longMultiplier float64) float64 {
	cacheRead := d.CacheRead
	cacheWrite := d.CacheWrite
	// A model with no separate cache-write rate bills those tokens as fresh
	// input; folding them back in is more accurate than charging zero.
	if p.CacheWrite <= 0 {
		cacheWrite = 0
	}
	if p.CacheRead <= 0 {
		cacheRead = 0
	}
	fresh := d.Input - cacheRead - cacheWrite
	if fresh < 0 {
		// Counters disagreed about whether cached tokens are a subset. Trust
		// the smaller, non-negative interpretation rather than going negative.
		fresh = 0
	}

	usd := float64(fresh)*p.Input +
		float64(cacheRead)*p.CacheRead +
		float64(cacheWrite)*p.CacheWrite +
		float64(d.Output)*p.Output
	usd /= 1e6

	if longThreshold > 0 && d.Input > longThreshold && longMultiplier > 0 {
		usd *= longMultiplier
	}
	return usd
}

func normalizeModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func deref(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
