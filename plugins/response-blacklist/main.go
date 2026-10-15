// Command response-blacklist is a Bifrost v2.3.0 dynamic (.so) plugin that
// keeps gateway internals out of what customers receive. It removes and
// renames response headers, and removes or renames entries of JSON response
// bodies, streamed or not, following rules from a GO Feature Flag flag, with
// rules in the plugin config as the baseline when the flag serves none.
//
// Build it with build.sh, never a bare `go build`: a Go plugin only loads into
// a host built by the same Go toolchain, the same -trimpath setting, the same
// libc and the same version of every shared dependency.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

const (
	pluginName     = "response-blacklist"
	defaultFlagKey = "blacklist-bifrost"
	warnInterval   = 30 * time.Second
)

// Version is stamped by build.sh through -ldflags "-X main.Version=...".
var Version = "dev"

var (
	// active is the instance whose rules the hooks apply.
	active atomic.Pointer[blacklist]
	// live counts initialized instances. Bifrost reloads a plugin by running the
	// new instance's Init before the old instance's Cleanup, and a .so reopened
	// from the same file shares this state, so only the last Cleanup stops work.
	live atomic.Int32
)

type blacklist struct {
	inline   *rules      // from the plugin config; nil when it has none
	flag     *flagSource // nil when no flag_url is configured
	interval time.Duration

	flagRules atomic.Pointer[rules] // what the flag serves; nil when it serves none
	lastRaw   []byte                // the flag value flagRules came from; poller only
	reported  string                // last state logged; poller only

	stop     chan struct{}
	stopOnce sync.Once
	lastWarn atomic.Int64
	warned   sync.Map // removal paths already reported as unremovable from streams
}

// Init receives the plugin's "config" object from Bifrost; see README.md.
func Init(config any) error {
	b, err := newBlacklist(config)
	if err != nil {
		return fmt.Errorf("%s: %w", pluginName, err)
	}
	if b.flag != nil {
		b.refresh() // answer the first requests with the flag's rules
		go b.poll()
	}
	if old := active.Swap(b); old != nil {
		old.shutdown()
	}
	live.Add(1)
	source := "plugin config"
	if b.flag != nil {
		source = fmt.Sprintf("flag %s at %s every %s", b.flag.key, b.flag.evalURL, b.interval)
	}
	fmt.Printf("[%s] plugin %s loaded, rules from %s; now applying: %s\n", pluginName, Version, source, b.current().describe())
	return nil
}

// GetName is required by the Bifrost plugin loader.
func GetName() string {
	return pluginName
}

// HTTPTransportResponseHeadersHook removes and renames response headers. It
// runs right before Bifrost commits the headers of every response on the
// inference routes, streamed or not.
func HTTPTransportResponseHeadersHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponseMetadata) error {
	if b := active.Load(); b != nil && resp != nil {
		if r := b.current(); r.hasHeaderRules() {
			r.applyHeaders(resp)
		}
	}
	return nil
}

// HTTPTransportPostHook applies the body rules to a non-streamed JSON body:
// the bytes the client is about to receive.
func HTTPTransportPostHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponse) error {
	b := active.Load()
	if b == nil || resp == nil || len(resp.Body) == 0 {
		return nil
	}
	r := b.current()
	if !r.hasBodyRules() || !isJSON(resp) {
		return nil
	}
	out, changed, err := r.sanitizeJSON(resp.Body)
	if err != nil {
		b.warn("response body left unchanged, it is not valid JSON: %v", err)
		return nil
	}
	if changed {
		resp.Body = out
	}
	return nil
}

// HTTPTransportStreamChunkHook applies the body rules to each stream chunk.
func HTTPTransportStreamChunkHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	b := active.Load()
	if b == nil || chunk == nil {
		return chunk, nil
	}
	r := b.current()
	if !r.hasBodyRules() {
		return chunk, nil
	}
	out, unremoved := r.sanitizeChunk(chunk)
	for _, path := range unremoved {
		if _, seen := b.warned.LoadOrStore(path, true); !seen {
			fmt.Fprintf(os.Stderr, "[%s] body.remove %s: stream chunks keep this field, emptied, because Bifrost always serializes it\n", pluginName, path)
		}
	}
	return out, nil
}

// Cleanup is required by the Bifrost plugin loader.
func Cleanup() error {
	if live.Add(-1) <= 0 {
		live.Store(0)
		if b := active.Swap(nil); b != nil {
			b.shutdown()
		}
	}
	return nil
}

func newBlacklist(config any) (*blacklist, error) {
	cfg, _ := config.(map[string]any)
	if cfg == nil {
		return nil, errors.New("config must be an object with flag_url and/or rules")
	}
	b := &blacklist{stop: make(chan struct{})}

	if raw, ok := cfg["rules"]; ok && raw != nil {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("rules: %w", err)
		}
		if b.inline, err = decodeRules(encoded); err != nil {
			return nil, fmt.Errorf("rules: %w", err)
		}
	}

	if base := resolveEnv(stringOpt(cfg, "flag_url")); base != "" {
		key := stringOpt(cfg, "flag_key")
		if key == "" {
			key = defaultFlagKey
		}
		timeout := time.Duration(intOpt(cfg, "timeout_ms", 2000)) * time.Millisecond
		if timeout <= 0 {
			timeout = 2 * time.Second
		}
		var err error
		if b.flag, err = newFlagSource(base, key, timeout); err != nil {
			return nil, err
		}
		b.interval = time.Duration(intOpt(cfg, "refresh_seconds", 30)) * time.Second
		if b.interval < time.Second {
			b.interval = time.Second
		}
	}

	if b.inline == nil && b.flag == nil {
		return nil, errors.New("set flag_url (GO Feature Flag relay proxy) and/or rules")
	}
	return b, nil
}

// current is the rule set the hooks apply: the flag's when it serves one,
// otherwise the plugin config's.
func (b *blacklist) current() *rules {
	if r := b.flagRules.Load(); r != nil {
		return r
	}
	return b.inline
}

func (b *blacklist) poll() {
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
			b.refresh()
		}
	}
}

// refresh asks the relay for the flag. A flag that is disabled or missing
// hands over to the plugin config's rules; a relay that cannot answer, or a
// value that does not compile, keeps whatever is applied now.
func (b *blacklist) refresh() {
	raw, err := b.flag.fetch()
	switch {
	case errors.Is(err, errNotServed):
		b.flagRules.Store(nil)
		b.lastRaw = nil
		b.report(fmt.Sprintf("flag %s serves no rules (disabled or missing); applying the plugin config's: %s", b.flag.key, b.inline.describe()))
		return
	case err != nil:
		b.warn("flag %s unavailable, keeping the rules applied now: %v", b.flag.key, err)
		return
	}
	if b.flagRules.Load() != nil && bytes.Equal(raw, b.lastRaw) {
		return
	}
	r, err := decodeRules(raw)
	if err != nil {
		b.warn("flag %s has invalid rules, keeping the rules applied now: %v", b.flag.key, err)
		return
	}
	b.flagRules.Store(r)
	b.lastRaw = append(b.lastRaw[:0], raw...)
	b.report(fmt.Sprintf("flag %s rules applied: %s", b.flag.key, r.describe()))
}

// report logs a change of state once, not on every poll.
func (b *blacklist) report(msg string) {
	if msg == b.reported {
		return
	}
	b.reported = msg
	fmt.Printf("[%s] %s\n", pluginName, msg)
}

func (b *blacklist) shutdown() {
	b.stopOnce.Do(func() { close(b.stop) })
}

// warn prints at most one line per warnInterval, so an outage cannot flood the log.
func (b *blacklist) warn(format string, args ...any) {
	now := time.Now().UnixNano()
	last := b.lastWarn.Load()
	if now-last < int64(warnInterval) || !b.lastWarn.CompareAndSwap(last, now) {
		return
	}
	fmt.Fprintf(os.Stderr, "["+pluginName+"] "+format+"\n", args...)
}

func (r *rules) describe() string {
	if r == nil {
		return "none"
	}
	return r.summary
}

// isJSON reports whether a buffered response body is JSON.
func isJSON(resp *schemas.HTTPResponse) bool {
	for k, v := range resp.Headers {
		if strings.EqualFold(k, "content-type") {
			return strings.Contains(strings.ToLower(v), "json")
		}
	}
	trimmed := bytes.TrimSpace(resp.Body)
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

// resolveEnv expands Bifrost's "env.NAME" convention.
func resolveEnv(value string) string {
	if name, ok := strings.CutPrefix(value, "env."); ok {
		return strings.TrimSpace(os.Getenv(name))
	}
	return strings.TrimSpace(value)
}

func stringOpt(cfg map[string]any, key string) string {
	s, _ := cfg[key].(string)
	return s
}

func intOpt(cfg map[string]any, key string, def int) int {
	switch v := cfg[key].(type) {
	case float64: // JSON numbers
		return int(v)
	case int:
		return v
	}
	return def
}
