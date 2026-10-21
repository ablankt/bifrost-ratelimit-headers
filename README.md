# bifrost-ratelimit-headers

Custom [Bifrost](https://github.com/maximhq/bifrost) plugins, built for **Bifrost v2.3.0**
(dynamically linked build, including the enterprise image).

| Plugin | What it does | Latest release |
|---|---|---|
| [`api-key-mapper`](#api-key-mapper) | Accepts API keys issued for the previous gateway by swapping in the Bifrost virtual key they map to | `api-key-mapper-v0.1.2` |
| [`response-blacklist`](#response-blacklist) | Removes and renames response headers and JSON body entries (streamed too) so gateway internals do not reach customers, with rules from a GO Feature Flag flag | `response-blacklist-v0.1.0` |
| [`model-normalizer`](#model-normalizer) | Rewrites the requested model to its canonical spelling (`qWeN3.5-9B` → `Qwen3.5-9B`) so logs and cost attribution match the datasheet | `model-normalizer-v0.1.1` |
| [`vk-metadata-headers`](#vk-metadata-headers) | Test, **Enterprise 2.3.2 only**: copies virtual key metadata into the request headers recorded on the trace (Kafka) | `vk-metadata-headers-v0.1.0` |
| [`joke-header`](#joke-header) | Smoke test: adds `x-joke-header` to every inference response | `v0.1.0` |

Every release ships four builds. Use the one that matches your Bifrost binary:

| Bifrost binary | Asset suffix |
|---|---|
| Alpine / musl, x86_64 | `-linux-amd64-musl.so` |
| Alpine / musl, arm64 | `-linux-arm64-musl.so` |
| Debian/Ubuntu / glibc, x86_64 | `-linux-amd64-glibc.so` |
| Debian/Ubuntu / glibc, arm64 | `-linux-arm64-glibc.so` |

To check yours, run `uname -m` (`x86_64` means amd64, `aarch64` means arm64) and `ldd /app/main | grep libc`
(`libc.musl-*` means musl, `libc.so.6` means glibc) inside the Bifrost container.

---

## api-key-mapper

Clients keep sending the API keys they used with the previous gateway. Before Bifrost authenticates
a request, the plugin asks an api-key-mapping service whether it knows the presented key:

```
POST {mapping_url}/v1/api-key-mappings/resolve     {"apiKey": "<presented key>"}
200 {"bifrostVirtualKeyId": "...", "bifrostVirtualKeyValue": "sk-bf-...", ...}   -> mapped
404 {"code": "mapping_not_found", ...}                                            -> unknown key
```

* **Mapped:** the virtual key replaces the old key in the header it arrived in, and is also set as
  `x-bf-vk`. Bifrost then authenticates, governs and logs the request as that virtual key, and the
  old key is never forwarded to a provider. The response carries `x-key-mapping: mapped` and
  `x-lookup-latency: <n>ms`, the time the lookup added to the request (about `0.0ms` once the key
  is cached). The request's plugin log reads
  `legacy API key mapped to virtual key <id> in <n> ms`, ending in `(cached)` for a cache hit.
* **Unknown key, mapping service down, or misconfigured:** the request goes on unchanged and
  Bifrost's own authentication decides. An unknown key still gets the normal 401.
* **Already a Bifrost credential:** the plugin does not look it up. This covers an `sk-bf-…` key in
  any header, any `x-bf-vk` header, and user tokens (JWTs).

The plugin reads the key from the same headers Bifrost reads virtual keys from:
`Authorization: Bearer …`, `x-api-key`, `api-key` and `x-goog-api-key`. It also accepts a bare
`Authorization: <key>` with no scheme, and rewrites it to Bearer form.

### Configure

```json
{
  "enabled": true,
  "name": "api-key-mapper",
  "path": "https://github.com/ablankt/bifrost-ratelimit-headers/releases/download/api-key-mapper-v0.1.2/api-key-mapper-linux-amd64-musl.so",
  "config": {
    "mapping_url": "http://<mapping-service>.<namespace>.svc.cluster.local:8080"
  }
}
```

| Key | Default | Meaning |
|---|---|---|
| `mapping_url` | *(required)* | Base URL of the mapping service. `env.NAME` reads it from an environment variable. |
| `resolve_path` | `/v1/api-key-mappings/resolve` | Path of the resolve endpoint. |
| `timeout_ms` | `2000` | Per-lookup timeout. |
| `cache_ttl_seconds` | `300` | How long a found mapping is reused. A changed or removed mapping takes effect after at most this long. A deactivated virtual key is refused by Bifrost immediately. |
| `negative_cache_ttl_seconds` | `60` | How long an unknown key is remembered. A newly created mapping can take this long to work. |
| `error_cache_ttl_seconds` | `5` | How long a failed lookup is remembered, so an outage costs one timeout per key per window, not one per request. |
| `cache_max_entries` | `10000` | Cache bound. Entries are keyed by the SHA-256 of the key, so raw legacy keys are never held in memory. |
| `max_concurrent_lookups` | `64` | Concurrent lookups allowed against the mapping service. Concurrent requests with the same key share one lookup. |
| `skip_prefixes` | `["sk-bf-"]` | Keys starting with any of these are treated as Bifrost credentials and never looked up. |
| `status_header` | `true` | Add `x-key-mapping: mapped` and `x-lookup-latency` to the responses of mapped requests. |

The plugin connects to the mapping service directly and ignores `HTTP(S)_PROXY`. Idle connections
are dropped after 4s, before uvicorn's 5s keep-alive closes them, and a lookup whose connection
fails is retried once. It never logs key material. Lookup failures are printed at most once every
30s.

### Adding it or changing its config: restart Bifrost

When `path` is a URL, Bifrost downloads a fresh copy of the `.so` every time the plugin is saved,
but Go cannot load the same plugin twice into one process. Every pod that already loaded it
(even a load whose `Init` failed, for example because the plugin was added before its config was
filled in) then logs `plugin.Open(...): plugin already loaded` and keeps running without the
plugin. Bifrost then rolls the saved row back, and deletes it if the failed save created it, so
the change is not kept either.

So with a URL:

* **First install:** add the plugin with its URL *and* config in a single save, on pods that have
  never loaded it.
* **Config change or new version:** delete the plugin, restart the Bifrost pods
  (`kubectl -n <ns> rollout restart deploy/<bifrost>`), then add it again with the new URL and
  config in a single save. Every version shares one Go plugin path, so a new version also counts
  as "already loaded" in a process that ran an old one.

With a local file path instead of a URL (for example a `.so` placed by an init container), a save
reopens the same file, and Bifrost re-runs `Init` with the new config without a restart.

### Try it

```sh
curl -si https://<bifrost>/v1/chat/completions \
  -H "Authorization: Bearer <old-gateway-key>" -H "content-type: application/json" \
  -d '{"model":"<provider>/<model>","messages":[{"role":"user","content":"hi"}]}' | grep -i -E "^HTTP|x-key-mapping"
```

A mapped key gives your normal response plus `x-key-mapping: mapped`. A 401 means the key is
unknown, or the mapping service could not be reached; check the Bifrost log for `[api-key-mapper]`
warnings.

### Tested

The arm64 musl build was tested in a dynamically linked Bifrost v2.3.0 with
`enforce_auth_on_inference`, governance and a virtual key, against a fake mapping service:

| Request | Result |
|---|---|
| Mapped key, via `Authorization: Bearer` or `x-api-key` | 200 with `x-key-mapping: mapped`; the upstream saw only Bifrost's provider key |
| Same key again | served from the cache, no second lookup |
| Unknown key, unknown key again, or a spoofed `x-key-mapping-status` request header | 401, with one lookup across all three |
| Bifrost virtual key, or no credential | not looked up; normal Bifrost behaviour |
| Mapping service down | 401 in ~0.1s, with a warning in the Bifrost log |
| Config changed through `PUT /api/plugins/...`, `.so` loaded from a local path | new config applied without a restart |

Unit tests (`go test -race ./plugins/...`) cover the same cases, plus the wrong-path guard, concurrent
lookups, a dropped connection being retried, a bare `Authorization` key, and Bifrost's reload order
(new `Init`, then old `Cleanup`).

In the enterprise v2.3.0 dev deployment, the amd64 musl build mapped a real legacy key on every pod
that loaded it.

---

## response-blacklist

Keeps gateway internals out of what customers receive. It removes and renames response headers, and
removes or renames entries of JSON response bodies, in normal and streamed responses alike, on
every inference route (`/v1/...`, `/openai/...`, ...).

The rules come from a [GO Feature Flag](https://gofeatureflag.org) flag, evaluated through the relay
proxy every `refresh_seconds`, so changing them needs no Bifrost restart. While the flag serves no
rules (it is disabled or missing), the plugin applies the `rules` in its own config. While the
relay cannot answer, or serves rules the plugin refuses, it keeps the rules it applied before.

```yaml
blacklist-bifrost:               # the flag; every variation is a rule set
  variations:
    enforced:
      headers:
        remove:                  # regexes the whole header name must match, any case
          - x-bifrost-routing-info-key
        rename:                  # in order, on the lower-cased names that are left
          - pattern: '(?i)bifrost'      # x-bifrost-trace-id -> x-backend-trace-id
            replacement: backend
            target: both                # names (default), values or both
      body:
        remove:                  # JSON paths, whatever their value; * = any key, arrays are walked
          - extra_fields.provider
          - extra_fields.provider_response_headers
          - prompt_filter_results
        rename:
          - pattern: bifrost            # keys (default): is_bifrost_error -> is_backend_error
            replacement: backend
          - pattern: '(?i)bifrost'      # values: string values below paths only, so
            replacement: backend        # model output is never rewritten
            target: values
            paths: [error.message]
    off: {}
  defaultRule:
    variation: enforced
```

Removals run first, using the names Bifrost writes; renames then run in order. Patterns are Go
(RE2) regular expressions, and `$1` in a replacement is the first group. Unknown keys or invalid
patterns make the plugin refuse the whole rule set.

### Configure

```json
{
  "enabled": true,
  "name": "response-blacklist",
  "path": "https://github.com/ablankt/bifrost-ratelimit-headers/releases/download/response-blacklist-v0.1.0/response-blacklist-linux-amd64-musl.so",
  "config": {
    "flag_url": "http://go-feature-flag.go-feature-flag.svc.cluster.local",
    "rules": { "headers": { "rename": [{ "pattern": "bifrost", "replacement": "backend" }] } }
  }
}
```

| Key | Default | Meaning |
|---|---|---|
| `flag_url` | | Base URL of the GO Feature Flag relay proxy (`env.NAME` reads an environment variable). Without it, only `rules` apply. |
| `flag_key` | `blacklist-bifrost` | The flag to evaluate. The evaluation context is key `bifrost`, custom `service: bifrost`, so targeting can tell it apart. |
| `refresh_seconds` | `30` | How often the flag is evaluated. |
| `timeout_ms` | `2000` | Per-evaluation timeout. |
| `rules` | | A rule set in the flag's format, applied while the flag serves none. At least one of `flag_url` and `rules` is required. |

The Bifrost log shows which rules are applied (`[response-blacklist] flag blacklist-bifrost rules
applied: ...`), whenever they change.

### How it reaches each part of a response

* **Headers:** in `HTTPTransportResponseHeadersHook`, right before Bifrost commits them, for normal,
  streamed and error responses.
* **Non-streamed bodies:** in `HTTPTransportPostHook`, on the JSON bytes the client receives. Bodies
  that are not JSON, and bodies without any key a rule names, are left untouched and not
  re-encoded. Numbers keep their exact digits.
* **Stream chunks:** in `HTTPTransportStreamChunkHook`, on Bifrost's typed chunk before it is
  serialized or converted for an integration route. The plugin edits a copy, so Bifrost's own
  chunk (and with it logging) is untouched, and fields Bifrost keeps out of JSON survive for the
  converters. Removing a field Bifrost always writes (no `omitempty`, such as
  `extra_fields.routing_info`) can only empty it; the log reports each such path once. Key renames
  do not apply to stream chunks.

Out of reach: responses to requests Bifrost's authentication refuses (401s run no plugin hooks),
CORS preflight (`OPTIONS`) answers, and very large responses Bifrost streams without buffering.

### Tested

The arm64 musl build was tested in a dynamically linked Bifrost v2.3.0, with the rules served by a
GO Feature Flag v1.55.3 relay proxy from the real `blacklist_bifrost.yaml`, against a fake OpenAI
upstream that sends `prompt_filter_results` and rate-limit headers:

| Check | Result |
|---|---|
| `/v1/chat/completions` and `/openai/v1/chat/completions`, normal and streamed | no header name or value containing `bifrost`; `provider`, `provider_response_headers`, `routing_info.key` and `prompt_filter_results` gone; model output unchanged |
| Flag switched to `off` | everything above back within ~2s, without a restart |
| Flag edited | new rules applied within ~2s, without a restart |

Unit tests cover paths through arrays, wildcards and `json.RawMessage` fields, removal of any value
type, key and value renames, number and HTML preservation, header rules, refused rule sets, that
stream chunks are copied rather than modified, the flag's served, disabled, missing, invalid and
unreachable states, and Bifrost's reload order.

---

## model-normalizer

Clients may send a model in any case (`t-systems-dev/qWeN3.5-9B`). The upstream may accept it, but Bifrost then logs that spelling, and cost attribution against a datasheet fails because datasheet keys are case-sensitive (`t-systems-dev/Qwen3.5-9B`). Lowercasing would not help: the key is mixed case. The plugin rewrites the top-level `"model"` of a JSON request body to the canonical name that matches it case-insensitively, in `HTTPTransportPreHook`, before Bifrost parses the request. Routing, logs and pricing then all see one spelling. The rest of the body is not re-encoded.

```json
{
  "enabled": true,
  "name": "model-normalizer",
  "path": "https://github.com/ablankt/bifrost-ratelimit-headers/releases/download/model-normalizer-v0.1.1/model-normalizer-linux-amd64-musl.so",
  "config": {
    "datasheet_url": "http://portal-api.portal-api.svc.cluster.local:8000/v1/datasheet"
  }
}
```

| Key | Default | Meaning |
|---|---|---|
| `datasheet_url` | | A datasheet: a JSON object keyed by `provider/model` (portal-api's `/v1/datasheet`, `https://getbifrost.ai/datasheet`). Its keys are the canonical names. Read at start and every `refresh_seconds`; a failed read keeps the names it had. `env.NAME` reads an environment variable. |
| `models` | | Extra canonical names, e.g. `["t-systems-dev/Qwen3.5-9B"]`. At least one of `datasheet_url` and `models` is required. |
| `refresh_seconds` | `300` | Datasheet refresh interval (at least 10). |
| `timeout_ms` | `5000` | Datasheet request timeout. |

- Every response carries `x-normaliser-timing`, the time the plugin spent on the request (e.g. `0.007ms` for a model that needed no change). A value the client sends is overwritten.
- `provider/model` matches in full. A model sent without a provider prefix matches on the model part when exactly one canonical name has it.
- Unknown models, bodies that are not JSON (multipart uploads) and models in the URL path (`/genai/...`) are left alone.
- Tested in a dynamically linked Bifrost 2.3.0: `openai/qWeN3.5-9B` and `OPENAI/qwen3.5-9b` reached the upstream and Bifrost's `original_model_requested` as `Qwen3.5-9B`.

---

## vk-metadata-headers

A test of whether a virtual key's CCE identity can travel with the trace. The plugin copies entries
of the presented key's [metadata](https://docs.getbifrost.ai/features/governance/virtual-keys#metadata-per-key)
into the request headers Bifrost records on the request's trace, so a trace connector (the Kafka
plugin's `request_headers`) exports them as if the caller had sent them. The request itself is not
changed: nothing reaches the provider or the response.

Governance puts the key's metadata on the request context in its `PreRequestHook`. In `PreLLMHook`,
which runs after every plugin's `PreRequestHook`, the plugin merges the mapped values into the
request's headers and hands them to the tracer's `SetTraceRequestHeaders`. That setter replaces the
recorded headers, keeps only the names a connector captures, and redacts credentials. A metadata
value wins over a header the caller sent under the same name.

```json
{
  "name": "vk-metadata-headers",
  "enabled": true,
  "path": "https://github.com/ablankt/bifrost-ratelimit-headers/releases/download/vk-metadata-headers-v0.1.0/vk-metadata-headers-linux-amd64-musl.so",
  "config": {
    "headers": {
      "x-cce-key-id": "cce_key_id",
      "x-cce-project-id": "cce_project_id"
    }
  }
}
```

| Key | Default | |
|---|---|---|
| `headers` | *(required)* | Header name → virtual key metadata key. Names are case-insensitive. A header must also be in the Kafka plugin's `request_headers`, or the tracer drops it. |

To test, give a virtual key metadata such as `cce_key_id: hello-world` and `cce_project_id:
hello-project` (`PUT /api/governance/virtual-keys/{id}` with `{"metadata": {...}}`, or the key's
sheet), then send a request with that key directly to Bifrost. The Kafka message then carries
`"x-cce-key-id": "hello-world"` in `RequestHeaders`, and `PluginLogs` has a `vk-metadata-headers`
entry: `trace headers set from virtual key metadata: ...`, or why it set nothing.

Unlike the other plugins, this one targets **Bifrost Enterprise 2.3.2** as deployed on
llm-serving-dev. Its own `go.mod` pins `bifrost/core` v1.13.1 and its `build.env` selects go1.27.3
on Alpine 3.23, amd64 musl only (`./build.sh vk-metadata-headers v0.1.0`); a v2.3.0 build does not
load into 2.3.2, nor the other way round. It relies on two internals no plugin API promises: the
context key `bifrost-governance-virtual-key-metadata` and the tracer's `SetTraceRequestHeaders`. A
Bifrost upgrade can break either, which then shows only as a `PluginLogs` warning. A key without the
mapped metadata keeps whatever the caller sent in those headers.

---

## joke-header

Adds `x-joke-header` to every response on Bifrost's inference routes, including streaming and error
responses, so you can confirm custom plugins load. `config` is optional; `{"joke": "..."}` pins
the value. Path: `https://github.com/ablankt/bifrost-ratelimit-headers/releases/download/v0.1.0/joke-header-linux-amd64-musl.so`
(swap the suffix as above). The amd64 musl build has been confirmed in an enterprise v2.3.0
deployment.

---

## Compatibility rules (why there are 4 files)

A Go plugin loads only into a host binary built with exactly the same:

* **Go toolchain**: 1.27.0, as used for Bifrost v2.3.0.
* **Dependency versions**: `go.mod`/`go.sum` here are Bifrost's own `transports/go.mod`/`go.sum` at
  tag `transports/v2.3.0`, so every shared module (core v1.12.0, sonic, fasthttp, ...) matches.
* **`-trimpath`**: Bifrost's Dockerfile, Makefile (`DYNAMIC=1`) and docs all build with it.
* **libc** (musl vs glibc) and **CPU architecture**.

If any of these differ, Bifrost logs `plugin was built with a different version of package ...`.
**Rebuild on every Bifrost upgrade.**

## Build

Requires Docker. Each variant builds in the pinned image Bifrost's own release uses.

```sh
./build.sh api-key-mapper v0.1.0               # all four -> dist/
TARGETS="amd64/musl" ./build.sh joke-header    # just one
```
