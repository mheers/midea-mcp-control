package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef"

func testHTTPConfig() HTTPConfig {
	return HTTPConfig{Addr: "127.0.0.1:0", Token: testToken, Stateless: true}
}

func TestHTTPServerRequiresToken(t *testing.T) {
	config := testHTTPConfig()
	config.Token = ""
	if _, err := NewHTTPServer(nil, config); err == nil {
		t.Fatal("an HTTP server was created without a bearer token")
	}
}

func TestHTTPServerRefusesNonLoopbackByDefault(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8765", "192.0.2.10:8765", "example.com:8765"} {
		config := testHTTPConfig()
		config.Addr = addr
		if _, err := NewHTTPServer(nil, config); err == nil {
			t.Errorf("listen address %q was accepted without --http-allow-remote", addr)
		}
	}
}

func TestHTTPServerAcceptsLoopbackAndExplicitRemote(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8765", "localhost:8765", "[::1]:8765", ""} {
		config := testHTTPConfig()
		config.Addr = addr
		if _, err := NewHTTPServer(nil, config); err != nil {
			t.Errorf("loopback address %q was rejected: %v", addr, err)
		}
	}
	config := testHTTPConfig()
	config.Addr = "192.0.2.10:8765"
	config.AllowRemote = true
	if _, err := NewHTTPServer(nil, config); err != nil {
		t.Errorf("explicit remote bind was rejected: %v", err)
	}
}

func newProtectedHandler(t *testing.T, config HTTPConfig) http.Handler {
	t.Helper()
	server, err := NewHTTPServer(NewService(), config)
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler()
}

func TestHTTPRequiresBearerToken(t *testing.T) {
	handler := newProtectedHandler(t, testHTTPConfig())

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated request returned %d, want 401", response.Code)
	}

	for _, header := range []struct{ name, value string }{
		{"Authorization", "Bearer " + testToken},
		{"Authorization", "bearer " + testToken},
		{"X-MCP-Token", testToken},
	} {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", nil)
		request.Header.Set(header.name, header.value)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code == http.StatusUnauthorized {
			t.Errorf("%s %q was rejected", header.name, header.value)
		}
	}
}

func TestHTTPRejectsWrongToken(t *testing.T) {
	handler := newProtectedHandler(t, testHTTPConfig())
	for _, token := range []string{"wrong", testToken + "x", testToken[:len(testToken)-1], ""} {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("token %q returned %d, want 401", token, response.Code)
		}
	}
}

// DNS rebinding: a page on a hostile origin, or a request carrying a hostile
// Host header, must not reach the MCP endpoint.
func TestHTTPBlocksDNSRebinding(t *testing.T) {
	handler := newProtectedHandler(t, testHTTPConfig())

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", nil)
	request.Host = "evil.example.com"
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Errorf("hostile Host returned %d, want 403", response.Code)
	}

	for _, origin := range []string{"http://evil.example.com", "https://evil.example.com", "null"} {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", nil)
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("origin %q returned %d, want 403", origin, response.Code)
		}
	}

	for _, origin := range []string{"http://localhost:3000", "http://127.0.0.1:8765"} {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", nil)
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code == http.StatusForbidden {
			t.Errorf("loopback origin %q was rejected", origin)
		}
	}
}

func TestValidHostAndOrigin(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1:8765", "[::1]:8765", "127.0.0.1"} {
		if !validHost(host, nil) {
			t.Errorf("validHost(%q) = false", host)
		}
	}
	for _, host := range []string{"", "evil.com", "evil.com:80", "10.0.0.5:80"} {
		if validHost(host, nil) {
			t.Errorf("validHost(%q) = true", host)
		}
	}
}

// A container reached under its Docker service name must be able to present
// that name in the Host header — and only names on the allow-list may pass.
func TestHTTPAllowsConfiguredHostsOnly(t *testing.T) {
	config := testHTTPConfig()
	config.AllowedHosts = []string{"midea", "midea:8765"}
	handler := newProtectedHandler(t, config)

	// The allow-list must not bypass authentication.
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", nil)
	request.Host = "midea:8765"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated request with an allowed host returned %d, want 401", response.Code)
	}

	for _, host := range []string{"midea", "midea:8765"} {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", nil)
		request.Host = host
		request.Header.Set("Authorization", "Bearer "+testToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code == http.StatusForbidden || response.Code == http.StatusUnauthorized {
			t.Errorf("allowed host %q returned %d", host, response.Code)
		}
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", nil)
	request.Host = "evil.example.com"
	request.Header.Set("Authorization", "Bearer "+testToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Errorf("host %q outside the allow-list returned %d, want 403", "evil.example.com", response.Code)
	}
}

// normalizeHosts must make "name" and "name:port" equivalent and drop
// whitespace and empty entries.
func TestNormalizeHosts(t *testing.T) {
	got := normalizeHosts([]string{" midea ", "midea:8765", "", "  ", "other"})
	want := []string{"midea", "midea", "other"}
	if len(got) != len(want) {
		t.Fatalf("normalizeHosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeHosts = %v, want %v", got, want)
		}
	}
}

// /healthz answers without a token and without data: it exists for container
// healthchecks and probes, like the other showboat MCP sidecars.
func TestHTTPHealthzIsUnauthenticatedAndDataFree(t *testing.T) {
	handler := newProtectedHandler(t, testHTTPConfig())

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8765/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("/healthz returned %d, want 200", response.Code)
	}
	if body := response.Body.String(); body != "ok" {
		t.Errorf("/healthz body = %q, want \"ok\"", body)
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/healthz", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz returned %d, want 405", response.Code)
	}
}

func TestTokenFileIsCreatedWithRestrictivePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "mcp-token")
	token, file, err := LoadOrCreateToken("", path)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || file != path {
		t.Fatalf("token=%q file=%q", token, file)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %04o, want 0600", info.Mode().Perm())
	}
	// A second call must reuse the stored token rather than rotate it.
	again, _, err := LoadOrCreateToken("", path)
	if err != nil {
		t.Fatal(err)
	}
	if again != token {
		t.Error("the token was rotated on the second call")
	}
}

func TestTokenEnvTakesPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-token")
	token, file, err := LoadOrCreateToken("from-env", path)
	if err != nil {
		t.Fatal(err)
	}
	if token != "from-env" || file != "" {
		t.Fatalf("token=%q file=%q, want the env value and no file", token, file)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a token file was created even though the env supplied one")
	}
}

func TestGenerateTokenIsRandomAndLong(t *testing.T) {
	first, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("GenerateToken returned the same value twice")
	}
	if len(first) < 32 || strings.ContainsAny(first, "ghijklmnopqrstuvwxyz ") {
		t.Errorf("unexpected token shape: %q", first)
	}
}
