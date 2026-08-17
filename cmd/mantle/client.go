package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// apiClient talks to a Mantle instance's admin API.
//
// Every CLI command goes through here and none reaches into Postgres or the
// filesystem directly (§2.2). That is what keeps the API honest, lets the CLI
// work against a remote instance, and means a future `mantle-ui` inherits full
// capability rather than needing endpoints invented for it.
type apiClient struct {
	baseURL string
	// username and secret are HTTP Basic credentials. A machine credential is
	// presented as the password with any username, as Docker does.
	username string
	secret   string
	http     *http.Client
}

// credentials are what `mantle login` stores.
type credentials struct {
	Registry string `yaml:"registry"`
	Username string `yaml:"username"`
	Secret   string `yaml:"secret"`
}

// credentialsPath is where the CLI keeps its configuration.
func credentialsPath() (string, error) {
	if override := os.Getenv("MANTLE_CLI_CONFIG"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating your home directory: %w", err)
	}
	return filepath.Join(home, ".config", "mantle", "credentials.yaml"), nil
}

// loadCredentials reads stored credentials, returning a zero value when none
// have been saved.
func loadCredentials() (*credentials, error) {
	path, err := credentialsPath()
	if err != nil {
		return &credentials{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &credentials{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var creds credentials
	if err := yaml.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &creds, nil
}

// saveCredentials writes credentials with owner-only permissions.
func saveCredentials(creds *credentials) (string, error) {
	path, err := credentialsPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	data, err := yaml.Marshal(creds)
	if err != nil {
		return "", err
	}
	// 0600: this file holds a credential that can push images.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

// newClient builds an API client from flags, the environment, and stored
// credentials, in that precedence order.
func newClient(registryFlag, usernameFlag, secretFlag string) (*apiClient, error) {
	stored, err := loadCredentials()
	if err != nil {
		return nil, err
	}

	registry := firstNonEmpty(registryFlag, os.Getenv("MANTLE_REGISTRY"), stored.Registry)
	username := firstNonEmpty(usernameFlag, os.Getenv("MANTLE_USERNAME"), stored.Username)
	secret := firstNonEmpty(secretFlag, os.Getenv("MANTLE_TOKEN"), os.Getenv("MANTLE_PASSWORD"), stored.Secret)

	if registry == "" {
		return nil, fmt.Errorf(
			"no registry configured\n" +
				"  Run 'mantle login https://registry.example.com', or set MANTLE_REGISTRY,\n" +
				"  or pass --registry.")
	}
	if !strings.Contains(registry, "://") {
		registry = "https://" + registry
	}
	parsed, err := url.Parse(registry)
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid registry URL: %w", registry, err)
	}

	return &apiClient{
		baseURL:  strings.TrimSuffix(parsed.String(), "/"),
		username: username,
		secret:   secret,
		http:     &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

// apiError is a structured failure from the admin API.
type apiError struct {
	Status  int
	Code    string
	Message string
	Remedy  string
}

func (e *apiError) Error() string {
	message := e.Message
	if message == "" {
		message = fmt.Sprintf("the registry returned HTTP %d", e.Status)
	}
	if e.Remedy != "" {
		// Errors are UI (principle 4): the failure and the next action, on
		// separate lines so the remedy is not lost in the middle of a sentence.
		return message + "\n  " + e.Remedy
	}
	return message
}

// do performs a request against the admin API and decodes the response.
func (c *apiClient) do(method, path string, body, into any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding the request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.username != "" || c.secret != "" {
		username := c.username
		if username == "" {
			username = "mantle"
		}
		req.SetBasicAuth(username, c.secret)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w\n  Check the registry is running and the URL is correct.",
			c.baseURL, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("reading the response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return decodeAPIError(resp.StatusCode, payload)
	}
	if into != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, into); err != nil {
			return fmt.Errorf("the registry returned a response this version of mantle "+
				"could not parse: %w", err)
		}
	}
	return nil
}

// decodeAPIError turns an error response into an apiError, falling back
// gracefully when the body is not the expected shape — a proxy in front of the
// registry may return HTML, and "unexpected end of JSON input" would be a
// useless thing to show the operator.
func decodeAPIError(status int, payload []byte) error {
	var structured struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Remedy  string `json:"remedy"`
		} `json:"error"`
		// The OCI envelope, in case the caller reached a /v2 path.
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &structured); err == nil {
		if structured.Error.Message != "" {
			return &apiError{
				Status: status, Code: structured.Error.Code,
				Message: structured.Error.Message, Remedy: structured.Error.Remedy,
			}
		}
		if len(structured.Errors) > 0 {
			return &apiError{
				Status: status, Code: structured.Errors[0].Code,
				Message: structured.Errors[0].Message,
			}
		}
	}

	switch status {
	case http.StatusUnauthorized:
		return &apiError{Status: status, Code: "unauthorized",
			Message: "authentication failed",
			Remedy:  "Run 'mantle login' again, or check MANTLE_TOKEN."}
	case http.StatusForbidden:
		return &apiError{Status: status, Code: "forbidden",
			Message: "this credential is not permitted to perform that operation"}
	case http.StatusNotFound:
		return &apiError{Status: status, Code: "not_found", Message: "no such resource"}
	default:
		snippet := strings.TrimSpace(string(payload))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return &apiError{Status: status, Code: "http_error",
			Message: fmt.Sprintf("the registry returned HTTP %d: %s", status, snippet)}
	}
}

func (c *apiClient) get(path string, into any) error { return c.do(http.MethodGet, path, nil, into) }
func (c *apiClient) post(path string, body, into any) error {
	return c.do(http.MethodPost, path, body, into)
}
func (c *apiClient) patch(path string, body, into any) error {
	return c.do(http.MethodPatch, path, body, into)
}
func (c *apiClient) delete(path string, into any) error {
	return c.do(http.MethodDelete, path, nil, into)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
