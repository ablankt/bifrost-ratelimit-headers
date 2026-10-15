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
	"strings"
	"time"
)

// flagSource reads the rules from a GO Feature Flag relay proxy. It evaluates
// the flag rather than reading its raw configuration, so targeting, disable
// and scheduled rollouts are the relay's to apply.
type flagSource struct {
	evalURL string
	key     string
	client  *http.Client
}

func newFlagSource(baseURL, key string, timeout time.Duration) (*flagSource, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("flag_url must be an absolute http(s) URL, got %q", baseURL)
	}
	return &flagSource{
		evalURL: u.String() + "/v1/feature/" + url.PathEscape(key) + "/eval",
		key:     key,
		// The relay is cluster-internal: connect directly even when the pod sets
		// HTTP(S)_PROXY for provider traffic.
		client: &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, IdleConnTimeout: 4 * time.Second}},
	}, nil
}

// errNotServed is the relay's authoritative answer that the flag serves no
// rules: it is disabled, missing, or its variation is null.
var errNotServed = errors.New("flag not served")

// fetch returns the flag's value. errNotServed means the relay answered and
// there are no rules; any other error means it could not answer.
func (f *flagSource) fetch() (json.RawMessage, error) {
	body := []byte(`{"evaluationContext":{"key":"bifrost","custom":{"service":"bifrost"}},"defaultValue":null}`)
	ctx, cancel := context.WithTimeout(context.Background(), f.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.evalURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Failed    bool            `json:"failed"`
		Reason    string          `json:"reason"`
		ErrorCode string          `json:"errorCode"`
		Value     json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("unreadable relay response: %w", err)
	}
	switch {
	case result.Failed && result.ErrorCode == "FLAG_NOT_FOUND":
		return nil, errNotServed
	case result.Failed:
		return nil, fmt.Errorf("relay could not evaluate the flag: %s", result.ErrorCode)
	case result.Reason == "DISABLED", len(result.Value) == 0, string(result.Value) == "null":
		return nil, errNotServed
	}
	return result.Value, nil
}
