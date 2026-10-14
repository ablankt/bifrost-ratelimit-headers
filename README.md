# bifrost-ratelimit-headers

Custom [Bifrost](https://github.com/maximhq/bifrost) plugins, built for **Bifrost v2.3.0**
(dynamically linked build, including the enterprise image).

| Plugin | What it does | Latest release |
|---|---|---|
| [`api-key-mapper`](#api-key-mapper) | Accepts API keys issued for the previous gateway by swapping in the Bifrost virtual key they map to | `api-key-mapper-v0.1.2` |
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
