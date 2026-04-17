package hostnames

import (
	"fmt"
	"net"
	"strings"
)

// NormalizeHost lowercases hostnames, strips ports when present, and removes a trailing dot.
func NormalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	return strings.TrimSuffix(host, ".")
}

func NormalizeDomain(domain string) string {
	return NormalizeHost(domain)
}

func NormalizeLabel(label string) string {
	return strings.TrimSpace(strings.ToLower(label))
}

func Subdomain(label, domain string) string {
	return NormalizeHost(label + "." + domain)
}

func ValidateDomain(domain string) error {
	domain = NormalizeDomain(domain)
	if domain == "" {
		return fmt.Errorf("domain is required")
	}

	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return fmt.Errorf("domain %q must have at least two labels", domain)
	}
	for _, label := range labels {
		if err := ValidateLabel(label); err != nil {
			return fmt.Errorf("domain %q is invalid: %w", domain, err)
		}
	}
	return nil
}

func ValidateLabel(label string) error {
	if label == "" {
		return fmt.Errorf("label is empty")
	}
	if len(label) > 63 {
		return fmt.Errorf("label %q exceeds the 63 character DNS limit", label)
	}

	for i, r := range label {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(label)-1:
		default:
			return fmt.Errorf("label %q contains invalid character %q", label, r)
		}
	}

	return nil
}
