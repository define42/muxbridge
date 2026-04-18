package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/define42/muxbridge/internal/hostnames"
)

const (
	publicDomainEnv      = "MUXBRIDGE_PUBLIC_DOMAIN"
	clientCredentialsEnv = "MUXBRIDGE_CLIENT_CREDENTIALS"
	tlsCertFileEnv       = "MUXBRIDGE_TLS_CERT_FILE"
	tlsKeyFileEnv        = "MUXBRIDGE_TLS_KEY_FILE"
	debugEnv             = "MUXBRIDGE_DEBUG"
	dataDirEnv           = "MUXBRIDGH_DATA"
	dataDirCompatEnv     = "MUXBRIDGE_DATA"
)

type edgeConfig struct {
	PublicDomain      string
	EdgeDomain        string
	ClientCredentials map[string]string
	TLSCertFile       string
	TLSKeyFile        string
	Debug             bool
	DataDir           string
}

func (c edgeConfig) managedHosts() []string {
	hosts := []string{c.PublicDomain, c.EdgeDomain}
	for _, username := range c.sortedUsers() {
		hosts = append(hosts, hostnames.Subdomain(username, c.PublicDomain))
	}
	return hosts
}

func (c edgeConfig) sortedUsers() []string {
	users := make([]string, 0, len(c.ClientCredentials))
	for _, username := range c.ClientCredentials {
		users = append(users, username)
	}
	sort.Strings(users)
	return users
}

type repeatedFlag []string

func (f *repeatedFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func loadConfig(args []string, getenv func(string) string) (edgeConfig, error) {
	fs := flag.NewFlagSet("edge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var publicDomain string
	var tlsCertFile string
	var tlsKeyFile string
	var debug bool
	var credentialFlags repeatedFlag

	debug = parseBoolString(getenv(debugEnv))
	fs.StringVar(&publicDomain, "public-domain", "", "Public base domain")
	fs.StringVar(&tlsCertFile, "tls-cert-file", "", "Static TLS certificate PEM file")
	fs.StringVar(&tlsKeyFile, "tls-key-file", "", "Static TLS private key PEM file")
	fs.BoolVar(&debug, "debug", debug, "Enable debug logging")
	fs.Var(&credentialFlags, "client-credential", "Client credential in token=username form")

	if err := fs.Parse(args); err != nil {
		return edgeConfig{}, err
	}

	if publicDomain == "" {
		publicDomain = getenv(publicDomainEnv)
	}
	if tlsCertFile == "" {
		tlsCertFile = strings.TrimSpace(getenv(tlsCertFileEnv))
	}
	if tlsKeyFile == "" {
		tlsKeyFile = strings.TrimSpace(getenv(tlsKeyFileEnv))
	}
	dataDir := resolveDataDir(getenv)
	publicDomain = hostnames.NormalizeDomain(publicDomain)
	if err := hostnames.ValidateDomain(publicDomain); err != nil {
		return edgeConfig{}, fmt.Errorf("invalid public domain: %w", err)
	}
	if (tlsCertFile == "") != (tlsKeyFile == "") {
		return edgeConfig{}, errors.New("tls-cert-file and tls-key-file must be provided together")
	}

	credentials, err := parseCredentials(getenv(clientCredentialsEnv), credentialFlags)
	if err != nil {
		return edgeConfig{}, err
	}

	return edgeConfig{
		PublicDomain:      publicDomain,
		EdgeDomain:        hostnames.Subdomain("edge", publicDomain),
		ClientCredentials: credentials,
		TLSCertFile:       tlsCertFile,
		TLSKeyFile:        tlsKeyFile,
		Debug:             debug,
		DataDir:           dataDir,
	}, nil
}

func resolveDataDir(getenv func(string) string) string {
	for _, key := range []string{dataDirEnv, dataDirCompatEnv} {
		if dir := strings.TrimSpace(getenv(key)); dir != "" {
			return filepath.Clean(dir)
		}
	}

	homeDir, err := os.UserHomeDir()
	if err == nil && strings.TrimSpace(homeDir) != "" {
		return filepath.Join(homeDir, ".local", "share", "muxbridge")
	}
	return filepath.Clean("muxbridge-data")
}

func parseCredentials(envValue string, cliValues []string) (map[string]string, error) {
	entries := make([]string, 0, len(cliValues)+1)
	if trimmed := strings.TrimSpace(envValue); trimmed != "" {
		entries = append(entries, splitCredentialEnv(trimmed)...)
	}
	entries = append(entries, cliValues...)

	credentials := make(map[string]string, len(entries))
	userTokens := make(map[string]string, len(entries))
	for _, entry := range entries {
		token, username, err := parseCredential(entry)
		if err != nil {
			return nil, err
		}
		if previousUser, ok := credentials[token]; ok {
			return nil, fmt.Errorf("duplicate token %q for users %q and %q", token, previousUser, username)
		}
		if previousToken, ok := userTokens[username]; ok {
			return nil, fmt.Errorf("duplicate username %q for tokens %q and %q", username, previousToken, token)
		}

		credentials[token] = username
		userTokens[username] = token
	}

	return credentials, nil
}

func splitCredentialEnv(value string) []string {
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseCredential(entry string) (string, string, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", "", errors.New("malformed credential: empty entry")
	}

	token, username, ok := strings.Cut(entry, "=")
	if !ok {
		return "", "", fmt.Errorf("malformed credential %q: expected token=username", entry)
	}

	token = strings.TrimSpace(token)
	username = hostnames.NormalizeLabel(username)

	if token == "" {
		return "", "", fmt.Errorf("malformed credential %q: token must not be empty", entry)
	}
	if err := hostnames.ValidateLabel(username); err != nil {
		return "", "", fmt.Errorf("malformed credential %q: invalid username: %w", entry, err)
	}
	if username == "edge" {
		return "", "", errors.New(`username "edge" is reserved`)
	}

	return token, username, nil
}

func parseBoolString(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}
