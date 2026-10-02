package main

import (
	"os"
	"testing"
)

// TestReadCPAConfigProvidersLive exercises the reader end to end through the
// real file-resolution order, which is what the Tools view depends on.
//
// The container mounts the live config at /CLIProxyAPI/config.yaml — the first
// candidate findCPAConfigFile() tries — so this runs the exact path the plugin
// takes on the host. It is the regression guard for the Tools view losing every
// provider after CPA v8 moved openai-compatibility under api-keys: the reader
// only understood the v7 layout, so it returned an empty list and the UI showed
// "Không tìm thấy provider nào trong config.yaml".
func TestReadCPAConfigProvidersLive(t *testing.T) {
	path := "/CLIProxyAPI/config.yaml"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("live config not mounted at %s", path)
	}
	all := readCPAConfigProviders()
	t.Logf("readCPAConfigProviders() returned %d providers", len(all))
	active := 0
	for _, p := range all {
		t.Logf("  name=%-24s disabled=%-5v keys=%-2d models=%-3d url=%s",
			p.Name, p.Disabled, len(p.Keys), len(p.Models), p.BaseURL)
		if !p.Disabled && len(p.Keys) > 0 {
			active++
		}
	}
	if len(all) == 0 {
		t.Fatalf("0 providers parsed: the Tools view would render " +
			"'Không tìm thấy provider nào trong config.yaml'")
	}
	if active == 0 {
		t.Fatalf("0 enabled providers with a key: the Tools view would render an empty list")
	}
	t.Logf("active providers with keys: %d", active)
}
