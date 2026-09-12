package main

import (
	"encoding/json"
	"testing"
)

// TestProvidersActiveOnly verifies /providers returns only enabled providers that have keys.
func TestProvidersActiveOnly(t *testing.T) {
	all := readCPAConfigProviders()
	if len(all) == 0 {
		t.Fatal("config.yaml not readable in this environment; cannot verify")
	}

	disabledCount, noKeyCount := 0, 0
	for _, p := range all {
		if p.Disabled {
			disabledCount++
		}
		if len(p.Keys) == 0 {
			noKeyCount++
		}
	}

	raw, err := providersGet(managementRequest{})
	if err != nil {
		t.Fatalf("providersGet error: %v", err)
	}

	var env struct {
		Result struct {
			StatusCode int    `json:"StatusCode"`
			Body       []byte `json:"Body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Result.StatusCode != 200 {
		t.Fatalf("unexpected status %d, body=%s", env.Result.StatusCode, env.Result.Body)
	}

	var payload struct {
		Providers []cpaConfigProvider `json:"providers"`
	}
	if err := json.Unmarshal(env.Result.Body, &payload); err != nil {
		t.Fatalf("unmarshal body: %v (raw=%s)", err, env.Result.Body)
	}

	got := payload.Providers
	want := len(all) - disabledCount - noKeyCount

	for _, p := range got {
		if p.Disabled {
			t.Errorf("DISABLED provider leaked into response: %s", p.Name)
		}
		if len(p.Keys) == 0 {
			t.Errorf("provider without keys leaked: %s", p.Name)
		}
		t.Logf("  OK active provider: %-12s keys=%d url=%s", p.Name, len(p.Keys), p.BaseURL)
	}

	if len(got) != want {
		t.Errorf("count mismatch: got %d, want %d (total=%d disabled=%d nokeys=%d)",
			len(got), want, len(all), disabledCount, noKeyCount)
	}
}
