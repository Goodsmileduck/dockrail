package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func infisicalServer(t *testing.T, perPath map[string]map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/universal-auth/login":
			json.NewEncoder(w).Encode(map[string]string{"accessToken": "tok"})
		case "/api/v3/secrets/raw":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var list []map[string]string
			for k, v := range perPath[r.URL.Query().Get("secretPath")] {
				list = append(list, map[string]string{"secretKey": k, "secretValue": v})
			}
			json.NewEncoder(w).Encode(map[string]any{"secrets": list})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func setInfisicalEnv(t *testing.T, site, paths string) {
	t.Helper()
	t.Setenv("INFISICAL_CLIENT_ID", "cid")
	t.Setenv("INFISICAL_CLIENT_SECRET", "cs")
	t.Setenv("INFISICAL_PROJECT_ID", "proj")
	t.Setenv("INFISICAL_ENVIRONMENT", "prod")
	t.Setenv("INFISICAL_SITE_URL", site)
	t.Setenv("INFISICAL_SECRET_PATH", paths)
}

func TestInfisical_FetchAndLastPathWins(t *testing.T) {
	srv := infisicalServer(t, map[string]map[string]string{
		"/":     {"DB_URL": "root-value", "API_KEY": "k1"},
		"/apps": {"DB_URL": "apps-value"},
	})
	defer srv.Close()
	setInfisicalEnv(t, srv.URL, "/,/apps")
	p, err := NewInfisical()
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Fetch(context.Background(), []string{"DB_URL", "API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if got["DB_URL"] != "apps-value" || got["API_KEY"] != "k1" {
		t.Fatalf("wrong values: %v", got)
	}
}

func TestInfisical_MissingNameIsError(t *testing.T) {
	srv := infisicalServer(t, map[string]map[string]string{"/": {}})
	defer srv.Close()
	setInfisicalEnv(t, srv.URL, "/")
	p, _ := NewInfisical()
	if _, err := p.Fetch(context.Background(), []string{"NOPE"}); err == nil {
		t.Fatal("want error for missing secret name")
	}
}

func TestNewInfisical_MissingCredentials(t *testing.T) {
	t.Setenv("INFISICAL_CLIENT_ID", "")
	if _, err := NewInfisical(); err == nil {
		t.Fatal("want error when INFISICAL_CLIENT_ID unset")
	}
}
