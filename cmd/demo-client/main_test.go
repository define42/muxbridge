package main

import (
	"errors"
	"flag"
	"os"
	"os/exec"
	"testing"
)

func TestDefaultString(t *testing.T) {
	t.Parallel()

	if got := defaultString("", "fallback"); got != "fallback" {
		t.Fatalf("defaultString empty = %q, want %q", got, "fallback")
	}
	if got := defaultString("value", "fallback"); got != "value" {
		t.Fatalf("defaultString value = %q, want %q", got, "value")
	}
}

func TestMainRequiresPublicDomainWhenEdgeAddrMissing(t *testing.T) {
	if os.Getenv("GO_WANT_DEMO_HELPER_PROCESS") == "1" {
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		os.Args = []string{"demo-client"}
		_ = os.Unsetenv("MUXBRIDGE_PUBLIC_DOMAIN")
		_ = os.Unsetenv("MUXBRIDGE_EDGE_ADDR")
		_ = os.Unsetenv("MUXBRIDGE_CLIENT_TOKEN")
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMainRequiresPublicDomainWhenEdgeAddrMissing")
	cmd.Env = append(os.Environ(), "GO_WANT_DEMO_HELPER_PROCESS=1")

	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run error = %v, want ExitError", err)
	}
}

func TestMainRejectsInvalidPublicDomain(t *testing.T) {
	if os.Getenv("GO_WANT_DEMO_HELPER_PROCESS") == "2" {
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		os.Args = []string{"demo-client", "--public-domain", "localhost"}
		_ = os.Unsetenv("MUXBRIDGE_EDGE_ADDR")
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMainRejectsInvalidPublicDomain")
	cmd.Env = append(os.Environ(), "GO_WANT_DEMO_HELPER_PROCESS=2")

	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run error = %v, want ExitError", err)
	}
}
