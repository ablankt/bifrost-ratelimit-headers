package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func initWith(t *testing.T, config map[string]any) {
	t.Helper()
	if err := Init(config); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = Cleanup() })
}

func chat(model string) *schemas.HTTPRequest {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "Qwen3.5-9B said <b>hi</b>"}},
		"stream":   true,
	})
	return &schemas.HTTPRequest{Method: "POST", Path: "/v1/chat/completions", Headers: map[string]string{"Content-Type": "application/json"}, Body: body}
}

func modelOf(t *testing.T, req *schemas.HTTPRequest) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(req.Body, &doc); err != nil {
		t.Fatalf("body is no longer JSON: %v\n%s", err, req.Body)
	}
	return doc["model"].(string)
}

func TestNormalizesToCanonicalSpelling(t *testing.T) {
	initWith(t, map[string]any{"models": []any{"t-systems-dev/Qwen3.5-9B", "t-systems/gpt-oss-120b"}})
	for in, want := range map[string]string{
		"t-systems-dev/qWeN3.5-9B": "t-systems-dev/Qwen3.5-9B",
		"T-SYSTEMS-DEV/qwen3.5-9b": "t-systems-dev/Qwen3.5-9B",
		"t-systems-dev/Qwen3.5-9B": "t-systems-dev/Qwen3.5-9B",
		"qwen3.5-9B":               "Qwen3.5-9B", // no provider prefix: model part only
		"t-systems/GPT-OSS-120B":   "t-systems/gpt-oss-120b",
		"openai/gpt-4o":            "openai/gpt-4o", // unknown: untouched
	} {
		req := chat(in)
		before := string(req.Body)
		if _, err := HTTPTransportPreHook(nil, req); err != nil {
			t.Fatal(err)
		}
		if got := modelOf(t, req); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
		if in == want && string(req.Body) != before {
			t.Errorf("%s: body changed although the model was already canonical", in)
		}
	}
}

func TestLeavesTheRestOfTheBodyAlone(t *testing.T) {
	initWith(t, map[string]any{"models": []any{"t-systems-dev/Qwen3.5-9B"}})
	req := chat("t-systems-dev/qwen3.5-9b")
	_, _ = HTTPTransportPreHook(nil, req)
	var doc map[string]any
	_ = json.Unmarshal(req.Body, &doc)
	msg := doc["messages"].([]any)[0].(map[string]any)["content"]
	if msg != "Qwen3.5-9B said <b>hi</b>" || doc["stream"] != true {
		t.Fatalf("body changed beyond the model: %s", req.Body)
	}
}

func TestAmbiguousModelPartIsLeftAlone(t *testing.T) {
	initWith(t, map[string]any{"models": []any{"a/Model-X", "b/model-x"}})
	req := chat("MODEL-X")
	_, _ = HTTPTransportPreHook(nil, req)
	if got := modelOf(t, req); got != "MODEL-X" {
		t.Fatalf("ambiguous model rewritten to %s", got)
	}
}

func TestSkipsNonJSONAndMissingModel(t *testing.T) {
	initWith(t, map[string]any{"models": []any{"t-systems-dev/Qwen3.5-9B"}})
	multipart := &schemas.HTTPRequest{Headers: map[string]string{"Content-Type": "multipart/form-data; boundary=x"}, Body: []byte(`--x\r\nmodel: t-systems-dev/qwen3.5-9b`)}
	before := string(multipart.Body)
	_, _ = HTTPTransportPreHook(nil, multipart)
	if string(multipart.Body) != before {
		t.Fatal("multipart body changed")
	}
	noModel := &schemas.HTTPRequest{Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"input":"x"}`)}
	_, _ = HTTPTransportPreHook(nil, noModel)
	if string(noModel.Body) != `{"input":"x"}` {
		t.Fatal("body without model changed")
	}
}

func TestDatasheetKeysAreCanonicalNames(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"t-systems-dev/Qwen3.5-9B": {"mode": "chat"}, "t-systems/gpt-oss-120b": {"mode": "chat"}}`))
	}))
	defer srv.Close()
	initWith(t, map[string]any{"datasheet_url": srv.URL, "models": []any{"extra/Static-Model"}})

	for in, want := range map[string]string{"t-systems-dev/qwen3.5-9b": "t-systems-dev/Qwen3.5-9B", "EXTRA/static-model": "extra/Static-Model"} {
		req := chat(in)
		_, _ = HTTPTransportPreHook(nil, req)
		if got := modelOf(t, req); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("datasheet read %d times at Init, want 1", calls.Load())
	}
}

func TestUnreachableDatasheetKeepsStaticNames(t *testing.T) {
	initWith(t, map[string]any{"datasheet_url": "http://127.0.0.1:1/v1/datasheet", "timeout_ms": float64(200), "models": []any{"t-systems-dev/Qwen3.5-9B"}})
	req := chat("t-systems-dev/QWEN3.5-9B")
	_, _ = HTTPTransportPreHook(nil, req)
	if got := modelOf(t, req); got != "t-systems-dev/Qwen3.5-9B" {
		t.Fatalf("got %s", got)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []any{nil, map[string]any{}, map[string]any{"models": "x"}, map[string]any{"datasheet_url": "ftp://x"}, map[string]any{"models": []any{"a/b"}, "modles": []any{}}} {
		if err := Init(cfg); err == nil {
			t.Errorf("accepted %v", cfg)
			_ = Cleanup()
		}
	}
}

// Bifrost reloads a plugin by running the new instance's Init, then the old instance's
// Cleanup; a .so reopened from the same path shares this state.
func TestReloadOrderKeepsTheNewNames(t *testing.T) {
	if err := Init(map[string]any{"models": []any{"old/Model"}}); err != nil {
		t.Fatal(err)
	}
	if err := Init(map[string]any{"models": []any{"new/Model"}}); err != nil {
		t.Fatal(err)
	}
	_ = Cleanup()
	req := chat("NEW/model")
	_, _ = HTTPTransportPreHook(nil, req)
	if got := modelOf(t, req); got != "new/Model" {
		t.Fatalf("got %s after reload", got)
	}
	_ = Cleanup()
	if active.Load() != nil {
		t.Fatal("last Cleanup did not stop the plugin")
	}
}

func BenchmarkCanonicalModel(b *testing.B) {
	if err := Init(map[string]any{"models": []any{"t-systems-dev/Qwen3.5-9B"}}); err != nil {
		b.Fatal(err)
	}
	defer Cleanup()
	req := chat("t-systems-dev/Qwen3.5-9B")
	b.ReportAllocs()
	for b.Loop() {
		_, _ = HTTPTransportPreHook(nil, req)
	}
}
