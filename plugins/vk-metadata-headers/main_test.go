package main

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// fakeTracer stands in for Bifrost's tracer: it records what the plugin hands its header setter.
type fakeTracer struct {
	calls   int
	traceID string
	headers map[string]string
}

func (f *fakeTracer) SetTraceRequestHeaders(traceID string, headers map[string]string) {
	f.calls++
	f.traceID, f.headers = traceID, maps.Clone(headers)
}

var testMapping = map[string]string{"x-cce-key-id": "cce_key_id", "x-cce-project-id": "cce_project_id"}

func initWith(t *testing.T, config map[string]any) {
	t.Helper()
	if err := Init(config); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { mapping.Store(nil) })
}

// request builds the context a PreLLMHook sees after governance resolved a key with this metadata.
func request(metadata, sent map[string]string, tracer any) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if metadata != nil {
		ctx.SetValue(virtualKeyMetadataKey, metadata)
	}
	if sent != nil {
		ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, sent)
	}
	if tracer != nil {
		ctx.SetValue(schemas.BifrostContextKeyTracer, tracer)
		ctx.SetValue(schemas.BifrostContextKeyTraceID, "trace-1")
	}
	return ctx
}

func TestCopiesMappedMetadataOntoTheTraceHeaders(t *testing.T) {
	initWith(t, map[string]any{"headers": map[string]any{"X-CCE-Key-Id": "cce_key_id", "x-cce-project-id": "cce_project_id"}})
	tracer := &fakeTracer{}
	sent := map[string]string{"x-cce-path": "/chat/completions", "x-cce-key-id": "spoofed", "authorization": "Bearer sk-bf-x"}
	req := &schemas.BifrostRequest{}

	got, shortCircuit, err := PreLLMHook(request(map[string]string{"cce_key_id": "hello-world", "cce_project_id": "hello-project", "owner": "abla"}, sent, tracer), req)

	if got != req || shortCircuit != nil || err != nil {
		t.Fatalf("PreLLMHook must pass the request through, got %v %v %v", got, shortCircuit, err)
	}
	want := map[string]string{
		"x-cce-path":       "/chat/completions",
		"x-cce-key-id":     "hello-world", // the key's metadata wins over what the caller sent
		"x-cce-project-id": "hello-project",
		"authorization":    "Bearer sk-bf-x", // filtered and redacted by Bifrost's setter, not here
	}
	if tracer.calls != 1 || tracer.traceID != "trace-1" || !maps.Equal(tracer.headers, want) {
		t.Fatalf("setter got %d call(s), trace %q, headers %v; want %v", tracer.calls, tracer.traceID, tracer.headers, want)
	}
	if sent["x-cce-key-id"] != "spoofed" {
		t.Fatal("the request's own header map must not be modified")
	}
}

func TestMapsOnlyTheMetadataKeysThatArePresent(t *testing.T) {
	initWith(t, map[string]any{"headers": map[string]any{"x-cce-key-id": "cce_key_id", "x-cce-project-id": "cce_project_id"}})
	tracer := &fakeTracer{}

	PreLLMHook(request(map[string]string{"cce_project_id": "hello-project", "cce_key_id": ""}, map[string]string{}, tracer), &schemas.BifrostRequest{})

	if want := map[string]string{"x-cce-project-id": "hello-project"}; !maps.Equal(tracer.headers, want) {
		t.Fatalf("headers %v, want %v", tracer.headers, want)
	}
}

func TestLeavesTheTraceAloneWhenThereIsNothingToCopy(t *testing.T) {
	initWith(t, map[string]any{"headers": map[string]any{"x-cce-key-id": "cce_key_id"}})
	sent := map[string]string{"x-cce-path": "/chat/completions"}
	cases := map[string]struct {
		metadata map[string]string
		sent     map[string]string
	}{
		"key without metadata":      {nil, sent},
		"metadata without the key":  {map[string]string{"owner": "abla"}, sent},
		"no request headers on ctx": {map[string]string{"cce_key_id": "hello-world"}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracer := &fakeTracer{}
			PreLLMHook(request(tc.metadata, tc.sent, tracer), &schemas.BifrostRequest{})
			if tracer.calls != 0 {
				t.Fatalf("setter called with %v", tracer.headers)
			}
		})
	}
}

func TestSurvivesAMissingOrForeignTracer(t *testing.T) {
	initWith(t, map[string]any{"headers": map[string]any{"x-cce-key-id": "cce_key_id"}})
	metadata := map[string]string{"cce_key_id": "hello-world"}

	PreLLMHook(request(metadata, map[string]string{}, nil), &schemas.BifrostRequest{})
	PreLLMHook(request(metadata, map[string]string{}, "not a tracer"), &schemas.BifrostRequest{})
}

func TestUnconfiguredPluginDoesNothing(t *testing.T) {
	mapping.Store(nil)
	tracer := &fakeTracer{}

	PreLLMHook(request(map[string]string{"cce_key_id": "hello-world"}, map[string]string{}, tracer), &schemas.BifrostRequest{})

	if tracer.calls != 0 {
		t.Fatal("setter called without a mapping")
	}
}

func TestParseConfig(t *testing.T) {
	got, err := parseConfig(map[string]any{"headers": map[string]any{" X-CCE-Key-Id ": " cce_key_id ", "x-cce-project-id": "cce_project_id"}})
	if err != nil || !maps.Equal(got, testMapping) {
		t.Fatalf("parseConfig = %v, %v; want %v", got, err, testMapping)
	}

	for name, config := range map[string]any{
		"not an object":       "headers",
		"no headers":          map[string]any{},
		"empty headers":       map[string]any{"headers": map[string]any{}},
		"non-string key":      map[string]any{"headers": map[string]any{"x-cce-key-id": 1}},
		"empty metadata key":  map[string]any{"headers": map[string]any{"x-cce-key-id": " "}},
		"header with a colon": map[string]any{"headers": map[string]any{"x-cce:key": "cce_key_id"}},
		"mapped twice":        map[string]any{"headers": map[string]any{"X-CCE-Key-Id": "a", "x-cce-key-id": "b"}},
	} {
		if _, err := parseConfig(config); err == nil {
			t.Errorf("%s: parseConfig accepted %v", name, config)
		}
	}
}

func TestInitRejectsABadConfigAndKeepsTheOldMapping(t *testing.T) {
	initWith(t, map[string]any{"headers": map[string]any{"x-cce-key-id": "cce_key_id"}})

	err := Init(map[string]any{"headers": map[string]any{}})

	if err == nil || !strings.HasPrefix(err.Error(), pluginName+": ") {
		t.Fatalf("Init error = %v", err)
	}
	if m := mapping.Load(); m == nil || (*m)["x-cce-key-id"] != "cce_key_id" {
		t.Fatalf("mapping after a failed Init = %v", m)
	}
}
