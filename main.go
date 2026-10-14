// Command joke-header is a Bifrost v2.3.0 dynamic (.so) plugin that stamps an
// x-joke-header on every response served through the inference routes. It does
// nothing useful on purpose: it exists to prove that a custom plugin builds
// against, downloads into, and loads inside a dynamically linked Bifrost.
//
// Build it with build.sh, never a bare `go build`: a Go plugin only loads into
// a host built by the same Go toolchain, the same -trimpath setting, the same
// libc and the same version of every shared dependency.
package main

import (
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

const (
	pluginName = "joke-header"
	headerName = "x-joke-header"
)

// Version is stamped by build.sh through -ldflags "-X main.Version=...".
var Version = "dev"

var jokes = []string{
	"I would tell you a UDP joke, but you might not get it.",
	"There are 10 kinds of people: those who understand binary and those who do not.",
	"A SQL query walks into a bar, walks up to two tables and asks: may I join you?",
	"Why do programmers prefer dark mode? Because light attracts bugs.",
	"It works on my machine. Then we will ship your machine.",
	"I have a joke about rate limits, but you will have to wait 60 seconds.",
	"Knock knock. Race condition. Who is there?",
	"A plugin walks into a gateway. The gateway says: different version of package.",
}

// fixedJoke, when set through the plugin config, replaces the rotating jokes.
// It is written once in Init, before Bifrost calls any hook.
var fixedJoke string

// Init receives the plugin's "config" object from Bifrost. Every key is
// optional; {"joke": "..."} pins the header to one value.
func Init(config any) error {
	if cfg, ok := config.(map[string]any); ok {
		if joke, ok := cfg["joke"].(string); ok {
			fixedJoke = headerSafe(joke)
		}
	}
	fmt.Printf("[%s] plugin %s loaded, stamping %s on inference responses\n", pluginName, Version, headerName)
	return nil
}

// GetName is required by the Bifrost plugin loader.
func GetName() string {
	return pluginName
}

// HTTPTransportResponseHeadersHook runs right before Bifrost commits response
// headers, for regular, streaming and short-circuited responses alike, which
// makes it the one hook whose headers reach every client.
func HTTPTransportResponseHeadersHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponseMetadata) error {
	resp.SetHeader(headerName, pickJoke())
	return nil
}

// Cleanup is required by the Bifrost plugin loader.
func Cleanup() error {
	return nil
}

func pickJoke() string {
	if fixedJoke != "" {
		return fixedJoke
	}
	return jokes[rand.IntN(len(jokes))]
}

// headerSafe keeps a configured value to printable ASCII so it cannot break
// header framing.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}
