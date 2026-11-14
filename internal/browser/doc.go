// Package browser drives a local Chromium-family browser over the Chrome
// DevTools Protocol (CDP). It is written from scratch on the standard
// library: an RFC 6455 WebSocket client, a pipe transport for
// --remote-debugging-pipe, a CDP connection that multiplexes requests by id
// and routes events by flattened session id, and a small page API for
// navigation, script evaluation, screenshots, screencasting and real input
// (mouse, keyboard with a US key-definition table, text insertion).
//
// A Browser owns one Chromium process started in its own process group with
// a temporary profile directory; Close shuts the browser down gracefully,
// kills whatever is left of the process group and removes the profile.
//
//	b, err := browser.Launch(ctx, browser.Options{Headless: true})
//	if err != nil { ... }
//	defer b.Close()
//	p, err := b.NewPage(ctx)
//	if err := p.Navigate(ctx, "https://example.com"); err != nil { ... }
//	title, err := p.Title(ctx)
//
// The executable is found by FindExecutable: the GTA_BROWSER environment
// variable, then the standard install locations of Chrome, Chromium and Edge
// on Linux, macOS and Windows (and Playwright's browser cache), then a PATH
// lookup.
package browser
