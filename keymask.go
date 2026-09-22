package main

// API key masking for display.
//
// The dashboard shows client keys in the traffic table, the request drawer and
// the key-usage chart. Showing them in full leaks a working credential to
// anyone who can see the screen, so every display path renders a mask instead:
// the first 6 characters, six asterisks, then the last 2.
//
//   sk-a1b2c3d4e5f60718-9abcdef0  ->  sk-a1b******f0
//
// A key short enough that the mask would hide nothing is masked harder rather
// than echoed back: anything at or below 10 characters keeps only its first
// character, so a short key never round-trips in full.

import "strings"

// maskAPIKey renders a client key for display. The mask is applied to the
// ORIGINAL string: leading/trailing spaces are not stripped first, because a
// key's exact spelling is what identifies it and silently trimming would make
// two different stored keys look identical.
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	const (
		head     = 6
		tail     = 2
		asterisk = "******"
	)
	runes := []rune(key)
	// Too short to mask meaningfully: keep one character so the row is still
	// recognisable, but never reveal the rest.
	if len(runes) <= head+tail {
		if len(runes) <= 1 {
			return asterisk
		}
		return string(runes[:1]) + asterisk
	}
	return string(runes[:head]) + asterisk + string(runes[len(runes)-tail:])
}

// maskAPIKeyForDisplay is the entry point used by the display paths. It keeps
// an already-masked value untouched so a second pass cannot double-mask.
func maskAPIKeyForDisplay(key string) string {
	if key == "" {
		return ""
	}
	if strings.Contains(key, "******") {
		return key
	}
	return maskAPIKey(key)
}
