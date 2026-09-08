package main

import (
	"errors"
	"testing"
)

// TestReleaseWebhookSecretConfigured — the view's `secret_configured` boolean.
// The environment fallback is a real source of the signing secret, so an
// instance signing from QUASAR_PLATFORM_RELEASE_WEBHOOK_SECRET alone must not
// report itself unsigned: secrets.Store.Status knows only about
// instance_secrets and answers Configured=false with NO error when there is no
// row, which is the case this covers.
func TestReleaseWebhookSecretConfigured(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stored    bool
		statusErr error
		env       string
		want      bool
	}{
		{name: "nothing anywhere"},
		{name: "stored only", stored: true, want: true},
		{name: "environment only", env: "s3cret", want: true},
		{name: "both", stored: true, env: "s3cret", want: true},
		{name: "status failed, environment set", statusErr: errors.New("boom"), env: "s3cret", want: true},
		{name: "status failed, nothing in the environment", statusErr: errors.New("boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := releaseWebhookSecretConfigured(tc.stored, tc.statusErr, tc.env); got != tc.want {
				t.Errorf("releaseWebhookSecretConfigured(%v, %v, %q) = %v, want %v",
					tc.stored, tc.statusErr, tc.env, got, tc.want)
			}
		})
	}
}
