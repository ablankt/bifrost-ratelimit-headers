// Command model-normalizer is a Bifrost v2.3.0 dynamic (.so) plugin that restores the
// canonical spelling of the requested model.
//
// Clients may send a model in any case ("t-systems-dev/qWeN3.5-9B"). The upstream can
// accept it, but logs and cost attribution then carry a name the datasheet does not
// know, since its keys are case-sensitive ("t-systems-dev/Qwen3.5-9B"). Before Bifrost
// parses the request, the plugin rewrites the top-level "model" of a JSON body to the
// canonical name that matches it case-insensitively, so routing, logs and pricing all
// see one spelling. Names it does not know are left alone.
//
// Build it with build.sh, never a bare `go build`: a Go plugin only loads into a host
// built by the same Go toolchain, the same -trimpath setting, the same libc and the same
// version of every shared dependency.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const pluginName = "model-normalizer"

// timingHeader reports how long the pre-hook took. The pre-hook stores it as a request
// header, which the response-headers hook copies to the response: the two hooks see the
// same request, but not the same BifrostContext.
const timingHeader = "x-normaliser-timing"

// Version is stamped by build.sh through -ldflags "-X main.Version=...".
var Version = "dev"

var (
	active atomic.Pointer[normalizer]
	// live counts initialised instances. Bifrost reloads a plugin by running the new
	// instance's Init before the old instance's Cleanup, and a .so reopened from the
	// same path shares this state, so only the last Cleanup stops the refresher.
	live   int
	liveMu sync.Mutex
)

// Init receives the plugin's "config" object from Bifrost:
//
//	{"datasheet_url": "http://portal-api.portal-api.svc.cluster.local:8000/v1/datasheet"}
//	{"models": ["t-systems-dev/Qwen3.5-9B"]}
func Init(config any) error {
	n, err := newNormalizer(config)
	if err != nil {
		return fmt.Errorf("%s: %w", pluginName, err)
	}
	n.start()
	liveMu.Lock()
	live++
	old := active.Swap(n)
	liveMu.Unlock()
	if old != nil {
		old.stop()
	}
	fmt.Printf("[%s] plugin %s loaded, %d canonical model names (%s)\n", pluginName, Version, n.index.Load().size(), n.source())
	return nil
}

// GetName is required by the Bifrost plugin loader.
func GetName() string { return pluginName }

// Cleanup stops the plugin once its last instance is cleaned up.
func Cleanup() error {
	liveMu.Lock()
	defer liveMu.Unlock()
	if live > 1 {
		live--
		return nil
	}
	live = 0
	if n := active.Swap(nil); n != nil {
		n.stop()
	}
	return nil
}

// HTTPTransportPreHook rewrites the model of a JSON request body to its canonical
// spelling. It runs after authentication and before Bifrost parses the body, so the
// requested model Bifrost records is already the canonical one.
func HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	n := active.Load()
	if n == nil || req == nil {
		return nil, nil
	}
	start := time.Now()
	normalize(ctx, n, req)
	if req.Headers != nil {
		// Always written, so a timing a client sent in the request is overwritten.
		setHeader(req.Headers, timingHeader, strconv.FormatFloat(float64(time.Since(start).Nanoseconds())/1e6, 'f', 3, 64)+"ms")
	}
	return nil, nil
}

// HTTPTransportResponseHeadersHook adds x-normaliser-timing, the time the pre-hook took,
// to the response.
func HTTPTransportResponseHeadersHook(_ *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponseMetadata) error {
	if active.Load() == nil || req == nil || resp == nil {
		return nil
	}
	if timing := req.CaseInsensitiveHeaderLookup(timingHeader); timing != "" {
		resp.SetHeader(timingHeader, timing)
	}
	return nil
}

func normalize(ctx *schemas.BifrostContext, n *normalizer, req *schemas.HTTPRequest) {
	if len(req.Body) == 0 || !isJSON(req) {
		return
	}
	model := gjson.GetBytes(req.Body, "model")
	if model.Type != gjson.String {
		return
	}
	canonical, ok := n.index.Load().canonical(model.Str)
	if !ok || canonical == model.Str {
		return
	}
	body, err := sjson.SetBytes(req.Body, "model", canonical)
	if err != nil {
		return
	}
	req.Body = body
	if ctx != nil {
		ctx.Log(schemas.LogLevelInfo, fmt.Sprintf("model %q normalized to %q", model.Str, canonical))
	}
}

// setHeader writes under the name the header already has, if any: two map entries
// differing only in case would both be written back, in random order.
func setHeader(headers map[string]string, name, value string) {
	for k := range headers {
		if strings.EqualFold(k, name) {
			headers[k] = value
			return
		}
	}
	headers[name] = value
}

// index maps case-folded model names to their canonical spelling. It is immutable once
// built, so lookups need no lock.
type index struct {
	full  map[string]string // "provider/model", folded -> canonical
	model map[string]string // "model" alone, folded -> canonical, only when unambiguous
}

func buildIndex(names []string) *index {
	ix := &index{full: map[string]string{}, model: map[string]string{}}
	ambiguous := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		ix.full[strings.ToLower(name)] = name
		if _, part, ok := strings.Cut(name, "/"); ok && part != "" {
			key := strings.ToLower(part)
			if prev, seen := ix.model[key]; seen && prev != part {
				ambiguous[key] = true
			}
			ix.model[key] = part
		}
	}
	for key := range ambiguous {
		delete(ix.model, key) // two providers spell it differently: leave it alone
	}
	return ix
}

func (ix *index) size() int { return len(ix.full) }

// canonical returns the canonical spelling of name, matching "provider/model" in full or,
// without a provider prefix, the model part when only one canonical name has it.
func (ix *index) canonical(name string) (string, bool) {
	if c, ok := ix.full[name]; ok {
		return c, true // already canonical: the common case, no allocation
	}
	folded := strings.ToLower(name)
	if c, ok := ix.full[folded]; ok {
		return c, true
	}
	if !strings.Contains(name, "/") {
		if c, ok := ix.model[folded]; ok {
			return c, true
		}
	}
	return "", false
}

type normalizer struct {
	static       []string
	datasheetURL string
	interval     time.Duration
	client       *http.Client
	index        atomic.Pointer[index]
	stopCh       chan struct{}
	stopOnce     sync.Once
	lastWarn     atomic.Int64
}

func newNormalizer(config any) (*normalizer, error) {
	cfg, _ := config.(map[string]any)
	if cfg == nil {
		return nil, errors.New("config must be an object with datasheet_url and/or models")
	}
	for key := range cfg {
		switch key {
		case "datasheet_url", "models", "refresh_seconds", "timeout_ms":
		default:
			return nil, fmt.Errorf("unknown config key %q", key)
		}
	}
	n := &normalizer{stopCh: make(chan struct{}), interval: 5 * time.Minute}
	if raw, ok := cfg["models"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, errors.New("models must be a list of canonical model names")
		}
		for _, item := range list {
			s, ok := item.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, errors.New("models must be non-empty strings")
			}
			n.static = append(n.static, s)
		}
	}
	if raw, _ := cfg["datasheet_url"].(string); raw != "" {
		u, err := url.Parse(resolveEnv(raw))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("datasheet_url must be an absolute http(s) URL, got %q", raw)
		}
		n.datasheetURL = u.String()
	}
	if n.datasheetURL == "" && len(n.static) == 0 {
		return nil, errors.New("set datasheet_url and/or models")
	}
	if v, ok := cfg["refresh_seconds"].(float64); ok && v >= 10 {
		n.interval = time.Duration(v) * time.Second
	}
	timeout := 5 * time.Second
	if v, ok := cfg["timeout_ms"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Millisecond
	}
	n.client = &http.Client{Timeout: timeout}
	n.index.Store(buildIndex(n.static))
	return n, nil
}

func (n *normalizer) source() string {
	if n.datasheetURL == "" {
		return "config"
	}
	return fmt.Sprintf("config and %s every %s", n.datasheetURL, n.interval)
}

// start reads the datasheet once, so the first requests already use it, then keeps it
// fresh until stop.
func (n *normalizer) start() {
	if n.datasheetURL == "" {
		return
	}
	n.refresh()
	go func() {
		ticker := time.NewTicker(n.interval)
		defer ticker.Stop()
		for {
			select {
			case <-n.stopCh:
				return
			case <-ticker.C:
				n.refresh()
			}
		}
	}()
}

func (n *normalizer) stop() {
	n.stopOnce.Do(func() {
		close(n.stopCh)
		n.client.CloseIdleConnections()
	})
}

// refresh rebuilds the index from the datasheet's keys plus the configured models. A
// datasheet that cannot be read keeps the index as it is.
func (n *normalizer) refresh() {
	names, err := n.fetchDatasheet()
	if err != nil {
		n.warn("datasheet unavailable, keeping %d known model names: %v", n.index.Load().size(), err)
		return
	}
	n.index.Store(buildIndex(append(names, n.static...)))
}

// fetchDatasheet returns the keys of a datasheet: a JSON object keyed by
// "provider/model", as served by portal-api's /v1/datasheet and getbifrost.ai/datasheet.
func (n *normalizer) fetchDatasheet() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), n.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.datasheetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var sheet map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&sheet); err != nil {
		return nil, fmt.Errorf("not a JSON object keyed by model: %w", err)
	}
	names := make([]string, 0, len(sheet))
	for name := range sheet {
		names = append(names, name)
	}
	return names, nil
}

// warn prints at most one line every 5 minutes, so an outage cannot flood the log.
func (n *normalizer) warn(format string, args ...any) {
	now := time.Now().UnixNano()
	last := n.lastWarn.Load()
	if now-last < int64(5*time.Minute) || !n.lastWarn.CompareAndSwap(last, now) {
		return
	}
	fmt.Fprintf(os.Stderr, "["+pluginName+"] "+format+"\n", args...)
}

// isJSON reports whether a request body is JSON: multipart uploads (transcriptions,
// files) carry their model elsewhere and are left alone.
func isJSON(req *schemas.HTTPRequest) bool {
	if ct := req.CaseInsensitiveHeaderLookup("content-type"); ct != "" {
		return strings.Contains(strings.ToLower(ct), "json")
	}
	trimmed := bytes.TrimSpace(req.Body)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// resolveEnv expands Bifrost's "env.NAME" convention.
func resolveEnv(value string) string {
	if name, ok := strings.CutPrefix(value, "env."); ok {
		return strings.TrimSpace(os.Getenv(name))
	}
	return strings.TrimSpace(value)
}
