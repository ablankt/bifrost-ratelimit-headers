// Command vk-metadata-headers is a test plugin for Bifrost Enterprise v2.3.2. It copies entries of
// the presented virtual key's metadata into the request headers Bifrost records on the request's
// trace, so trace connectors such as Kafka export them as if the caller had sent those headers.
// The request itself is not touched: nothing reaches the provider or the response.
//
// It leans on two internals no plugin API promises: the context key governance stores the key's
// metadata under, and the trace-header setter of Bifrost's own tracer. Both are checked at run
// time, and the plugin logs on the trace why it did nothing.
//
// Build it with build.sh, which picks up this directory's go.mod and build.env: the module set
// and toolchain of the Enterprise 2.3.2 binary, not the OSS v2.3.0 ones the other plugins use.
package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/maximhq/bifrost/core/schemas"
)

const pluginName = "vk-metadata-headers"

// virtualKeyMetadataKey is where governance leaves the presented key's metadata, in its
// PreRequestHook (plugins/governance/store.go, VirtualKeyMetadataContextKey). Copied rather than
// imported so the plugin does not link the governance package.
const virtualKeyMetadataKey = schemas.BifrostContextKey("bifrost-governance-virtual-key-metadata")

// traceHeaderSetter is implemented by Bifrost's tracer (framework/tracing.Tracer) but is not part
// of schemas.Tracer. It replaces the trace's recorded headers, keeping only the names a connector
// asked for (the Kafka plugin's request_headers) and redacting credentials.
type traceHeaderSetter interface {
	SetTraceRequestHeaders(traceID string, headers map[string]string)
}

// Version is stamped by build.sh through -ldflags "-X main.Version=...".
var Version = "dev"

// mapping holds header name -> metadata key. Init swaps it whole, so a hook sees one mapping.
var mapping atomic.Pointer[map[string]string]

// The loader type-asserts each symbol against an exact signature and refuses the plugin when one
// does not match; these fail the build instead, before a broken .so reaches a pod.
var _ func(config any) error = Init
var _ func() string = GetName
var _ func() error = Cleanup
var _ func(*schemas.BifrostContext, *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) = PreLLMHook

// Init reads {"headers": {"<header name>": "<virtual key metadata key>", ...}}.
func Init(config any) error {
	m, err := parseConfig(config)
	if err != nil {
		return fmt.Errorf("%s: %w", pluginName, err)
	}
	mapping.Store(&m)
	fmt.Printf("[%s] plugin %s loaded, copying virtual key metadata onto trace headers: %s\n", pluginName, Version, describe(m, "<-"))
	return nil
}

// GetName is required by the Bifrost plugin loader.
func GetName() string { return pluginName }

// Cleanup is required by the Bifrost plugin loader.
func Cleanup() error { return nil }

// PreLLMHook runs after every plugin's PreRequestHook, so governance has resolved the key by then.
func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if m := mapping.Load(); m != nil {
		copyToTrace(ctx, *m)
	}
	return req, nil, nil
}

func copyToTrace(ctx *schemas.BifrostContext, m map[string]string) {
	metadata, _ := ctx.Value(virtualKeyMetadataKey).(map[string]string)
	values := make(map[string]string, len(m))
	for header, key := range m {
		if value := metadata[key]; value != "" {
			values[header] = value
		}
	}
	if len(values) == 0 {
		ctx.Log(schemas.LogLevelInfo, "virtual key metadata has none of the mapped keys, trace headers left as sent")
		return
	}

	setter, _ := ctx.Value(schemas.BifrostContextKeyTracer).(traceHeaderSetter)
	traceID, _ := ctx.Value(schemas.BifrostContextKeyTraceID).(string)
	if setter == nil || traceID == "" {
		ctx.Log(schemas.LogLevelWarn, "no trace with a header setter on this request, trace headers left as sent")
		return
	}
	// The setter replaces the recorded headers, so without the sent ones they would be lost.
	sent, ok := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	if !ok {
		ctx.Log(schemas.LogLevelWarn, "request headers missing from the context, trace headers left as sent")
		return
	}

	headers := make(map[string]string, len(sent)+len(values))
	maps.Copy(headers, sent)
	maps.Copy(headers, values) // the key's metadata wins over a header the caller sent
	setter.SetTraceRequestHeaders(traceID, headers)
	ctx.Log(schemas.LogLevelInfo, "trace headers set from virtual key metadata: "+describe(values, "="))
}

func parseConfig(config any) (map[string]string, error) {
	cfg, ok := config.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(`config must be an object like {"headers": {"x-cce-key-id": "cce_key_id"}}`)
	}
	raw, ok := cfg["headers"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf(`"headers" must map at least one header name to a virtual key metadata key`)
	}
	m := make(map[string]string, len(raw))
	for name, value := range raw {
		key, ok := value.(string)
		header := strings.ToLower(strings.TrimSpace(name))
		switch {
		case !ok || strings.TrimSpace(key) == "":
			return nil, fmt.Errorf("headers.%s: must be a non-empty metadata key", name)
		case header == "" || strings.ContainsAny(header, " \t:"):
			return nil, fmt.Errorf("headers: %q is not a header name", name)
		case m[header] != "":
			return nil, fmt.Errorf("headers: %q is mapped twice (names are case-insensitive)", header)
		}
		m[header] = strings.TrimSpace(key)
	}
	return m, nil
}

// describe renders a header map as "a<sep>x, b<sep>y" in header order, for one-line logs.
func describe(m map[string]string, sep string) string {
	parts := make([]string, 0, len(m))
	for _, header := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, header+sep+m[header])
	}
	return strings.Join(parts, ", ")
}
