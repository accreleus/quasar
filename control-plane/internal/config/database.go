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
func databaseURL() (string, error) {
	if raw := os.Getenv("DATABASE_URL"); raw != "" {
		return raw, nil
	}
	host := os.Getenv("QUASAR_DATABASE_HOST")
	if host == "" {
		return "", fmt.Errorf("DATABASE_URL or QUASAR_DATABASE_HOST is required")
	}
	password := os.Getenv("QUASAR_DATABASE_PASSWORD")
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
