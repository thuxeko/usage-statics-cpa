package main

import (
	"os"
	"testing"
)

// v8 layout, as CPA v8 + the management panel write it: the openai-compatibility
// groups moved under api-keys, the credential list is now "keys", and the panel
// appends "name" after the models.
const v8Layout = `config-version: 8
api-keys:
  claude: []
  openai-compatibility:
    - "base-url": "https://vsllm.cc/v1"
      "disabled": false
      "keys":
        - "api-key": "sk-v8-key"
          "weight": 2
      "models":
        - "alias": ""
          "name": "kimi-k3"
      "name": "vsllm"
      "prefix": "vsl"
`

// v8 group WITHOUT a name: the panel can write one, but an older write may not
// have. The entry must still be listed (named after its endpoint) instead of
// silently disappearing from the Tools view.
const v8LayoutNoName = `config-version: 8
api-keys:
  openai-compatibility:
    - "base-url": "https://api.a6api.com/v1"
      "disabled": false
      "keys":
        - "api-key": "sk-a6"
      "models":
        - "name": "deepseek-v4.1-flash"
`

// v7 layout, kept working so an un-migrated config still resolves providers.
const v7Layout = `openai-compatibility:
  - name: vsllm
    prefix: vsl
    base-url: https://vsllm.cc/v1
    api-key-entries:
      - api-key: sk-v7-key
        weight: 2
    models:
      - name: kimi-k3
        alias: ""
`

func TestParseCPAConfigProviders(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantName string
		wantKey  string
	}{
		{"v8 layout (live)", v8Layout, "vsllm", "sk-v8-key"},
		{"v8 layout without name", v8LayoutNoName, "a6api", "sk-a6"},
		{"v7 layout (legacy)", v7Layout, "vsllm", "sk-v7-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCPAConfigProviders([]byte(tc.raw))
			if len(got) != 1 {
				t.Fatalf("providers = %d, want 1 (%+v)", len(got), got)
			}
			p := got[0]
			if p.Name != tc.wantName {
				t.Errorf("name = %q, want %q", p.Name, tc.wantName)
			}
			if len(p.Keys) != 1 || p.Keys[0] != tc.wantKey {
				t.Errorf("keys = %v, want [%s]", p.Keys, tc.wantKey)
			}
			if len(p.Models) != 1 {
				t.Errorf("models = %v, want 1 entry", p.Models)
			}
		})
	}
}

func TestProviderNameFromBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://vsllm.cc/v1":                  "vsllm",
		"https://api.a6api.com/v1":             "a6api",
		"https://inference-api.nousresearch.com/v1": "inference-api",
		"http://host.docker.internal:8788/v1":  "host",
		"https://kiro.pix4k.com":               "kiro",
		"":                                     "",
	}
	for in, want := range cases {
		if got := providerNameFromBaseURL(in); got != want {
			t.Errorf("providerNameFromBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The real config on the host, when CPA_CONFIG_TEST points at a copy of it.
// This is the regression guard for the Tools view losing every provider after
// CPA v8 moved openai-compatibility under api-keys.
func TestParseCPAConfigProvidersRealFile(t *testing.T) {
	path := os.Getenv("CPA_CONFIG_TEST")
	if path == "" {
		t.Skip("CPA_CONFIG_TEST not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := parseCPAConfigProviders(data)
	t.Logf("parsed %d providers from %s", len(got), path)
	usable := 0
	for _, p := range got {
		t.Logf("  name=%-26s disabled=%-5v keys=%-2d models=%-3d url=%s",
			p.Name, p.Disabled, len(p.Keys), len(p.Models), p.BaseURL)
		if !p.Disabled && len(p.Keys) > 0 {
			usable++
		}
	}
	if len(got) == 0 {
		t.Fatalf("0 providers parsed from the live config: the Tools view would show " +
			"'Không tìm thấy provider nào trong config.yaml'")
	}
	if usable == 0 {
		t.Fatalf("0 ENABLED providers with a key: the Tools view would show an empty list")
	}
	t.Logf("active providers with keys: %d", usable)
}
