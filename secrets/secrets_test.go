package secrets

import (
	"context"
	"strings"
	"testing"
)

func TestEnvFetchErrorsOnMissing(t *testing.T) {
	t.Setenv("APP_API_KEY", "abc")
	_, err := Env{}.Fetch(context.Background(), []string{"APP_API_KEY", "APP_DB_CONNECTION_URL"})
	if err == nil || !strings.Contains(err.Error(), "APP_DB_CONNECTION_URL") {
		t.Fatalf("want missing-var error naming the var, got %v", err)
	}
}

func TestEnvFetchErrorsOnEmpty(t *testing.T) {
	t.Setenv("APP_API_KEY", "")
	_, err := Env{}.Fetch(context.Background(), []string{"APP_API_KEY"})
	if err == nil || !strings.Contains(err.Error(), "APP_API_KEY") {
		t.Fatalf("want empty-var error naming the var, got %v", err)
	}
}

func TestEnvFetchReturnsValues(t *testing.T) {
	t.Setenv("APP_API_KEY", "abc")
	got, err := Env{}.Fetch(context.Background(), []string{"APP_API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if got["APP_API_KEY"] != "abc" {
		t.Fatalf("wrong value: %v", got)
	}
}

func TestNew_UnknownProvider(t *testing.T) {
	if _, err := New("vault"); err == nil {
		t.Fatal("want error for unknown provider")
	}
}

func TestNew_DefaultIsEnv(t *testing.T) {
	p, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(Env); !ok {
		t.Fatalf("want Env provider, got %T", p)
	}
}

func TestNew_ExplicitEnv(t *testing.T) {
	p, err := New("env")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(Env); !ok {
		t.Fatalf("want Env provider, got %T", p)
	}
}

func TestNew_InfisicalMissingCredentialsErrors(t *testing.T) {
	// Task 10 implements a real Infisical provider; without INFISICAL_*
	// credentials in the environment, New("infisical") must still fail —
	// but now because of missing config, not because it's unimplemented.
	t.Setenv("INFISICAL_CLIENT_ID", "")
	t.Setenv("INFISICAL_CLIENT_SECRET", "")
	t.Setenv("INFISICAL_PROJECT_ID", "")
	t.Setenv("INFISICAL_ENVIRONMENT", "")
	if _, err := New("infisical"); err == nil {
		t.Fatal("want error when INFISICAL_* credentials are unset")
	}
}
