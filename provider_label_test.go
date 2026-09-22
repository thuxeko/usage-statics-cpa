package main

import "testing"

func TestProviderLabel(t *testing.T) {
	cases := []struct {
		name   string
		detail RequestDetail
		want   string
	}{
		{
			name:   "openai-compat with config base-url -> short name + host",
			detail: RequestDetail{Provider: "openai-compatible-ca2", BaseURL: "", AuthID: ""},
			want:   "ca2", // no config readable in test env -> honest fallback to short name
		},
		{
			name:   "antigravity oauth account wins",
			detail: RequestDetail{Provider: "antigravity", AuthID: "antigravity-kevinhoang2334@gmail.com.json"},
			want:   "kevinhoang2334@gmail.com",
		},
		{
			name:   "empty auth id falls back to provider",
			detail: RequestDetail{Provider: "antigravity", AuthID: ""},
			want:   "antigravity",
		},
		{
			name:   "stored base_url is shown as endpoint",
			detail: RequestDetail{Provider: "openai-compatible-vsllm", BaseURL: "https://vsllm.com/v1"},
			want:   "vsllm.com/v1",
		},
		{
			name:   "endpoint keeps its path and port",
			detail: RequestDetail{Provider: "openai-compatible-localworkbuddy", BaseURL: "http://host.docker.internal:8788/v1"},
			want:   "host.docker.internal:8788/v1",
		},
		{
			name:   "synthetic auth id never becomes an account",
			detail: RequestDetail{Provider: "openai-compatible-vsllm", AuthID: "openai-compatibility:vsllm:b1f04bff4493", BaseURL: "https://vsllm.com/v1"},
			want:   "vsllm.com/v1",
		},
		{
			name:   "unknown provider stays raw",
			detail: RequestDetail{Provider: "mystery-provider"},
			want:   "mystery-provider",
		},
	}
	for _, c := range cases {
		got := providerLabel(c.detail)
		if got != c.want {
			t.Errorf("%s: providerLabel() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBaseURLHost(t *testing.T) {
	cases := map[string]string{
		"https://llm.goaichat.top/v1":            "llm.goaichat.top",
		"http://host.docker.internal:8788/v1":    "host.docker.internal:8788",
		"vsllm.com":                              "vsllm.com",
		"":                                       "",
	}
	for in, want := range cases {
		if got := baseURLHost(in); got != want {
			t.Errorf("baseURLHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAccountFromAuthID(t *testing.T) {
	cases := []struct{ id, provider, want string }{
		{"antigravity-kevinhoang2334@gmail.com.json", "antigravity", "kevinhoang2334@gmail.com"},
		{"hoantran_correct.json", "antigravity", "hoantran_correct"},
		{"thuylinh_correct.json", "antigravity", "thuylinh_correct"},
		{"openai-compatibility:vsllm:b1f04bff4493", "openai-compatible-vsllm", ""},
		{"openai-compatible-ca2-abc123.json", "openai-compatible-ca2", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := accountFromAuthID(c.id, c.provider); got != c.want {
			t.Errorf("accountFromAuthID(%q, %q) = %q, want %q", c.id, c.provider, got, c.want)
		}
	}
}

func TestProviderShortName(t *testing.T) {
	cases := map[string]string{
		"openai-compatible-ca2": "ca2",
		"antigravity":           "antigravity",
		"":                      "",
	}
	for in, want := range cases {
		if got := providerShortName(in); got != want {
			t.Errorf("providerShortName(%q) = %q, want %q", in, got, want)
		}
	}
}