package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func mustRules(t *testing.T, spec string) *rules {
	t.Helper()
	r, err := decodeRules([]byte(spec))
	if err != nil {
		t.Fatalf("decodeRules: %v", err)
	}
	return r
}

func sanitize(t *testing.T, r *rules, body string) map[string]any {
	t.Helper()
	out, _, err := r.sanitizeJSON([]byte(body))
	if err != nil {
		t.Fatalf("sanitizeJSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return doc
}

const bifrostBody = `{
  "id": "chatcmpl-1",
  "is_bifrost_error": false,
  "prompt_filter_results": [{"prompt_index": 0, "content_filter_results": {"hate": {"filtered": false}}}],
  "choices": [
    {"index": 0, "message": {"role": "assistant", "content": "Bifrost is the bridge to Asgard."}, "content_filter_results": {"x": 1}},
    {"index": 1, "message": {"role": "assistant", "content": "second"}, "content_filter_results": {"x": 2}}
  ],
  "usage": {"prompt_tokens": 12345678901234567, "total_tokens": 0.1},
  "extra_fields": {
    "provider": "t-systems",
    "provider_response_headers": {"x-upstream": "secret"},
    "routing_info": {"provider": "t-systems", "key": "dev-key"},
    "latency": 42
  }
}`

func TestBodyRemoval(t *testing.T) {
	r := mustRules(t, `{"body": {"remove": [
		"extra_fields.provider", "extra_fields.provider_response_headers",
		"prompt_filter_results", "choices.content_filter_results", "extra_fields.routing_info.*"]}}`)
	doc := sanitize(t, r, bifrostBody)

	extra := doc["extra_fields"].(map[string]any)
	for _, gone := range []string{"provider", "provider_response_headers"} {
		if _, ok := extra[gone]; ok {
			t.Errorf("extra_fields.%s survived", gone)
		}
	}
	if _, ok := doc["prompt_filter_results"]; ok {
		t.Error("prompt_filter_results survived")
	}
	for _, c := range doc["choices"].([]any) {
		if _, ok := c.(map[string]any)["content_filter_results"]; ok {
			t.Error("choices[].content_filter_results survived")
		}
	}
	if ri := extra["routing_info"].(map[string]any); len(ri) != 0 {
		t.Errorf("routing_info.* left %v", ri)
	}
	if extra["latency"] == nil || doc["id"] != "chatcmpl-1" {
		t.Error("unrelated fields were removed")
	}
}

func TestRemovesWhateverTheValueIs(t *testing.T) {
	r := mustRules(t, `{"body": {"remove": ["extra_fields.provider_response_headers"]}}`)
	for _, value := range []string{`"a string"`, `{"nested": {"deep": true}}`, `[1, 2]`, `null`} {
		doc := sanitize(t, r, `{"extra_fields": {"provider_response_headers": `+value+`, "keep": 1}}`)
		if _, ok := doc["extra_fields"].(map[string]any)["provider_response_headers"]; ok {
			t.Errorf("value %s survived", value)
		}
	}
}

func TestBodyKeyRename(t *testing.T) {
	r := mustRules(t, `{"body": {"rename": [{"pattern": "bifrost", "replacement": "backend"}]}}`)
	doc := sanitize(t, r, `{"is_bifrost_error": true, "nested": [{"bifrost_id": 1}]}`)
	if doc["is_backend_error"] != true || doc["is_bifrost_error"] != nil {
		t.Errorf("root key not renamed: %v", doc)
	}
	if doc["nested"].([]any)[0].(map[string]any)["backend_id"] == nil {
		t.Errorf("nested key not renamed: %v", doc)
	}
}

func TestBodyValueRenameOnlyUnderPaths(t *testing.T) {
	r := mustRules(t, `{"body": {"rename": [{"pattern": "(?i)bifrost", "replacement": "backend", "target": "values", "paths": ["error.message", "extra_fields"]}]}}`)
	doc := sanitize(t, r, `{"error": {"message": "Bifrost says no"}, "extra_fields": {"note": ["bifrost a", "b"]},
		"choices": [{"message": {"content": "Bifrost is the bridge to Asgard."}}]}`)
	if got := doc["error"].(map[string]any)["message"]; got != "backend says no" {
		t.Errorf("error.message = %v", got)
	}
	if got := doc["extra_fields"].(map[string]any)["note"].([]any)[0]; got != "backend a" {
		t.Errorf("extra_fields.note[0] = %v", got)
	}
	content := doc["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]
	if content != "Bifrost is the bridge to Asgard." {
		t.Errorf("model output was rewritten: %v", content)
	}
}

func TestPreservesNumbersAndHTML(t *testing.T) {
	r := mustRules(t, `{"body": {"remove": ["extra_fields.provider"]}}`)
	out, changed, err := r.sanitizeJSON([]byte(`{"n": 12345678901234567, "f": 0.10, "html": "<b>&</b>", "extra_fields": {"provider": "x"}}`))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	for _, want := range []string{`12345678901234567`, `0.10`, `"<b>&</b>"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output %s lost %s", out, want)
		}
	}
}

func TestUntouchedBodyIsNotReencoded(t *testing.T) {
	r := mustRules(t, `{"body": {"remove": ["extra_fields.provider"]}}`)
	in := []byte(`{"object": "list",   "data": []}`)
	out, changed, err := r.sanitizeJSON(in)
	if err != nil || changed || &out[0] != &in[0] {
		t.Fatalf("body without the key was touched: changed=%v err=%v", changed, err)
	}
}

func TestHeaders(t *testing.T) {
	r := mustRules(t, `{"headers": {
		"remove": ["x-bifrost-routing-info-.*", "X-Bifrost-Cache-Hit"],
		"rename": [
			{"pattern": "bifrost", "replacement": "backend"},
			{"pattern": "bifrost", "replacement": "backend", "target": "values"}
		]}}`)
	resp := &schemas.HTTPResponseMetadata{Headers: map[string]string{
		"X-Bifrost-Trace-Id":                 "abc",
		"X-Bifrost-Routing-Info-Provider":    "t-systems",
		"x-bifrost-cache-hit":                "true",
		"Content-Type":                       "application/json",
		"X-Request-Source":                   "bifrost-gateway",
		"X-Bifrost-Routing-Info-Primary-Key": "dev",
	}}
	r.applyHeaders(resp)
	want := map[string]string{
		"x-backend-trace-id": "abc",
		"Content-Type":       "application/json",
		"x-request-source":   "backend-gateway",
	}
	if len(resp.Headers) != len(want) {
		t.Fatalf("headers = %v", resp.Headers)
	}
	for k, v := range want {
		if resp.Headers[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, resp.Headers[k], v, resp.Headers)
		}
	}
}

func TestInvalidRulesAreRefused(t *testing.T) {
	for _, spec := range []string{
		`{"body": {"removes": ["x"]}}`,
		`{"headers": {"remove": ["("]}}`,
		`{"body": {"rename": [{"pattern": "bifrost", "replacement": "x", "target": "values"}]}}`,
		`{"body": {"rename": [{"pattern": "", "replacement": "x"}]}}`,
		`{"body": {"remove": ["a..b"]}}`,
		`{"headers": {"rename": [{"pattern": "a", "replacement": "b", "target": "keys"}]}}`,
	} {
		if _, err := decodeRules([]byte(spec)); err == nil {
			t.Errorf("accepted %s", spec)
		}
	}
}

func streamChunk() *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
		ID:                  "chatcmpl-1",
		Object:              "chat.completion.chunk",
		PromptFilterResults: json.RawMessage(`[{"prompt_index":0}]`),
		ExtraParams:         map[string]any{"internal": true},
		ExtraFields: schemas.BifrostResponseExtraFields{
			Provider:                "t-systems",
			ProviderResponseHeaders: map[string]string{"x-upstream": "secret"},
			RoutingInfo:             schemas.RoutingInfo{Provider: "t-systems", Key: "dev-key"},
			Latency:                 7,
		},
	}}
}

func TestStreamChunkRemovalCopiesAndKeepsInternalFields(t *testing.T) {
	r := mustRules(t, `{"body": {"remove": ["extra_fields.provider", "extra_fields.provider_response_headers", "prompt_filter_results", "extra_fields.routing_info"]}}`)
	in := streamChunk()
	out, unremoved := r.sanitizeChunk(in)
	if out == in || out.BifrostChatResponse == in.BifrostChatResponse {
		t.Fatal("chunk was not copied")
	}
	raw, _ := json.Marshal(out)
	for _, gone := range []string{`t-systems`, `x-upstream`, `prompt_filter_results`, `dev-key`} {
		if strings.Contains(string(raw), gone) {
			t.Errorf("stream chunk still contains %s: %s", gone, raw)
		}
	}
	if out.BifrostChatResponse.ExtraParams["internal"] != true || out.BifrostChatResponse.ExtraFields.Latency != 7 {
		t.Error("fields outside the rules were lost")
	}
	// routing_info has no omitempty: it is emptied, and reported.
	if len(unremoved) != 1 || unremoved[0] != "extra_fields.routing_info" {
		t.Errorf("unremoved = %v", unremoved)
	}
	// Bifrost's own chunk is untouched.
	if in.BifrostChatResponse.ExtraFields.Provider != "t-systems" || in.BifrostChatResponse.ExtraFields.ProviderResponseHeaders["x-upstream"] != "secret" || len(in.BifrostChatResponse.PromptFilterResults) == 0 {
		t.Error("original chunk was modified")
	}
}

func TestStreamChunkWithoutMatchesIsReturnedAsIs(t *testing.T) {
	r := mustRules(t, `{"body": {"remove": ["not_there"]}}`)
	in := streamChunk()
	if out, _ := r.sanitizeChunk(in); out != in {
		t.Fatal("unmatched chunk was copied")
	}
}

func TestStreamErrorValueRename(t *testing.T) {
	r := mustRules(t, `{"body": {"rename": [{"pattern": "(?i)bifrost", "replacement": "backend", "target": "values", "paths": ["error.message"]}]}}`)
	in := &schemas.BifrostStreamChunk{BifrostError: &schemas.BifrostError{Error: &schemas.ErrorField{Message: "bifrost: upstream failed"}}}
	out, _ := r.sanitizeChunk(in)
	if out.BifrostError.Error.Message != "backend: upstream failed" {
		t.Fatalf("message = %q", out.BifrostError.Error.Message)
	}
	if in.BifrostError.Error.Message != "bifrost: upstream failed" {
		t.Fatal("original error was modified")
	}
}

func TestStreamRawMessageNestedRemoval(t *testing.T) {
	r := mustRules(t, `{"body": {"remove": ["prompt_filter_results.prompt_index"]}}`)
	out, _ := r.sanitizeChunk(streamChunk())
	if got := string(out.BifrostChatResponse.PromptFilterResults); got != `[{}]` {
		t.Fatalf("prompt_filter_results = %s", got)
	}
}
