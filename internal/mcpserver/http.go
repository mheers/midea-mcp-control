package mcpserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPConfig configures the Streamable HTTP transport.
type HTTPConfig struct {
	// Addr is the listen address. It must resolve to a loopback address unless
	// AllowRemote is set.
	Addr string
	// Token is the bearer token clients must present. An empty token is
	// rejected: this server can change a physical device, so an unauthenticated
	// listener is never started.
	Token string
	// Stateless runs without server-side sessions, which suits a single local
	// client and keeps no state between requests.
	Stateless bool
	// AllowRemote permits a non-loopback bind. The authentication and
	// DNS-rebinding protections stay on regardless.
	AllowRemote bool
	// AllowedHosts lists additional Host header names (ports are ignored)
	// accepted beyond the always-allowed loopback names. A container behind a
	// Docker service name needs its name here; the default stays
	// loopback-only. Bearer authentication applies to every accepted name.
	AllowedHosts []string
}

// HTTPServer serves MCP over Streamable HTTP.
type HTTPServer struct {
	config  HTTPConfig
	service *Service
	server  *sdkmcp.Server
}

// NewHTTPServer builds an HTTP MCP server for a service.
func NewHTTPServer(service *Service, config HTTPConfig) (*HTTPServer, error) {
	if config.Token == "" {
		return nil, errors.New("http transport requires a bearer token")
	}
	if config.Addr == "" {
		config.Addr = "127.0.0.1:8765"
	}
	if err := CheckListenAddress(config.Addr, config.AllowRemote); err != nil {
		return nil, err
	}
	config.AllowedHosts = normalizeHosts(config.AllowedHosts)
	return &HTTPServer{
		config:  config,
		service: service,
		server:  NewMCPServer(service),
	}, nil
}

// normalizeHosts trims allow-list entries and drops any :port suffix, so both
// "midea" and "midea:8765" name the same host.
func normalizeHosts(hosts []string) []string {
	normalized := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		normalized = append(normalized, host)
	}
	return normalized
}

// CheckListenAddress refuses a non-loopback bind address unless it was
// explicitly allowed. It guards both the MCP listener and the Prometheus
// metrics listener.
func CheckListenAddress(addr string, allowRemote bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	return checkLoopback(host, allowRemote)
}

// checkLoopback refuses a non-loopback bind unless it was asked for.
func checkLoopback(host string, allowRemote bool) error {
	if allowRemote {
		return nil
	}
	if host == "" {
		return fmt.Errorf(
			"refusing to listen on all interfaces without --http-allow-remote; bind 127.0.0.1 explicitly")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A name like "localhost" is fine; anything else is not obviously safe.
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		return fmt.Errorf(
			"refusing to listen on %q: pass --http-allow-remote to bind a non-loopback address", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf(
			"refusing to listen on non-loopback address %q: pass --http-allow-remote if this is intended", host)
	}
	return nil
}

// Handler returns the HTTP handler with authentication and DNS-rebinding
// protection in front of the MCP endpoint. /healthz is answered without
// authentication and carries no data — it exists for container healthchecks
// and uptime probes, matching the other showboat MCP sidecars.
func (h *HTTPServer) Handler() http.Handler {
	inner := sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return h.server },
		&sdkmcp.StreamableHTTPOptions{
			Stateless: h.config.Stateless,
			// protect() below runs its own Host/Origin validation on every
			// request (loopback by default plus the explicit allow-list). The
			// SDK's built-in loopback check would additionally reject
			// allow-listed host names whenever the listener binds to loopback,
			// making behavior depend on the bind address, so it is disabled in
			// favor of the single, always-on check.
			DisableLocalhostProtection: true,
		},
	)
	protected := h.protect(inner)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			if request.Method != http.MethodGet && request.Method != http.MethodHead {
				writer.Header().Set("Allow", "GET, HEAD")
				http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte("ok"))
			return
		}
		protected.ServeHTTP(writer, request)
	})
}

// protect applies bearer authentication, Host validation and Origin
// validation. Together these stop DNS rebinding, where an attacker's page
// resolves to a local address and drives the device from a browser.
func (h *HTTPServer) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !h.authorized(request) {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="midea-mcp-control"`)
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !validHost(request.Host, h.config.AllowedHosts) {
			http.Error(writer, "forbidden: bad host", http.StatusForbidden)
			return
		}
		if origin := request.Header.Get("Origin"); origin != "" && !validOrigin(origin) {
			http.Error(writer, "forbidden: bad origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// authorized checks the bearer token in constant time and accepts either the
// Authorization header or the X-MCP-Token header.
func (h *HTTPServer) authorized(request *http.Request) bool {
	presented := ""
	if header := request.Header.Get("Authorization"); header != "" {
		// The auth scheme is case-insensitive per RFC 7235.
		if scheme, value, found := strings.Cut(header, " "); found &&
			strings.EqualFold(scheme, "Bearer") {
			presented = strings.TrimSpace(value)
		}
	}
	if presented == "" {
		presented = request.Header.Get("X-MCP-Token")
	}
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(h.config.Token)) == 1
}

// validHost reports whether a Host header is acceptable: a loopback name or
// one of the explicitly allowed host names. The allow-list is what lets a
// container be reached as http://midea:8765/mcp without weakening the
// loopback-only default.
func validHost(host string, allowed []string) bool {
	if host == "" {
		return false
	}
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, host) {
			return true
		}
	}
	return false
}

// validOrigin allows only loopback origins. An empty Origin (a non-browser
// client) is allowed; the bearer token is what protects those. The host
// allow-list deliberately does not extend here: browsers may only drive the
// endpoint from loopback pages.
func validOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return validHost(parsed.Host, nil)
}

// GenerateToken returns a random bearer token.
func GenerateToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// LoadOrCreateToken resolves the bearer token from an environment variable, a
// file, or by generating one and writing it with restrictive permissions.
//
// The generated token is never returned in a log line; only its path is.
func LoadOrCreateToken(envValue, tokenFile string) (token string, path string, err error) {
	if envValue != "" {
		return envValue, "", nil
	}
	if tokenFile == "" {
		dir, dirErr := os.UserConfigDir()
		if dirErr != nil {
			return "", "", fmt.Errorf("locate config dir: %w", dirErr)
		}
		tokenFile = filepath.Join(dir, "midea-mcp-control", "mcp-token")
	}
	if existing, readErr := os.ReadFile(tokenFile); readErr == nil {
		value := strings.TrimSpace(string(existing))
		if value == "" {
			return "", "", fmt.Errorf("token file %s is empty", tokenFile)
		}
		return value, tokenFile, nil
	}
	token, err = GenerateToken()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		return "", "", fmt.Errorf("create token directory: %w", err)
	}
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		return "", "", fmt.Errorf("write token file: %w", err)
	}
	if err := os.Chmod(tokenFile, 0o600); err != nil {
		return "", "", fmt.Errorf("secure token file: %w", err)
	}
	return token, tokenFile, nil
}
