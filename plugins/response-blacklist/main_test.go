package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// fakeRelay answers like GO Feature Flag v1.55's /v1/feature/{key}/eval.
func fakeRelay(t *testing.T, answer *atomic.Pointer[string], status *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/feature/"+defaultFlagKey+"/eval" {
			http.NotFound(w, r)
			return
		}
		if code := status.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		_, _ = w.Write([]byte(*answer.Load()))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	servedV1 = `{"failed":false,"reason":"STATIC","errorCode":"","value":{"body":{"remove":["extra_fields.provider"]}}}`
	servedV2 = `{"failed":false,"reason":"STATIC","errorCode":"","value":{"body":{"remove":["extra_fields.provider","prompt_filter_results"]}}}`
	invalid  = `{"failed":false,"reason":"STATIC","errorCode":"","value":{"body":{"remove":["a..b"]}}}`
	disabled = `{"failed":false,"reason":"DISABLED","errorCode":"","value":null}`
	missing  = `{"failed":true,"reason":"ERROR","errorCode":"FLAG_NOT_FOUND","value":null}`
)

func TestFlagLifecycle(t *testing.T) {
	var answer atomic.Pointer[string]
	var status atomic.Int32
	set := func(s string) { answer.Store(&s) }
	set(servedV1)
	srv := fakeRelay(t, &answer, &status)

	b, err := newBlacklist(map[string]any{
		"flag_url": srv.URL,
		"rules":    map[string]any{"headers": map[string]any{"remove": []any{"x-inline"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	removals := func() int { return len(b.current().bodyRemove) }

	b.refresh()
	if removals() != 1 {
		t.Fatalf("served flag not applied: %s", b.current().describe())
	}
	set(servedV2)
	b.refresh()
	if removals() != 2 {
		t.Fatal("changed flag not applied")
	}
	status.Store(http.StatusInternalServerError)
	b.refresh()
	if removals() != 2 {
		t.Fatal("relay outage dropped the rules")
	}
	status.Store(0)
	set(invalid)
	b.refresh()
	if removals() != 2 {
		t.Fatal("invalid flag replaced valid rules")
	}
	for _, notServed := range []string{disabled, missing} {
		set(servedV1)
		b.refresh()
		set(notServed)
		b.refresh()
		if b.current() != b.inline {
			t.Fatalf("%s did not hand over to the plugin config's rules", notServed)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []any{nil, map[string]any{}, map[string]any{"flag_url": "not a url"}, map[string]any{"rules": map[string]any{"body": map[string]any{"remove": []any{""}}}}} {
		if _, err := newBlacklist(cfg); err == nil {
			t.Errorf("config %v accepted", cfg)
		}
	}
}

// Bifrost reloads a plugin by running the new instance's Init, then the old
// instance's Cleanup; a .so reopened from the same file shares this state.
func TestReloadOrderKeepsNewInstance(t *testing.T) {
	t.Cleanup(func() { active.Store(nil); live.Store(0) })
	inline := func(header string) map[string]any {
		return map[string]any{"rules": map[string]any{"headers": map[string]any{"remove": []any{header}}}}
	}
	if err := Init(inline("x-old")); err != nil {
		t.Fatal(err)
	}
	if err := Init(inline("x-new")); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(); err != nil {
		t.Fatal(err)
	}
	resp := &schemas.HTTPResponseMetadata{Headers: map[string]string{"X-New": "1", "X-Old": "1"}}
	if err := HTTPTransportResponseHeadersHook(nil, nil, resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Headers["X-New"]; ok || resp.Headers["X-Old"] != "1" {
		t.Fatalf("headers = %v, want the new rules applied", resp.Headers)
	}
	if err := Cleanup(); err != nil || active.Load() != nil {
		t.Fatal("last Cleanup did not stop the plugin")
	}
}

func TestPostHookOnlyTouchesJSON(t *testing.T) {
	t.Cleanup(func() { active.Store(nil); live.Store(0) })
	if err := Init(map[string]any{"rules": map[string]any{"body": map[string]any{"remove": []any{"extra_fields"}}}}); err != nil {
		t.Fatal(err)
	}
	jsonResp := &schemas.HTTPResponse{Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"a":1,"extra_fields":{}}`)}
	_ = HTTPTransportPostHook(nil, nil, jsonResp)
	if string(jsonResp.Body) != `{"a":1}` {
		t.Errorf("json body = %s", jsonResp.Body)
	}
	audio := []byte(`{"extra_fields": "this is not JSON, it only looks like it"`)
	binResp := &schemas.HTTPResponse{Headers: map[string]string{"Content-Type": "audio/mpeg"}, Body: audio}
	_ = HTTPTransportPostHook(nil, nil, binResp)
	if string(binResp.Body) != string(audio) {
		t.Error("non-JSON body was modified")
	}
}
