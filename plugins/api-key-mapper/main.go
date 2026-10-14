// Command api-key-mapper is a Bifrost v2.3.0 dynamic (.so) plugin that lets
// clients keep using API keys issued for the previous gateway.
//
// Before Bifrost authenticates a request, the plugin looks the presented key up
// in an api-key-mapping service (POST {mapping_url}/v1/api-key-mappings/resolve).
// When the service knows the key, the plugin puts the Bifrost virtual key it
// maps to in place of the old key, so authentication, governance and logging
// all see an ordinary virtual-key request. When it does not, or the service
// cannot answer, the request is left untouched and Bifrost's own
// authentication decides, so an unknown key is still refused.
//
// Build it with build.sh, never a bare `go build`: a Go plugin only loads into
// a host built by the same Go toolchain, the same -trimpath setting, the same
// libc and the same version of every shared dependency.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

const (
	pluginName = "api-key-mapper"

	virtualKeyHeader = "x-bf-vk"
	statusHeader     = "x-key-mapping"

	defaultResolvePath = "/v1/api-key-mappings/resolve"
	maxResponseBytes   = 64 << 10
	warnInterval       = 30 * time.Second
)

// Version is stamped by build.sh through -ldflags "-X main.Version=...".
var Version = "dev"

// markerHeader carries a successful mapping from the pre-auth hook to the
// response-headers hook as a request header: the two hooks get different
// BifrostContexts, but both see the request's headers.
const markerHeader = "x-key-mapping-status"

// Lookup outcomes, also the values of the x-key-mapping response header.
const (
	statusMapped      = "mapped"
	statusNotFound    = "not_found"
	statusUnavailable = "unavailable"
)

// credentialHeaders are the headers Bifrost reads a virtual key from, in the
// order it reads them. Authorization carries the key as a Bearer token.
var credentialHeaders = []string{"authorization", "x-api-key", "api-key", "x-goog-api-key"}

// active is the mapper built by Init; hooks are no-ops until it is set.
var active atomic.Pointer[mapper]

// Init receives the plugin's "config" object from Bifrost. See README.md for
// the keys; only mapping_url is required.
func Init(config any) error {
	m, err := newMapper(config)
	if err != nil {
		return fmt.Errorf("%s: %w", pluginName, err)
	}
	if old := active.Swap(m); old != nil {
		old.close()
	}
	fmt.Printf("[%s] plugin %s loaded, resolving legacy keys via %s\n", pluginName, Version, m.resolveURL)
	return nil
}

// GetName is required by the Bifrost plugin loader.
func GetName() string {
	return pluginName
}

// HTTPTransportPreAuthHook swaps a mapped legacy key for its virtual key. It is
// the pre-auth hook, not the pre-hook, because the swapped-in key is the
// credential authentication reads.
func HTTPTransportPreAuthHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	m := active.Load()
	if m == nil || req == nil {
		return nil, nil
	}
	m.translate(ctx, req)
	return nil, nil
}

// HTTPTransportResponseHeadersHook reports a successful mapping in the
// x-key-mapping response header. Bifrost skips this hook for requests its
// authentication refuses, so there is no header for unknown keys.
func HTTPTransportResponseHeadersHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponseMetadata) error {
	m := active.Load()
	if m == nil || !m.statusHeader || req == nil {
		return nil
	}
	if req.CaseInsensitiveHeaderLookup(markerHeader) == statusMapped {
		resp.SetHeader(statusHeader, statusMapped)
	}
	return nil
}

// Cleanup is required by the Bifrost plugin loader.
func Cleanup() error {
	if m := active.Swap(nil); m != nil {
		m.close()
	}
	return nil
}

// mapper resolves legacy keys against the mapping service and caches answers.
type mapper struct {
	resolveURL   string
	client       *http.Client
	skipPrefixes []string
	statusHeader bool

	cacheTTL    time.Duration
	notFoundTTL time.Duration
	errorTTL    time.Duration
	cache       *keyCache

	// lookups bounds concurrent calls to the mapping service. Unauthenticated
	// callers can present any number of distinct keys, so without a bound every
	// made-up key would become a mapping-service request.
	lookups  chan struct{}
	inflight flightGroup

	lastWarn atomic.Int64
}

func newMapper(config any) (*mapper, error) {
	cfg, _ := config.(map[string]any)
	if cfg == nil {
		return nil, errors.New("config must be an object with at least mapping_url")
	}

	base := resolveEnv(stringOpt(cfg, "mapping_url", ""))
	if base == "" {
		return nil, errors.New("mapping_url is required, e.g. http://api-key-mapping.<namespace>.svc.cluster.local:8080")
	}
	resolveURL, err := joinURL(base, stringOpt(cfg, "resolve_path", defaultResolvePath))
	if err != nil {
		return nil, err
	}

	timeout := durationOpt(cfg, "timeout_ms", 2000, time.Millisecond)
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	maxLookups := intOpt(cfg, "max_concurrent_lookups", 64)
	if maxLookups < 1 {
		maxLookups = 1
	}

	skipPrefixes := []string{"sk-bf-"}
	if raw, ok := cfg["skip_prefixes"].([]any); ok {
		skipPrefixes = skipPrefixes[:0]
		for _, item := range raw {
			if s, ok := item.(string); ok && s != "" {
				skipPrefixes = append(skipPrefixes, strings.ToLower(s))
			}
		}
	}

	return &mapper{
		resolveURL: resolveURL,
		client: &http.Client{
			Timeout: timeout,
			// The mapping service is cluster-internal: connect directly even when
			// the pod sets HTTP(S)_PROXY for provider traffic.
			Transport: &http.Transport{
				Proxy:               nil,
				DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
				MaxIdleConns:        maxLookups,
				MaxIdleConnsPerHost: maxLookups,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		skipPrefixes: skipPrefixes,
		statusHeader: boolOpt(cfg, "status_header", true),
		cacheTTL:     durationOpt(cfg, "cache_ttl_seconds", 300, time.Second),
		notFoundTTL:  durationOpt(cfg, "negative_cache_ttl_seconds", 60, time.Second),
		errorTTL:     durationOpt(cfg, "error_cache_ttl_seconds", 5, time.Second),
		cache:        newKeyCache(intOpt(cfg, "cache_max_entries", 10000)),
		lookups:      make(chan struct{}, maxLookups),
	}, nil
}

func (m *mapper) close() {
	m.client.CloseIdleConnections()
}

// translate rewrites req in place when its presented key maps to a virtual key.
func (m *mapper) translate(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) {
	if req.Headers == nil {
		return
	}
	// A client-sent marker must not claim a mapping that never happened. The
	// transport only writes headers back, never deletes them, so blank it.
	if name, _ := lookupHeader(req.Headers, markerHeader); name != "" {
		req.Headers[name] = ""
	}
	if req.CaseInsensitiveHeaderLookup(virtualKeyHeader) != "" {
		return // the caller already names its virtual key explicitly
	}
	header, key, ok := m.presentedKey(req)
	if !ok {
		return
	}

	res := m.resolve(key)
	if res.status != statusMapped {
		return
	}
	if ctx != nil {
		ctx.Log(schemas.LogLevelInfo, "legacy API key mapped to virtual key "+res.virtualKeyID)
	}

	if strings.EqualFold(header, "authorization") {
		req.Headers[header] = "Bearer " + res.virtualKey
	} else {
		req.Headers[header] = res.virtualKey
	}
	// x-bf-vk is accepted whatever the key looks like, the other headers only
	// with the sk-bf- prefix, so set it too.
	req.Headers[virtualKeyHeader] = res.virtualKey
	setHeader(req.Headers, markerHeader, statusMapped)
}

// presentedKey returns the header carrying a key worth looking up, under the
// exact name it arrived with, and the key itself. It reports false when the
// request carries no key, or already carries a Bifrost credential and so needs
// no translation.
func (m *mapper) presentedKey(req *schemas.HTTPRequest) (header, key string, ok bool) {
	for _, want := range credentialHeaders {
		name, value := lookupHeader(req.Headers, want)
		if name == "" {
			continue
		}
		value = strings.TrimSpace(value)
		if want == "authorization" {
			if len(value) < 7 || !strings.EqualFold(value[:7], "bearer ") {
				continue // Basic auth and other schemes are not API keys
			}
			value = strings.TrimSpace(value[7:])
		}
		if value == "" {
			continue
		}
		if m.isBifrostCredential(value) {
			return "", "", false
		}
		if looksLikeJWT(value) {
			continue // a user token, which Bifrost verifies itself
		}
		if header == "" {
			header, key = name, value
		}
	}
	return header, key, header != ""
}

func (m *mapper) isBifrostCredential(value string) bool {
	lower := strings.ToLower(value)
	for _, prefix := range m.skipPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

type resolution struct {
	status       string
	virtualKey   string
	virtualKeyID string
}

// resolve answers from the cache, or asks the mapping service once on behalf
// of every concurrent request presenting the same key. Cache entries are
// keyed by the key's SHA-256, so raw legacy keys are never held in memory.
func (m *mapper) resolve(key string) resolution {
	digest := sha256.Sum256([]byte(key))
	if res, ok := m.cache.get(digest); ok {
		return res
	}
	return m.inflight.do(digest, func() resolution {
		if res, ok := m.cache.get(digest); ok {
			return res
		}
		select {
		case m.lookups <- struct{}{}:
			defer func() { <-m.lookups }()
		default:
			// Saturated: not cached, so the next request with this key retries.
			m.warn("mapping lookup skipped: %d lookups already in flight", cap(m.lookups))
			return resolution{status: statusUnavailable}
		}
		res := m.lookup(key)
		ttl := m.errorTTL
		switch res.status {
		case statusMapped:
			ttl = m.cacheTTL
		case statusNotFound:
			ttl = m.notFoundTTL
		}
		m.cache.put(digest, res, ttl)
		return res
	})
}

// lookup calls the mapping service. Nothing it logs contains key material.
func (m *mapper) lookup(key string) resolution {
	body, err := json.Marshal(map[string]string{"apiKey": key})
	if err != nil {
		return resolution{status: statusUnavailable}
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.resolveURL, bytes.NewReader(body))
	if err != nil {
		return resolution{status: statusUnavailable}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		m.warn("mapping service unreachable: %v", err)
		return resolution{status: statusUnavailable}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
	}()

	var payload struct {
		BifrostVirtualKeyID    string `json:"bifrostVirtualKeyId"`
		BifrostVirtualKeyValue string `json:"bifrostVirtualKeyValue"`
		Code                   string `json:"code"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload)

	switch {
	case resp.StatusCode == http.StatusOK && decodeErr == nil && payload.BifrostVirtualKeyValue != "":
		return resolution{status: statusMapped, virtualKey: payload.BifrostVirtualKeyValue, virtualKeyID: payload.BifrostVirtualKeyID}
	case resp.StatusCode == http.StatusNotFound && payload.Code == "mapping_not_found":
		return resolution{status: statusNotFound}
	case resp.StatusCode == http.StatusNotFound:
		// A 404 without the service's own error code is a wrong URL, not an unknown key.
		m.warn("mapping service returned 404 without mapping_not_found; check mapping_url and resolve_path (%s)", m.resolveURL)
	default:
		m.warn("mapping service returned HTTP %d", resp.StatusCode)
	}
	return resolution{status: statusUnavailable}
}

// warn prints at most one line per warnInterval, so an outage cannot flood the log.
func (m *mapper) warn(format string, args ...any) {
	now := time.Now().UnixNano()
	last := m.lastWarn.Load()
	if now-last < int64(warnInterval) || !m.lastWarn.CompareAndSwap(last, now) {
		return
	}
	fmt.Fprintf(os.Stderr, "[%s] "+format+"\n", append([]any{pluginName}, args...)...)
}

// keyCache is a bounded TTL cache of lookup outcomes.
type keyCache struct {
	mu      sync.Mutex
	max     int
	entries map[[sha256.Size]byte]cacheEntry
}

type cacheEntry struct {
	res     resolution
	expires time.Time
}

func newKeyCache(max int) *keyCache {
	if max < 1 {
		max = 1
	}
	return &keyCache{max: max, entries: make(map[[sha256.Size]byte]cacheEntry)}
}

func (c *keyCache) get(k [sha256.Size]byte) (resolution, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return resolution{}, false
	}
	if time.Now().After(e.expires) {
		delete(c.entries, k)
		return resolution{}, false
	}
	return e.res, true
}

func (c *keyCache) put(k [sha256.Size]byte, res resolution, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		for key, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, key)
			}
		}
		// Still full: drop arbitrary entries (map order is random) down to 3/4.
		for key := range c.entries {
			if len(c.entries) < c.max*3/4+1 {
				break
			}
			delete(c.entries, key)
		}
	}
	c.entries[k] = cacheEntry{res: res, expires: now.Add(ttl)}
}

// flightGroup collapses concurrent lookups of the same key into one call.
type flightGroup struct {
	mu    sync.Mutex
	calls map[[sha256.Size]byte]*flight
}

type flight struct {
	wg  sync.WaitGroup
	res resolution
}

func (g *flightGroup) do(k [sha256.Size]byte, fn func() resolution) resolution {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[[sha256.Size]byte]*flight)
	}
	if f, ok := g.calls[k]; ok {
		g.mu.Unlock()
		f.wg.Wait()
		return f.res
	}
	f := &flight{}
	f.wg.Add(1)
	g.calls[k] = f
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.calls, k)
		g.mu.Unlock()
		f.wg.Done()
	}()
	f.res = fn()
	return f.res
}

// setHeader writes under the name the header already has, if any: two map
// entries differing only in case would both be written back, in random order.
func setHeader(headers map[string]string, name, value string) {
	if existing, _ := lookupHeader(headers, name); existing != "" {
		name = existing
	}
	headers[name] = value
}

func lookupHeader(headers map[string]string, want string) (name, value string) {
	for k, v := range headers {
		if strings.EqualFold(k, want) {
			return k, v
		}
	}
	return "", ""
}

// looksLikeJWT spots user tokens (three base64url segments, JSON header), which
// are never legacy API keys and are verified by Bifrost itself.
func looksLikeJWT(value string) bool {
	return strings.HasPrefix(value, "eyJ") && strings.Count(value, ".") == 2
}

func joinURL(base, path string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("mapping_url must be an absolute http(s) URL, got %q", base)
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return u.String() + path, nil
}

// resolveEnv expands Bifrost's "env.NAME" convention.
func resolveEnv(value string) string {
	if name, ok := strings.CutPrefix(value, "env."); ok {
		return strings.TrimSpace(os.Getenv(name))
	}
	return strings.TrimSpace(value)
}

func stringOpt(cfg map[string]any, key, def string) string {
	if s, ok := cfg[key].(string); ok {
		return s
	}
	return def
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

func boolOpt(cfg map[string]any, key string, def bool) bool {
	if b, ok := cfg[key].(bool); ok {
		return b
	}
	return def
}

func durationOpt(cfg map[string]any, key string, def int, unit time.Duration) time.Duration {
	return time.Duration(intOpt(cfg, key, def)) * unit
}
