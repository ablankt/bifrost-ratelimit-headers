# bifrost-ratelimit-headers

Custom [Bifrost](https://github.com/maximhq/bifrost) plugins, built for **Bifrost v2.3.0**
(dynamically linked build).

Current release: **`joke-header`**, a smoke-test plugin. It adds an `x-joke-header` to every response
on Bifrost's inference routes (`/v1/...`), including streaming and error responses. Use it to
confirm that your Bifrost can download and load a custom `.so` before deploying a plugin that
does real work.

## Use it

In Bifrost go to **Plugins → Add plugin**, or edit `config.json`. Set the path to the release asset
that matches your Bifrost binary:

| Bifrost binary built on | Path |
|---|---|
| Alpine / musl, x86_64 | `https://github.com/ablankt/bifrost-ratelimit-headers/releases/download/v0.1.0/joke-header-linux-amd64-musl.so` |
| Alpine / musl, arm64 | `https://github.com/ablankt/bifrost-ratelimit-headers/releases/download/v0.1.0/joke-header-linux-arm64-musl.so` |
| Debian/Ubuntu / glibc, x86_64 | `https://github.com/ablankt/bifrost-ratelimit-headers/releases/download/v0.1.0/joke-header-linux-amd64-glibc.so` |
| Debian/Ubuntu / glibc, arm64 | `https://github.com/ablankt/bifrost-ratelimit-headers/releases/download/v0.1.0/joke-header-linux-arm64-glibc.so` |

```json
{
  "plugins": [
    {
      "enabled": true,
      "name": "joke-header",
      "path": "https://github.com/ablankt/bifrost-ratelimit-headers/releases/download/v0.1.0/joke-header-linux-amd64-musl.so",
      "config": {}
    }
  ]
}
```

`config` is optional. `{"joke": "..."}` pins the header to a fixed value instead of rotating jokes.

To find out which variant you need, run this inside the Bifrost container:

```sh
uname -m                         # x86_64 -> amd64, aarch64 -> arm64
ldd /app/main 2>&1 | grep libc   # libc.musl-* -> musl, libc.so.6 -> glibc
```

If the binary turns out to be statically linked, it cannot load any plugin.

Verify:

```sh
curl -si http://<bifrost>/v1/models | grep -i x-joke-header
# X-Joke-Header: I would tell you a UDP joke, but you might not get it.
```

The Bifrost log shows `[joke-header] plugin v0.1.0 loaded ...` followed by `plugin status: joke-header - active`.

## Compatibility rules (why there are 4 files)

A Go plugin loads only into a host binary built with exactly the same:

* **Go toolchain**: 1.27.0, as used for Bifrost v2.3.0.
* **Dependency versions**: `go.mod`/`go.sum` here are Bifrost's own `transports/go.mod`/`go.sum` at
  tag `transports/v2.3.0`, so every shared module (core v1.12.0, sonic, fasthttp, ...) matches.
* **`-trimpath`**: Bifrost's Dockerfile, Makefile (`DYNAMIC=1`) and docs all build with `-trimpath`,
  so these plugins do too. If your binary was built without it, rebuild the plugin without it.
* **libc** (musl vs glibc) and **CPU architecture**.

If any of these differ, Bifrost logs `plugin was built with a different version of package ...`.
**Rebuild on every Bifrost upgrade.**

## Build

Requires Docker. Each variant builds in the same pinned image family that Bifrost's release uses.

```sh
./build.sh v0.1.0                     # all four -> dist/
TARGETS="amd64/musl" ./build.sh       # just one
```

## Tested

Each test host was a dynamically linked Bifrost v2.3.0 built from the `transports/v2.3.0` tag with
the docs' Option A (Alpine/musl) or Option B (Debian/glibc) recipe:

| Variant | Loaded from | Result |
|---|---|---|
| arm64 musl | local path and this release's URL | active; header present on non-streaming, streaming, `/v1/models` and error responses |
| arm64 glibc | local path | active; same |
| amd64 musl | this release's URL | active; header present |
| amd64 glibc | not load-tested | built with the same pinned toolchain and flags |
