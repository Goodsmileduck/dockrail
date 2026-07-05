package engine

import (
	"strings"
	"testing"
)

func TestCollectSecretsErrorsOnMissing(t *testing.T) {
	t.Setenv("APP_API_KEY", "abc")
	_, err := collectSecrets([]string{"APP_API_KEY", "APP_DB_CONNECTION_URL"})
	if err == nil || !strings.Contains(err.Error(), "APP_DB_CONNECTION_URL") {
		t.Fatalf("want missing-var error naming the var, got %v", err)
	}
}

func TestCollectSecretsReturnsValues(t *testing.T) {
	t.Setenv("APP_API_KEY", "abc")
	got, err := collectSecrets([]string{"APP_API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if got["APP_API_KEY"] != "abc" {
		t.Fatalf("wrong value: %v", got)
	}
}
