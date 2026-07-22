package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Infisical fetches secrets from Infisical via its REST API using machine
// identity (universal auth). Configured entirely from dockrail's own
// environment — credentials never appear in deploy.yml. stdlib HTTP only:
// no SDK dependency, keeps D1's static-binary/cross-compile guarantee.
type Infisical struct {
	site, clientID, clientSecret, projectID, environment string
	paths                                                []string
	httpc                                                *http.Client
}

func NewInfisical() (Provider, error) {
	required := map[string]string{
		"INFISICAL_CLIENT_ID":     os.Getenv("INFISICAL_CLIENT_ID"),
		"INFISICAL_CLIENT_SECRET": os.Getenv("INFISICAL_CLIENT_SECRET"),
		"INFISICAL_PROJECT_ID":    os.Getenv("INFISICAL_PROJECT_ID"),
		"INFISICAL_ENVIRONMENT":   os.Getenv("INFISICAL_ENVIRONMENT"),
	}
	for name, v := range required {
		if v == "" {
			return nil, fmt.Errorf("infisical provider: %s is not set in dockrail's environment", name)
		}
	}
	site := os.Getenv("INFISICAL_SITE_URL")
	if site == "" {
		site = "https://app.infisical.com"
	}
	pathSpec := os.Getenv("INFISICAL_SECRET_PATH")
	if pathSpec == "" {
		pathSpec = "/"
	}
	var paths []string
	for _, p := range strings.Split(pathSpec, ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return &Infisical{
		site:         strings.TrimRight(site, "/"),
		clientID:     required["INFISICAL_CLIENT_ID"],
		clientSecret: required["INFISICAL_CLIENT_SECRET"],
		projectID:    required["INFISICAL_PROJECT_ID"],
		environment:  required["INFISICAL_ENVIRONMENT"],
		paths:        paths,
		httpc:        &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (i *Infisical) Fetch(ctx context.Context, names []string) (map[string]string, error) {
	token, err := i.login(ctx)
	if err != nil {
		return nil, fmt.Errorf("infisical login: %w", err)
	}
	all := map[string]string{} // later paths overwrite earlier ones (last wins)
	for _, p := range i.paths {
		kv, err := i.listPath(ctx, token, p)
		if err != nil {
			return nil, fmt.Errorf("infisical path %s: %w", p, err)
		}
		for k, v := range kv {
			all[k] = v
		}
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		v, ok := all[n]
		if !ok || v == "" {
			return nil, fmt.Errorf("required secret %q not found in infisical (env %s, paths %s)", n, i.environment, strings.Join(i.paths, ","))
		}
		out[n] = v
	}
	return out, nil
}

func (i *Infisical) login(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"clientId": i.clientID, "clientSecret": i.clientSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.site+"/api/v1/auth/universal-auth/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var resp struct {
		AccessToken string `json:"accessToken"`
	}
	if err := i.do(req, &resp); err != nil {
		return "", err
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("empty access token")
	}
	return resp.AccessToken, nil
}

func (i *Infisical) listPath(ctx context.Context, token, path string) (map[string]string, error) {
	q := url.Values{"workspaceId": {i.projectID}, "environment": {i.environment}, "secretPath": {path}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.site+"/api/v3/secrets/raw?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	var resp struct {
		Secrets []struct {
			SecretKey   string `json:"secretKey"`
			SecretValue string `json:"secretValue"`
		} `json:"secrets"`
	}
	if err := i.do(req, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(resp.Secrets))
	for _, s := range resp.Secrets {
		out[s.SecretKey] = s.SecretValue
	}
	return out, nil
}

func (i *Infisical) do(req *http.Request, into any) error {
	res, err := i.httpc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		// never echo the response body — it could restate credentials
		return fmt.Errorf("%s: HTTP %d", req.URL.Path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(into)
}
