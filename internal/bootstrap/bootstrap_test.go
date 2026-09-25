package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"midea-control/internal/cloud"
	"midea-control/internal/config"
	"midea-control/internal/controller"
	"midea-control/internal/discovery"
)

const (
	bedroomID = 100000000000001
	kitchenID = 100000000000002
	livingID  = 100000000000003

	testTokenA = "ee755a84a115703768bcc7c6c13d3d629aa416f1e2fd798beb9f78cbb1381d091cc245d7b063aad2a900e5b498fbd936c811f5d504b2e656d4f33b3bbc6d1da3"
	testKeyA   = "ed37bd31558a4b039aaf4e7a7a59aa7a75fd9101682045f69baf45d28380ae5c"
)

var (
	testTokenB = strings.Repeat("ab", 64)
	testKeyB   = strings.Repeat("cd", 32)
)

type fakeCloud struct {
	loginErr    error
	appliances  map[uint64]cloud.Appliance
	tokens      map[uint64]cloud.Credential
	tokenErr    error
	loggedIn    bool
	requestedID []uint64
}

func (f *fakeCloud) Login(context.Context) error {
	f.loggedIn = true
	return f.loginErr
}

func (f *fakeCloud) ListAppliances(context.Context) (map[uint64]cloud.Appliance, error) {
	return f.appliances, nil
}

func (f *fakeCloud) GetToken(_ context.Context, id uint64, _ cloud.UDPPIDMethod) (cloud.Credential, bool, error) {
	f.requestedID = append(f.requestedID, id)
	if f.tokenErr != nil {
		return cloud.Credential{}, false, f.tokenErr
	}
	credential, ok := f.tokens[id]
	return credential, ok, nil
}

type fakeControl struct {
	// accept maps a device id to the token that authenticates against it.
	// A device missing from the map refuses every credential.
	accept map[string]string
	polls  int
}

func (f *fakeControl) Poll(_ context.Context, device config.Device) (controller.State, error) {
	f.polls++
	token, ok := f.accept[device.ID]
	if !ok {
		return controller.State{}, errors.New("unreachable")
	}
	if token != device.Token {
		return controller.State{}, errors.New("auth rejected")
	}
	return controller.State{Power: false, Mode: "heat"}, nil
}

func discovered() []discovery.Device {
	return []discovery.Device{
		{ID: bedroomID, IP: "192.0.2.131", Port: 6444, Type: 0xac, Model: "00000Q18", Protocol: 3},
		{ID: kitchenID, IP: "192.0.2.130", Port: 6444, Type: 0xac, Model: "00000Q18", Protocol: 3},
		{ID: livingID, IP: "192.0.2.153", Port: 6444, Type: 0xac, Model: "00000Q1F", Protocol: 3},
	}
}

func newRunner(t *testing.T, client *fakeCloud, control *fakeControl) *Runner {
	t.Helper()
	runner := New()
	runner.ConfigPath = t.TempDir() + "/devices.json"
	runner.Account = "person@example.com"
	runner.Password = "secret"
	runner.Out = io.Discard
	runner.discover = func(context.Context, discovery.Options) ([]discovery.Device, error) {
		return discovered(), nil
	}
	runner.newCloud = func(string, string, string) (cloudClient, error) { return client, nil }
	runner.control = control
	return runner
}

func allAccept() *fakeControl {
	return &fakeControl{accept: map[string]string{
		"100000000000001": testTokenA,
		"100000000000002": testTokenA,
		"100000000000003": testTokenA,
	}}
}

func fullCloud() *fakeCloud {
	return &fakeCloud{
		appliances: map[uint64]cloud.Appliance{
			bedroomID: {ID: bedroomID, Name: "bedroom"},
			kitchenID: {ID: kitchenID, Name: "kids-room"},
			livingID:  {ID: livingID, Name: "living-room"},
		},
		tokens: map[uint64]cloud.Credential{
			bedroomID: {Token: testTokenA, Key: testKeyA},
			kitchenID: {Token: testTokenA, Key: testKeyA},
			livingID:  {Token: testTokenA, Key: testKeyA},
		},
	}
}

func TestRunWritesVerifiedCredentials(t *testing.T) {
	runner := newRunner(t, fullCloud(), allAccept())
	results, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	for _, result := range results {
		if !result.Verified || result.Err != nil {
			t.Fatalf("result %+v was not verified", result)
		}
		if result.Device.CredentialState != "stored" {
			t.Errorf("device %s credential state = %q", result.Device.Name, result.Device.CredentialState)
		}
	}

	stored, err := config.Load(runner.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Devices) != 3 {
		t.Fatalf("stored %d devices, want 3", len(stored.Devices))
	}
	byID := map[string]config.Device{}
	for _, device := range stored.Devices {
		byID[device.ID] = device
	}
	bedroom := byID["100000000000001"]
	if bedroom.Name != "bedroom" {
		t.Errorf("name = %q, want bedroom", bedroom.Name)
	}
	if bedroom.Token != testTokenA || bedroom.Key != testKeyA {
		t.Error("stored credential does not match the verified one")
	}
	if bedroom.CredentialMethod != int(cloud.UDPPIDBig) {
		t.Errorf("credential method = %d, want %d", bedroom.CredentialMethod, cloud.UDPPIDBig)
	}
	if bedroom.LastVerified.IsZero() {
		t.Error("last_verified was not recorded")
	}
}

func TestRunContinuesAfterOneDeviceFails(t *testing.T) {
	client := fullCloud()
	// Only two of the three tokens authenticate against the real devices.
	client.tokens[livingID] = cloud.Credential{Token: testTokenB, Key: testKeyB}
	control := &fakeControl{accept: map[string]string{
		"100000000000001": testTokenA,
		"100000000000002": testTokenA,
	}}
	runner := newRunner(t, client, control)

	results, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run returned an error even though two devices worked: %v", err)
	}
	var verified, failed int
	for _, result := range results {
		if result.Verified {
			verified++
		}
		if result.Err != nil {
			failed++
		}
	}
	if verified != 2 || failed != 1 {
		t.Fatalf("verified = %d, failed = %d, want 2 and 1", verified, failed)
	}
	// The good credentials must still be saved.
	stored, err := config.Load(runner.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Devices) != 2 {
		t.Fatalf("stored %d devices, want the 2 that verified", len(stored.Devices))
	}
}

func TestRunRejectsCredentialsTheDeviceRefuses(t *testing.T) {
	client := fullCloud()
	control := &fakeControl{accept: map[string]string{"100000000000001": testTokenB}}
	runner := newRunner(t, client, control)

	results, err := runner.Run(context.Background())
	if err == nil {
		t.Fatal("run succeeded even though no device accepted the cloud token")
	}
	for _, result := range results {
		if result.Verified {
			t.Errorf("device %s was marked verified without acceptance", result.Device.IP)
		}
	}
}

func TestRunRequiresCredentials(t *testing.T) {
	runner := newRunner(t, fullCloud(), allAccept())
	runner.Password = ""
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("run proceeded without a password")
	}
}

func TestRunFailsWhenLoginFails(t *testing.T) {
	client := fullCloud()
	client.loginErr = errors.New("bad credentials")
	runner := newRunner(t, client, allAccept())
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("run succeeded despite a login failure")
	}
}

func TestDryRunDoesNotWrite(t *testing.T) {
	runner := newRunner(t, fullCloud(), allAccept())
	runner.DryRun = true
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runner.ConfigPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry run created the inventory file")
	}
}

func TestSelectorLimitsWork(t *testing.T) {
	client := fullCloud()
	control := allAccept()
	runner := newRunner(t, client, control)
	runner.Selector = "bedroom"

	results, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var verified, skipped int
	for _, result := range results {
		if result.Verified {
			verified++
		}
		if result.Skipped {
			skipped++
		}
	}
	if verified != 1 || skipped != 2 {
		t.Fatalf("verified = %d, skipped = %d, want 1 and 2", verified, skipped)
	}
	if len(client.requestedID) != 1 || client.requestedID[0] != bedroomID {
		t.Errorf("token requests = %v, want only the bedroom", client.requestedID)
	}
}

func TestSelectorByIPAndID(t *testing.T) {
	for _, selector := range []string{"192.0.2.153", "100000000000003", "living-room"} {
		runner := newRunner(t, fullCloud(), allAccept())
		runner.Selector = selector
		results, err := runner.Run(context.Background())
		if err != nil {
			t.Fatalf("selector %q: %v", selector, err)
		}
		for _, result := range results {
			if result.Verified && result.Device.ID != "100000000000003" {
				t.Errorf("selector %q verified the wrong device %s", selector, result.Device.ID)
			}
		}
	}
}

func TestExistingInventoryIsPreservedAndMerged(t *testing.T) {
	existing := config.File{Format: 1, Devices: []config.Device{{
		ID: "999", Name: "Spare", IP: "192.0.2.200", Port: 6444,
		Type: 0xac, Model: "00000Q18", Protocol: 3,
		Token: testTokenB, Key: testKeyB,
	}}}
	path := t.TempDir() + "/devices.json"
	if err := config.Save(path, existing); err != nil {
		t.Fatal(err)
	}

	runner := newRunner(t, fullCloud(), allAccept())
	runner.ConfigPath = path
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Devices) != 4 {
		t.Fatalf("stored %d devices, want 4", len(stored.Devices))
	}
	for _, device := range stored.Devices {
		if device.ID == "999" && device.Token != testTokenB {
			t.Error("the untouched device lost its credential")
		}
	}
}

func TestUnusableExistingInventoryIsNotOverwritten(t *testing.T) {
	path := t.TempDir() + "/devices.json"
	if err := os.WriteFile(path, []byte(`{"format":1,"devices":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(t, fullCloud(), allAccept())
	runner.ConfigPath = path
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("run replaced an inventory with unsafe permissions")
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("the unsafe inventory was rewritten instead of rejected")
	}
}

func TestMergeKeepsNamesUnique(t *testing.T) {
	existing := config.File{Format: 1, Devices: []config.Device{
		{ID: "1", Name: "Living", IP: "192.0.2.1", Port: 6444, Type: 0xac, Protocol: 3, Token: testTokenA, Key: testKeyA},
	}}
	updates := map[string]config.Device{
		"2": {ID: "2", Name: "Living", IP: "192.0.2.2", Port: 6444, Type: 0xac, Protocol: 3, Token: testTokenA, Key: testKeyA},
		"3": {ID: "3", Name: "living", IP: "192.0.2.3", Port: 6444, Type: 0xac, Protocol: 3, Token: testTokenA, Key: testKeyA},
	}
	merged := merge(existing, updates)
	if err := merged.Validate(); err != nil {
		t.Fatalf("merged inventory is invalid: %v", err)
	}
	names := map[string]bool{}
	for _, device := range merged.Devices {
		key := strings.ToLower(device.Name)
		if names[key] {
			t.Errorf("duplicate name %q after merge", device.Name)
		}
		names[key] = true
	}
}

func TestRunReportsRotationOfExistingCredential(t *testing.T) {
	// A device that is already configured with a different credential must be
	// reported as rotated, because the cloud mints a new pair per request.
	path := t.TempDir() + "/devices.json"
	existing := config.File{Format: 1, Devices: []config.Device{{
		ID: "100000000000001", Name: "bedroom", IP: "192.0.2.131", Port: 6444,
		Type: 0xac, Model: "00000Q18", Protocol: 3,
		Token: testTokenB, Key: testKeyB,
	}}}
	if err := config.Save(path, existing); err != nil {
		t.Fatal(err)
	}

	runner := newRunner(t, fullCloud(), allAccept())
	runner.ConfigPath = path
	results, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.Device.ID == "100000000000001" && !result.Changed {
			t.Error("a re-issued credential was not reported as changed")
		}
	}
}

func TestRunReportsUnchangedCredential(t *testing.T) {
	path := t.TempDir() + "/devices.json"
	existing := config.File{Format: 1, Devices: []config.Device{{
		ID: "100000000000001", Name: "bedroom", IP: "192.0.2.131", Port: 6444,
		Type: 0xac, Model: "00000Q18", Protocol: 3,
		Token: testTokenA, Key: testKeyA,
	}}}
	if err := config.Save(path, existing); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(t, fullCloud(), allAccept())
	runner.ConfigPath = path
	results, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.Device.ID == "100000000000001" && result.Changed {
			t.Error("an identical credential was reported as changed")
		}
	}
}

func TestResultsNeverCarrySecrets(t *testing.T) {
	runner := newRunner(t, fullCloud(), allAccept())
	results, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testTokenA, testKeyA} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("results contain credential material: %s", encoded)
		}
	}
}

func TestDiscoveryFailureIsReported(t *testing.T) {
	runner := newRunner(t, fullCloud(), allAccept())
	runner.discover = func(context.Context, discovery.Options) ([]discovery.Device, error) {
		return nil, errors.New("no route to host")
	}
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("run ignored a discovery failure")
	}
}

func TestNonACAndV2DevicesAreIgnored(t *testing.T) {
	client := fullCloud()
	control := allAccept()
	runner := newRunner(t, client, control)
	runner.discover = func(context.Context, discovery.Options) ([]discovery.Device, error) {
		return []discovery.Device{
			{ID: 1, IP: "192.0.2.1", Port: 6444, Type: 0xcc, Protocol: 3},
			{ID: 2, IP: "192.0.2.2", Port: 6444, Type: 0xac, Protocol: 2},
		}, nil
	}
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("run attempted unsupported devices")
	}
	if len(client.requestedID) != 0 {
		t.Errorf("unsupported devices triggered token requests: %v", client.requestedID)
	}
}

func TestMissingInventoryIsCreated(t *testing.T) {
	runner := newRunner(t, fullCloud(), allAccept())
	runner.ConfigPath = t.TempDir() + "/nested/devices.json"
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(runner.ConfigPath); err != nil {
		t.Fatalf("inventory was not created: %v", err)
	}
}
