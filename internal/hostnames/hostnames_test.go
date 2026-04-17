package hostnames

import (
	"strings"
	"testing"
)

func TestNormalizeHostAndDomain(t *testing.T) {
	t.Parallel()

	if got := NormalizeHost(" Example.COM.:443 "); got != "example.com" {
		t.Fatalf("NormalizeHost = %q, want %q", got, "example.com")
	}
	if got := NormalizeDomain(" Demo.Example.COM. "); got != "demo.example.com" {
		t.Fatalf("NormalizeDomain = %q, want %q", got, "demo.example.com")
	}
	if got := NormalizeLabel(" Demo-User "); got != "demo-user" {
		t.Fatalf("NormalizeLabel = %q, want %q", got, "demo-user")
	}
	if got := Subdomain("Demo", "Example.COM."); got != "demo.example.com" {
		t.Fatalf("Subdomain = %q, want %q", got, "demo.example.com")
	}
}

func TestValidateDomain(t *testing.T) {
	t.Parallel()

	if err := ValidateDomain("example.com"); err != nil {
		t.Fatalf("ValidateDomain returned error for valid domain: %v", err)
	}

	invalid := []string{"", "localhost", "bad_domain.example.com", "-bad.example.com"}
	for _, domain := range invalid {
		if err := ValidateDomain(domain); err == nil {
			t.Fatalf("ValidateDomain(%q) succeeded, want error", domain)
		}
	}
}

func TestValidateLabel(t *testing.T) {
	t.Parallel()

	valid := []string{"demo", "demo-1", "a1"}
	for _, label := range valid {
		if err := ValidateLabel(label); err != nil {
			t.Fatalf("ValidateLabel(%q) returned error: %v", label, err)
		}
	}

	invalid := []string{"", "-demo", "demo-", "demo.user", "demo_user", strings.Repeat("a", 64)}
	for _, label := range invalid {
		if err := ValidateLabel(label); err == nil {
			t.Fatalf("ValidateLabel(%q) succeeded, want error", label)
		}
	}
}
