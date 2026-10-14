package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

const (
	legacyKey  = "legacy-team-key-123"
	mappedVK   = "sk-bf-mapped-virtual-key"
	mappedVKID = "vk-123"
)

// fakeMappingService mimics api-key-mapping's resolve endpoint.
func fakeMappingService(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != defaultResolvePath {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		if body["apiKey"] != legacyKey {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"mapping_not_found","message":"Mapping not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiKeyRootId":           "root-1",
			"apiKey":                 legacyKey,
			"projectUuid":            "00000000-0000-0000-0000-000000000001",
			"bifrostVirtualKeyId":    mappedVKID,
			"bifrostVirtualKeyValue": mappedVK,
			"appliedSourceVersion":   1,
			"createdAt":              "2026-10-14T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testMapper(t *testing.T, url string, extra map[string]any) *mapper {
	t.Helper()
	cfg := map[string]any{"mapping_url": url}
	for k, v := range extra {
		cfg[k] = v
	}
	m, err := newMapper(cfg)
	if err != nil {
		t.Fatalf("newMapper: %v", err)
	}
	return m
}

func request(headers map[string]string) *schemas.HTTPRequest {
	return &schemas.HTTPRequest{Method: "POST", Path: "/v1/chat/completions", Headers: headers}
}

func TestMapsBearerKey(t *testing.T) {
	srv, calls := fakeMappingService(t, 0)
	m := testMapper(t, srv.URL, nil)

	req := request(map[string]string{"Authorization": "Bearer " + legacyKey, "Content-Type": "application/json"})
	m.translate(nil, req)

	if got := req.Headers["Authorization"]; got != "Bearer "+mappedVK {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Headers[virtualKeyHeader]; got != mappedVK {
		t.Fatalf("x-bf-vk = %q", got)
	}
	if got := req.CaseInsensitiveHeaderLookup(markerHeader); got != statusMapped {
		t.Fatalf("marker = %q", got)
	}

	// Second request is answered from the cache.
	m.translate(nil, request(map[string]string{"Authorization": "Bearer " + legacyKey}))
	if calls.Load() != 1 {
		t.Fatalf("mapping service called %d times, want 1", calls.Load())
	}
}

func TestMapsOtherKeyHeaders(t *testing.T) {
	srv, _ := fakeMappingService(t, 0)
	m := testMapper(t, srv.URL, nil)
	for _, h := range []string{"X-Api-Key", "Api-Key", "X-Goog-Api-Key"} {
		req := request(map[string]string{h: legacyKey})
		m.translate(nil, req)
		if req.Headers[h] != mappedVK || req.Headers[virtualKeyHeader] != mappedVK {
			t.Fatalf("%s: headers = %v", h, req.Headers)
		}
	}
}

func TestMapsBareAuthorizationKey(t *testing.T) {
	srv, _ := fakeMappingService(t, 0)
	m := testMapper(t, srv.URL, nil)
	req := request(map[string]string{"Authorization": legacyKey})
	m.translate(nil, req)
	if got := req.Headers["Authorization"]; got != "Bearer "+mappedVK {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestRetriesOnceWhenConnectionDrops(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// What a keep-alive connection the server just closed looks like: no response.
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"bifrostVirtualKeyId":"` + mappedVKID + `","bifrostVirtualKeyValue":"` + mappedVK + `"}`))
	}))
	t.Cleanup(srv.Close)
	m := testMapper(t, srv.URL, nil)
	if res := m.resolve(legacyKey); res.status != statusMapped || calls.Load() != 2 {
		t.Fatalf("status = %q after %d calls, want mapped after 2", res.status, calls.Load())
	}
}

func TestLeavesBifrostCredentialsAlone(t *testing.T) {
	srv, calls := fakeMappingService(t, 0)
	m := testMapper(t, srv.URL, nil)
	cases := []map[string]string{
		{"Authorization": "Bearer sk-bf-already-a-virtual-key"},
		{"X-Api-Key": "SK-BF-upper"},
		{"X-Bf-Vk": "anything", "Authorization": "Bearer " + legacyKey},
		{"Authorization": "Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"},
		{"Authorization": "Basic dXNlcjpwYXNz"},
		{},
	}
	for _, headers := range cases {
		before := len(headers)
		req := request(headers)
		m.translate(nil, req)
		if len(req.Headers) != before {
			t.Fatalf("request %v was modified", headers)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("mapping service called %d times, want 0", calls.Load())
	}
}

func TestUnknownKeyPassesThroughAndIsNegativelyCached(t *testing.T) {
	srv, calls := fakeMappingService(t, 0)
	m := testMapper(t, srv.URL, nil)
	for range 3 {
		req := request(map[string]string{"Authorization": "Bearer unknown-key"})
		m.translate(nil, req)
		if req.Headers["Authorization"] != "Bearer unknown-key" || req.Headers[virtualKeyHeader] != "" {
			t.Fatalf("unknown key rewritten: %v", req.Headers)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("mapping service called %d times, want 1", calls.Load())
	}
}

func TestClientMarkerIsBlanked(t *testing.T) {
	srv, _ := fakeMappingService(t, 0)
	m := testMapper(t, srv.URL, nil)
	req := request(map[string]string{"Authorization": "Bearer unknown-key", "X-Key-Mapping-Status": statusMapped})
	m.translate(nil, req)
	if req.CaseInsensitiveHeaderLookup(markerHeader) != "" {
		t.Fatalf("spoofed marker survived: %v", req.Headers)
	}
}

func TestServiceDownLeavesRequestUntouched(t *testing.T) {
	srv, _ := fakeMappingService(t, 0)
	url := srv.URL
	srv.Close()
	m := testMapper(t, url, map[string]any{"timeout_ms": float64(200)})
	req := request(map[string]string{"Authorization": "Bearer " + legacyKey})
	m.translate(nil, req)
	if req.Headers["Authorization"] != "Bearer "+legacyKey || req.Headers[virtualKeyHeader] != "" {
		t.Fatalf("request rewritten while service down: %v", req.Headers)
	}
}

func TestWrongPathIsNotTreatedAsUnknownKey(t *testing.T) {
	srv, _ := fakeMappingService(t, 0)
	m := testMapper(t, srv.URL, map[string]any{"resolve_path": "/wrong"})
	if res := m.resolve(legacyKey); res.status != statusUnavailable {
		t.Fatalf("status = %q, want %q", res.status, statusUnavailable)
	}
}

func TestConcurrentRequestsShareOneLookup(t *testing.T) {
	srv, calls := fakeMappingService(t, 100*time.Millisecond)
	m := testMapper(t, srv.URL, nil)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := m.resolve(legacyKey); res.status != statusMapped {
				t.Errorf("status = %q", res.status)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("mapping service called %d times, want 1", calls.Load())
	}
}

func TestResponseHeaderHook(t *testing.T) {
	srv, _ := fakeMappingService(t, 0)
	active.Store(testMapper(t, srv.URL, nil))
	t.Cleanup(func() { active.Store(nil) })

	req := request(map[string]string{"Authorization": "Bearer " + legacyKey})
	if _, err := HTTPTransportPreAuthHook(nil, req); err != nil {
		t.Fatal(err)
	}
	resp := &schemas.HTTPResponseMetadata{Headers: map[string]string{}}
	if err := HTTPTransportResponseHeadersHook(nil, req, resp); err != nil {
		t.Fatal(err)
	}
	if resp.Header(statusHeader) != statusMapped {
		t.Fatalf("response headers = %v", resp.Headers)
	}
}

// Bifrost reloads a plugin by running the new instance's Init, then the old
// instance's Cleanup; a .so reopened from the same file shares one Init/Cleanup.
func TestReloadOrderKeepsNewConfig(t *testing.T) {
	srv, _ := fakeMappingService(t, 0)
	t.Cleanup(func() { active.Store(nil) })
	if err := Init(map[string]any{"mapping_url": "http://old-mapping:8080"}); err != nil {
		t.Fatal(err)
	}
	if err := Init(map[string]any{"mapping_url": srv.URL}); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(); err != nil {
		t.Fatal(err)
	}
	req := request(map[string]string{"Authorization": "Bearer " + legacyKey})
	if _, err := HTTPTransportPreAuthHook(nil, req); err != nil {
		t.Fatal(err)
	}
	if req.Headers[virtualKeyHeader] != mappedVK {
		t.Fatalf("not mapped after reload: %v", req.Headers)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []any{nil, map[string]any{}, map[string]any{"mapping_url": "not a url"}, map[string]any{"mapping_url": "env.API_KEY_MAPPER_TEST_UNSET"}} {
		if _, err := newMapper(cfg); err == nil {
			t.Fatalf("config %v accepted", cfg)
		}
	}
	t.Setenv("API_KEY_MAPPER_TEST_URL", "http://mapping:8080/")
	m, err := newMapper(map[string]any{"mapping_url": "env.API_KEY_MAPPER_TEST_URL"})
	if err != nil || m.resolveURL != "http://mapping:8080"+defaultResolvePath {
		t.Fatalf("resolveURL = %q, err = %v", m.resolveURL, err)
	}
}
