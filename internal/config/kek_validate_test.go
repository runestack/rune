package config

import "testing"

func TestValidateKEK(t *testing.T) {
	var c Config
	if err := c.ValidateKEK(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	c.Secret.Encryption.KEK.Source = "generated"
	if err := c.ValidateKEK(); err != nil {
		t.Fatalf("generated source rejected: %v", err)
	}
	c.Secret.Encryption.KEK.Passphrase.Enabled = true
	err := c.ValidateKEK()
	if err == nil {
		t.Fatal("passphrase.enabled=true was accepted")
	}
	// The operator needs to find the issue and the alternative.
	for _, want := range []string{"passphrase", "324"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
