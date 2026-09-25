package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testAccount  = "person@example.com"
	testPassword = "correct horse battery staple"
	testLoginID  = "0123456789abcdef"
)

type recordedRequest struct {
	path        string
	body        string
	random      string
	sign        string
	recipeApp   string
	authorize   string
	contentType string
}

// newFakeCloud serves the endpoints used by Login, ListAppliances and
// GetToken, and verifies that every request signature covers the exact body.
func newFakeCloud(t *testing.T, appliances map[uint64]Appliance) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	recorded := &[]recordedRequest{}
	sec := newSecurity(
		"ac21b9f9cbfe4ca5a88562ef25e2b768",
		"meicloud",
		"PROD_VnoClJI9aikS8dyy",
	)
	// The login response is encrypted with keys derived from the app key, the
	// same way the real client derives its session keys.
	digest := sha256.Sum256([]byte(sec.loginKey))
	hexDigest := hex.EncodeToString(digest[:])
	tempKey := []byte(hexDigest[:16])
	tempIV := []byte(hexDigest[16:32])

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		// The real endpoint is the "alias" query parameter of the proxy URL,
		// not the request path.
		endpoint := request.URL.Query().Get("alias")
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		*recorded = append(*recorded, recordedRequest{
			path:        endpoint,
			body:        string(body),
			random:      request.Header.Get("random"),
			sign:        request.Header.Get("sign"),
			recipeApp:   request.Header.Get("x-recipe-app"),
			authorize:   request.Header.Get("authorization"),
			contentType: request.Header.Get("content-type"),
		})

		// Every request must be signed over the exact bytes sent.
		if want := sec.sign(string(body), request.Header.Get("random")); want != request.Header.Get("sign") {
			t.Errorf("bad signature for %s: got %q, want %q", endpoint, request.Header.Get("sign"), want)
		}

		writer.Header().Set("content-type", "application/json")
		switch endpoint {
		case "/v1/multicloud/platform/user/route":
			writeJSON(t, writer, map[string]any{
				"code": 0,
				"data": map[string]any{"masUrl": "http://" + request.Host + "/mas/v5/app/proxy?alias="},
			})
		case "/v1/user/login/id/get":
			writeJSON(t, writer, map[string]any{
				"code": 0,
				"data": map[string]any{"loginId": testLoginID},
			})
		case "/mj/user/login":
			writeJSON(t, writer, map[string]any{
				"code": 0,
				"data": map[string]any{
					"uid":         "uid-123",
					"mdata":       map[string]any{"accessToken": "cloud-access-token"},
					"accessToken": encryptHex(t, testSessionKey, tempKey, tempIV),
					"randomData":  encryptHex(t, testSessionIV, tempKey, tempIV),
				},
			})
		case "/v1/appliance/user/list/get":
			// The fake server must encrypt serials with the session keys the
			// client derived at login, so it re-derives them the same way.
			clientSec := newSecurity(
				"ac21b9f9cbfe4ca5a88562ef25e2b768",
				"meicloud",
				"PROD_VnoClJI9aikS8dyy",
			)
			if err := clientSec.setSessionKeys(
				encryptHex(t, testSessionKey, tempKey, tempIV),
				encryptHex(t, testSessionIV, tempKey, tempIV),
			); err != nil {
				t.Fatal(err)
			}
			list := make([]any, 0, len(appliances))
			for id, appliance := range appliances {
				list = append(list, map[string]any{
					"id":             id,
					"name":           appliance.Name,
					"type":           "ac0",
					"sn":             encryptHex(t, appliance.SN, clientSec.aesKey, clientSec.aesIV),
					"onlineStatus":   "1",
					"enterpriseCode": "0000",
				})
			}
			writeJSON(t, writer, map[string]any{"code": 0, "data": map[string]any{"list": list}})
		case "/v1/iot/secure/getToken":
			var payload struct {
				UDPID          string `json:"udpid"`
				ApplianceCodes string `json:"applianceCodes"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("decode getToken body: %v", err)
			}
			if payload.ApplianceCodes == "" {
				t.Error("getToken request did not include applianceCodes")
			}
			writeJSON(t, writer, map[string]any{
				"code": 0,
				"data": map[string]any{
					"tokenlist": []any{map[string]any{
						"udpId": payload.UDPID,
						"token": strings.Repeat("AB", 64),
						"key":   strings.Repeat("CD", 32),
					}},
				},
			})
		default:
			t.Errorf("unexpected endpoint %s", endpoint)
			writer.WriteHeader(http.StatusNotFound)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, recorded
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := New(
		"SmartHome", testAccount, testPassword,
		WithAPIURL(server.URL+"/mas/v5/app/proxy?alias="),
		WithHTTPClient(server.Client()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestLoginListAndGetToken(t *testing.T) {
	const applianceID = 100000000000001
	appliances := map[uint64]Appliance{
		applianceID: {ID: applianceID, Name: "bedroom", Type: 0xac0, SN: "000000P0000000Q18ABCDEFGH"},
	}
	server, recorded := newFakeCloud(t, appliances)
	client := newTestClient(t, server)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Login(ctx); err != nil {
		t.Fatal(err)
	}
	if client.uid != "uid-123" {
		t.Errorf("uid = %q, want uid-123", client.uid)
	}
	if client.accessToken != "cloud-access-token" {
		t.Errorf("access token = %q, want cloud-access-token", client.accessToken)
	}

	list, err := client.ListAppliances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	appliance, ok := list[applianceID]
	if !ok {
		t.Fatalf("appliance %d missing from %v", applianceID, list)
	}
	if appliance.Name != "bedroom" {
		t.Errorf("name = %q, want bedroom", appliance.Name)
	}
	if appliance.Type != 0xac0 {
		t.Errorf("type = %#x, want 0xac0", appliance.Type)
	}
	if appliance.SN != "000000P0000000Q18ABCDEFGH" {
		t.Errorf("serial = %q, want decrypted value", appliance.SN)
	}
	if appliance.Model != "00000Q18" {
		t.Errorf("model = %q, want 00000Q18", appliance.Model)
	}

	for _, method := range []UDPPIDMethod{UDPPIDBig, UDPPIDLittle} {
		credential, found, err := client.GetToken(ctx, applianceID, method)
		if err != nil {
			t.Fatalf("GetToken(method %d): %v", method, err)
		}
		if !found {
			t.Fatalf("GetToken(method %d) reported no token", method)
		}
		if len(credential.Token) != 128 || len(credential.Key) != 64 {
			t.Errorf("credential sizes = %d/%d, want 128/64 hex characters",
				len(credential.Token), len(credential.Key))
		}
		if credential.Token != strings.ToLower(credential.Token) {
			t.Error("token was not normalised to lower case")
		}
	}

	// The route call must re-point the client at the cluster URL.
	if len(*recorded) < 5 {
		t.Fatalf("recorded %d requests, want at least 5", len(*recorded))
	}
	for _, request := range *recorded {
		if request.recipeApp != "1010" {
			t.Errorf("x-recipe-app = %q, want 1010", request.recipeApp)
		}
		if !strings.HasPrefix(request.authorize, "Basic ") {
			t.Errorf("authorization = %q, want Basic ...", request.authorize)
		}
		if !strings.HasPrefix(request.contentType, "application/json") {
			t.Errorf("content-type = %q", request.contentType)
		}
	}
}

func TestLoginSendsHashedPasswords(t *testing.T) {
	server, recorded := newFakeCloud(t, nil)
	client := newTestClient(t, server)
	if err := client.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, request := range *recorded {
		if request.path != "/mj/user/login" {
			continue
		}
		found = true
		if strings.Contains(request.body, testPassword) {
			t.Fatal("login request contained the plaintext password")
		}
		var payload struct {
			IoTData struct {
				Password string `json:"password"`
				IAMPwd   string `json:"iampwd"`
			} `json:"iotData"`
			Data struct {
				Platform string `json:"platform"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(request.body), &payload); err != nil {
			t.Fatal(err)
		}
		sec := newSecurity(
			"ac21b9f9cbfe4ca5a88562ef25e2b768",
			"meicloud",
			"PROD_VnoClJI9aikS8dyy",
		)
		if want := sec.encryptPassword(testLoginID, testPassword); payload.IoTData.Password != want {
			t.Errorf("password hash = %q, want %q", payload.IoTData.Password, want)
		}
		if want := sec.encryptIAMPassword(testLoginID, testPassword); payload.IoTData.IAMPwd != want {
			t.Errorf("iampwd = %q, want %q", payload.IoTData.IAMPwd, want)
		}
		if payload.Data.Platform != "2" {
			t.Errorf("platform = %q, want \"2\"", payload.Data.Platform)
		}
	}
	if !found {
		t.Fatal("no login request recorded")
	}
}

func TestAPIErrorIsSurfaced(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"code":3004,"msg":"value is illegal"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := New("SmartHome", testAccount, testPassword,
		WithAPIURL(server.URL+"/"), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.loginID(context.Background())
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiError.Code != 3004 || apiError.Message != "value is illegal" {
		t.Errorf("apiError = %+v", apiError)
	}
}

func TestGetTokenWithoutTokenList(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"code":0,"data":{}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := New("SmartHome", testAccount, testPassword,
		WithAPIURL(server.URL+"/"), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	credential, found, err := client.GetToken(context.Background(), 12345, UDPPIDBig)
	if err != nil {
		t.Fatal(err)
	}
	if found || credential.Token != "" {
		t.Errorf("GetToken = %+v, found=%v; want empty", credential, found)
	}
}

func TestNewValidatesInput(t *testing.T) {
	if _, err := New("OtherCloud", testAccount, testPassword); err == nil {
		t.Error("New accepted an unsupported cloud")
	}
	if _, err := New("SmartHome", "", testPassword); err == nil {
		t.Error("New accepted an empty account")
	}
	if _, err := New("SmartHome", testAccount, ""); err == nil {
		t.Error("New accepted an empty password")
	}
	if _, err := New("MSmartHome", testAccount, testPassword); err != nil {
		t.Errorf("New rejected the MSmartHome alias: %v", err)
	}
}

func TestGetTokenRejectsOversizeApplianceID(t *testing.T) {
	client, err := New("SmartHome", testAccount, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.GetToken(context.Background(), 1<<48, UDPPIDBig); err == nil {
		t.Error("GetToken accepted an appliance id that cannot be encoded")
	}
}
