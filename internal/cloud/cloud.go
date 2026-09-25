package cloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// profile holds the app identity used to talk to one Midea cloud.
type profile struct {
	appID   string
	appKey  string
	iotKey  string
	hmacKey string
	apiURL  string
}

// profiles lists the cloud identities this package implements. Only the
// SmartHome (MSmartHome) identity is implemented; other vendor clouds use a
// different login handshake and are intentionally not guessed at.
var profiles = map[string]profile{
	"SmartHome": {
		appID:   "1010",
		appKey:  "ac21b9f9cbfe4ca5a88562ef25e2b768",
		iotKey:  "meicloud",
		hmacKey: "PROD_VnoClJI9aikS8dyy",
		apiURL:  "https://mp-prod.appsmb.com/mas/v5/app/proxy?alias=",
	},
}

// alias maps alternative spellings onto a profile name.
var alias = map[string]string{
	"MSmartHome": "SmartHome",
	"SmartHome":  "SmartHome",
}

// Appliance is one device as reported by the cloud inventory.
type Appliance struct {
	ID     uint64
	Name   string
	Type   int
	SN     string
	Model  string
	Online bool
}

// Credential is a V3 LAN token/key pair.
type Credential struct {
	Token string
	Key   string
}

// APIError is a non-zero response code from the cloud.
type APIError struct {
	Code     int
	Message  string
	Endpoint string
}

func (e *APIError) Error() string {
	message := e.Message
	if message == "" {
		message = "no message"
	}
	return fmt.Sprintf("cloud: %s returned code %d: %s", e.Endpoint, e.Code, message)
}

// Option customises a Client. Options exist mainly for tests.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client used for cloud requests.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

// WithAPIURL overrides the cloud endpoint. Intended for tests.
func WithAPIURL(url string) Option {
	return func(c *Client) {
		if url != "" {
			c.apiURL = url
		}
	}
}

// Client talks to the Midea SmartHome cloud.
type Client struct {
	http     *http.Client
	apiURL   string
	appID    string
	appKey   string
	account  string
	password string
	auth     string
	sec      *security
	deviceID string

	uid         string
	accessToken string
}

// New creates a cloud client for the named cloud.
func New(name, account, password string, options ...Option) (*Client, error) {
	resolved, ok := alias[name]
	if !ok {
		return nil, fmt.Errorf("cloud: unsupported cloud %q (supported: SmartHome)", name)
	}
	if account == "" || password == "" {
		return nil, errors.New("cloud: account and password are required")
	}
	config := profiles[resolved]
	client := &Client{
		http:     &http.Client{Timeout: 20 * time.Second},
		apiURL:   config.apiURL,
		appID:    config.appID,
		appKey:   config.appKey,
		account:  account,
		password: password,
		sec:      newSecurity(config.appKey, config.iotKey, config.hmacKey),
		deviceID: deviceID(account),
	}
	client.auth = "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(config.appKey+":"+config.iotKey),
	)
	for _, option := range options {
		option(client)
	}
	return client, nil
}

// Login authenticates against the cloud and establishes the AES session.
func (c *Client) Login(ctx context.Context) error {
	// The route endpoint tells us which regional cluster to use. It is
	// best-effort: the default endpoint usually works on its own.
	if err := c.route(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	loginID, err := c.loginID(ctx)
	if err != nil {
		return err
	}

	stamp := timestamp()
	iotData := c.generalData()
	delete(iotData, "uid")
	iotData["iampwd"] = c.sec.encryptIAMPassword(loginID, c.password)
	iotData["loginAccount"] = c.account
	iotData["password"] = c.sec.encryptPassword(loginID, c.password)
	iotData["stamp"] = stamp

	response, err := c.request(ctx, "/mj/user/login", map[string]any{
		"iotData": iotData,
		"data": map[string]any{
			"appKey":   c.appKey,
			"deviceId": c.deviceID,
			"platform": "2",
		},
		"stamp": stamp,
	})
	if err != nil {
		return fmt.Errorf("cloud: login: %w", err)
	}

	uid, _ := response["uid"].(string)
	if uid == "" {
		return errors.New("cloud: login response did not contain a uid")
	}
	metadata, _ := response["mdata"].(map[string]any)
	accessToken, _ := metadata["accessToken"].(string)
	if accessToken == "" {
		return errors.New("cloud: login response did not contain an access token")
	}
	encryptedKey, _ := response["accessToken"].(string)
	encryptedIV, _ := response["randomData"].(string)
	if err := c.sec.setSessionKeys(encryptedKey, encryptedIV); err != nil {
		return err
	}
	c.uid = uid
	c.accessToken = accessToken
	return nil
}

// ListAppliances returns the cloud inventory keyed by appliance ID.
func (c *Client) ListAppliances(ctx context.Context) (map[uint64]Appliance, error) {
	response, err := c.request(ctx, "/v1/appliance/user/list/get", c.generalData())
	if err != nil {
		return nil, fmt.Errorf("cloud: list appliances: %w", err)
	}
	entries, _ := response["list"].([]any)
	appliances := make(map[uint64]Appliance, len(entries))
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		id, ok := toUint64(item["id"])
		if !ok {
			continue
		}
		appliance := Appliance{ID: id}
		appliance.Name, _ = item["name"].(string)
		if typeText, ok := item["type"].(string); ok {
			if parsed, err := strconv.ParseInt(typeText, 16, 32); err == nil {
				appliance.Type = int(parsed)
			}
		}
		onlineStatus, _ := item["onlineStatus"].(string)
		appliance.Online = onlineStatus == "1"
		encryptedSN, _ := item["sn"].(string)
		if encryptedSN != "" {
			appliance.SN, err = c.sec.decrypt(encryptedSN)
			if err != nil {
				return nil, fmt.Errorf("cloud: decrypt serial for appliance %d: %w", id, err)
			}
		}
		// The cloud derives the model number from the serial number.
		if len(appliance.SN) > 17 {
			appliance.Model = appliance.SN[9:17]
		}
		appliances[id] = appliance
	}
	return appliances, nil
}

// GetToken fetches the V3 LAN credential for one appliance and encoding
// method. It reports false when the cloud has no token for that method.
func (c *Client) GetToken(ctx context.Context, applianceID uint64, method UDPPIDMethod) (Credential, bool, error) {
	id, err := udpID(applianceID, method)
	if err != nil {
		return Credential{}, false, err
	}
	data := c.generalData()
	data["udpid"] = id
	// The SmartHome cloud rejects the request with "value is illegal" unless
	// the appliance id is echoed as applianceCodes.
	data["applianceCodes"] = strconv.FormatUint(applianceID, 10)

	response, err := c.request(ctx, "/v1/iot/secure/getToken", data)
	if err != nil {
		return Credential{}, false, fmt.Errorf("cloud: get token for %d: %w", applianceID, err)
	}
	entries, _ := response["tokenlist"].([]any)
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		udp, _ := item["udpId"].(string)
		if udp != id {
			continue
		}
		token, _ := item["token"].(string)
		key, _ := item["key"].(string)
		if token == "" || key == "" {
			return Credential{}, false, nil
		}
		return Credential{
			Token: strings.ToLower(token),
			Key:   strings.ToLower(key),
		}, true, nil
	}
	return Credential{}, false, nil
}

func (c *Client) route(ctx context.Context) error {
	data := c.generalData()
	data["userType"] = "0"
	data["userName"] = c.account
	response, err := c.request(ctx, "/v1/multicloud/platform/user/route", data)
	if err != nil {
		return err
	}
	if url, ok := response["masUrl"].(string); ok && url != "" {
		c.apiURL = url
	}
	return nil
}

func (c *Client) loginID(ctx context.Context) (string, error) {
	data := c.generalData()
	data["loginAccount"] = c.account
	response, err := c.request(ctx, "/v1/user/login/id/get", data)
	if err != nil {
		return "", fmt.Errorf("cloud: get login id: %w", err)
	}
	loginID, _ := response["loginId"].(string)
	if loginID == "" {
		return "", errors.New("cloud: login id response was empty")
	}
	return loginID, nil
}

func (c *Client) generalData() map[string]any {
	var uid any
	if c.uid != "" {
		uid = c.uid
	}
	return map[string]any{
		"src":        c.appID,
		"format":     "2",
		"stamp":      timestamp(),
		"platformId": "1",
		"deviceId":   c.deviceID,
		"reqId":      randomHex(16),
		"uid":        uid,
		"clientType": "1",
		"appId":      c.appID,
		"language":   "en_US",
	}
}

// request performs one signed cloud API call and returns the "data" object.
func (c *Client) request(ctx context.Context, endpoint string, payload map[string]any) (map[string]any, error) {
	if _, ok := payload["reqId"]; !ok {
		payload["reqId"] = randomHex(16)
	}
	if _, ok := payload["stamp"]; !ok {
		payload["stamp"] = timestamp()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("cloud: encode request: %w", err)
	}
	random := strconv.FormatInt(time.Now().Unix(), 10)

	url := c.apiURL + endpoint
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url, bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("cloud: build request: %w", err)
	}
	httpRequest.Header.Set("content-type", "application/json; charset=utf-8")
	httpRequest.Header.Set("secretVersion", "1")
	httpRequest.Header.Set("sign", c.sec.sign(string(body), random))
	httpRequest.Header.Set("random", random)
	httpRequest.Header.Set("x-recipe-app", c.appID)
	httpRequest.Header.Set("authorization", c.auth)
	if c.uid != "" {
		httpRequest.Header.Set("uid", c.uid)
	}
	if c.accessToken != "" {
		httpRequest.Header.Set("accessToken", c.accessToken)
	}

	var response struct {
		Code any            `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	if err := c.do(httpRequest, &response); err != nil {
		return nil, fmt.Errorf("cloud: %s: %w", endpoint, err)
	}

	code, err := toInt(response.Code)
	if err != nil {
		return nil, fmt.Errorf("cloud: %s returned an unreadable code: %w", endpoint, err)
	}
	if code != 0 {
		return nil, &APIError{Code: code, Message: response.Msg, Endpoint: endpoint}
	}
	if response.Data == nil {
		return map[string]any{}, nil
	}
	return response.Data, nil
}

// do sends the request, retrying transient transport failures.
func (c *Client) do(request *http.Request, out any) error {
	const attempts = 3
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			// Restore the body for the retry; the first attempt consumed it.
			if err := resetBody(request); err != nil {
				return err
			}
		}
		response, err := c.http.Do(request.Clone(request.Context()))
		if err != nil {
			lastErr = err
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		closeErr := response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if closeErr != nil {
			lastErr = closeErr
			continue
		}
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected HTTP status %s", response.Status)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			lastErr = fmt.Errorf("decode response: %w", err)
			continue
		}
		return nil
	}
	return fmt.Errorf("request failed after %d attempts: %w", attempts, lastErr)
}

// resetBody rewinds a retryable request body.
func resetBody(request *http.Request) error {
	if request.GetBody == nil {
		return errors.New("cloud: request cannot be retried")
	}
	body, err := request.GetBody()
	if err != nil {
		return fmt.Errorf("cloud: rewind request: %w", err)
	}
	request.Body = body
	return nil
}

func randomHex(length int) string {
	buffer := make([]byte, length)
	if _, err := rand.Read(buffer); err != nil {
		// crypto/rand never fails on supported platforms; fall back to a
		// timestamp-derived value rather than aborting a credential fetch.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buffer)
}

func timestamp() string {
	return time.Now().UTC().Format("20060102150405")
}

func toUint64(value any) (uint64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case string:
		parsed, err := strconv.ParseUint(typed, 10, 64)
		return parsed, err == nil
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil || parsed < 0 {
			return 0, false
		}
		return uint64(parsed), true
	default:
		return 0, false
	}
}

func toInt(value any) (int, error) {
	switch typed := value.(type) {
	case float64:
		return int(typed), nil
	case string:
		return strconv.Atoi(typed)
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err
	case nil:
		return 0, errors.New("missing code")
	default:
		return 0, fmt.Errorf("unexpected type %T", value)
	}
}
