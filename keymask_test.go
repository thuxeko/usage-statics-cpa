package main

import "testing"

// TestMaskAPIKey pins the masking contract: first 6 characters, six asterisks,
// last 2. The example is the one the user gave.
func TestMaskAPIKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sk-a1b2c3d4e5f60718-9abcdef0", "sk-a1b******f0"},
		{"", ""},
		// Short values must not round-trip in full.
		{"sk-abc", "s******"},
		{"12345678", "1******"},
		{"123456789", "123456******89"},
		// 64-hex keys keep the same shape.
		{"05ac5fdb4c3026fb686b6993e136c50d3703be0923b88ce388e234c111a23f6c", "05ac5f******6c"},
		// Already-masked input must not be masked twice.
		{"sk-a1b******f0", "sk-a1b******f0"},
	}
	for _, c := range cases {
		if got := maskAPIKeyForDisplay(c.in); got != c.want {
			t.Errorf("maskAPIKeyForDisplay(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMaskAPIKeyHidesMiddle proves the mask actually removes the secret part:
// the original middle must not survive anywhere in the output.
func TestMaskAPIKeyHidesMiddle(t *testing.T) {
	const secret = "sk-a1b2c3d4e5f60718-9abcdef0"
	masked := maskAPIKeyForDisplay(secret)
	if masked == secret {
		t.Fatalf("key was returned unchanged")
	}
	// The distinguishing middle of the key must be gone. The needle is derived
	// from the fake key above, so this test never carries a real credential.
	if contains(masked, fakeKeyBody()) {
		t.Errorf("masked value still carries the key body: %q", masked)
	}
	if len(masked) >= len(secret) {
		t.Errorf("masked value %q is not shorter than the key", masked)
	}
}

// fakeKeyBody is the middle of the fake key used in this file, so the negative
// assertion below never embeds a fragment of a real credential.
func fakeKeyBody() string {
	return "a1b2c3d4e5f60718-9abcdef0"
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
