package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// databaseURL keeps the existing full-DSN interface, while allowing installers
// to pass credentials without implementing URI userinfo escaping themselves.
// QUASAR_DATABASE_PASSWORD_FILE completes only the QUASAR_DATABASE_* form.
func databaseURL() (string, error) {
	if raw := os.Getenv("DATABASE_URL"); raw != "" {
		if os.Getenv("QUASAR_DATABASE_PASSWORD_FILE") != "" {
			return "", fmt.Errorf("QUASAR_DATABASE_PASSWORD_FILE completes QUASAR_DATABASE_HOST, not DATABASE_URL: set only one of them")
		}
		return raw, nil
	}
	host := os.Getenv("QUASAR_DATABASE_HOST")
	if host == "" {
		return "", fmt.Errorf("DATABASE_URL or QUASAR_DATABASE_HOST is required")
	}
	password, err := envOrFile("QUASAR_DATABASE_PASSWORD")
	if err != nil {
		return "", err
	}
	if password == "" {
		return "", fmt.Errorf("QUASAR_DATABASE_PASSWORD is required with QUASAR_DATABASE_HOST")
	}
	if strings.HasPrefix(strings.TrimSpace(password), "$(openssl rand ") {
		return "", fmt.Errorf("QUASAR_DATABASE_PASSWORD contains a shell template; run the credential command in a terminal and paste its generated value into .env")
	}
	port := envOr("QUASAR_DATABASE_PORT", "5432")
	if _, err := parsePort(":" + port); err != nil {
		return "", fmt.Errorf("QUASAR_DATABASE_PORT must be a valid port")
	}
	u := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(host, port),
		User:   url.UserPassword(envOr("QUASAR_DATABASE_USER", "quasar"), password),
		Path:   "/" + envOr("QUASAR_DATABASE_NAME", "quasar"),
	}
	q := url.Values{"sslmode": {envOr("QUASAR_DATABASE_SSLMODE", "disable")}}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// envOrFile reads key, or the file named by key+"_FILE" with trailing
// whitespace trimmed. Both set is an error; so is a named file that is
// unreadable or empty. Errors name the variable and never quote the contents.
func envOrFile(key string) (string, error) {
	fileKey := key + "_FILE"
	path := os.Getenv(fileKey)
	if path == "" {
		return os.Getenv(key), nil
	}
	if os.Getenv(key) != "" {
		return "", fmt.Errorf("%s and %s are both set: set only one", key, fileKey)
	}
	return readSecretFile(fileKey, path)
}

// readSecretFile is a secret file's value with trailing whitespace trimmed;
// unreadable or empty is an error naming envKey.
func readSecretFile(envKey, path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: cannot read %s: %w", envKey, path, err)
	}
	v := strings.TrimRight(string(raw), " \t\r\n")
	if v == "" {
		return "", fmt.Errorf("%s: %s is empty", envKey, path)
	}
	return v, nil
}
