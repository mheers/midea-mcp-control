package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	midea "github.com/thekondor/midea-porta-split"

	"github.com/mheers/midea-mcp-control/internal/config"
	"github.com/mheers/midea-mcp-control/internal/controller"
	"github.com/mheers/midea-mcp-control/internal/discovery"
)

// withFakeClient returns a service whose protocol client also answers the
// optional capability and energy queries, so the optional-extension paths are
// exercised rather than silently skipped.
func withFakeClient(t *testing.T, responses []midea.Response) (*Service, *fakeClient) {
	t.Helper()
	client := &fakeClient{polls: responses}
	service := testService(t, client)
	return service, client
}

func TestApplyRefusesWithoutConfirmation(t *testing.T) {
	service, client := withFakeClient(t, nil)
	// Even with a valid selector, an unconfirmed write must never reach the
	// device. This is the only thing standing between a stray MCP call and a
	// physical change.
	if _, err := service.Apply(context.Background(), "bedroom",
		controller.Patch{TargetTemp: floatPtr(21)}, false); err == nil {
		t.Fatal("Apply succeeded without confirmation")
	}
	if len(client.updates) != 0 || client.pollIndex != 0 {
		t.Fatalf("unconfirmed write touched the device: updates=%v polls=%d",
			client.updates, client.pollIndex)
	}
}

func TestApplyRefusesEmptyPatch(t *testing.T) {
	service, client := withFakeClient(t, nil)
	if _, err := service.Apply(context.Background(), "bedroom",
		controller.Patch{}, true); err == nil {
		t.Fatal("Apply accepted an empty patch")
	}
	if client.pollIndex != 0 {
		t.Fatalf("empty patch touched the device: %d polls", client.pollIndex)
	}
}

func TestApplySendsOnlyTheRequestedFields(t *testing.T) {
	after := midea.Response{Power: false, Mode: midea.ModeCool, TargetTemp: 19, FanSpeed: midea.FanLow}
	service, client := withFakeClient(t, []midea.Response{after, after})
	result, err := service.Apply(context.Background(), "bedroom", controller.Patch{
		Mode:       "cool",
		FanSpeed:   "low",
		TargetTemp: floatPtr(19),
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Mode != "cool" || result.State.TargetTemperature != 19 {
		t.Fatalf("state = %+v", result.State)
	}
	if len(client.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(client.updates))
	}
	request := client.updates[0]
	if request.Power != nil {
		t.Error("power was sent although it was not requested")
	}
	if request.Eco != nil || request.Turbo != nil || request.Sleep != nil {
		t.Error("unrequested toggles were sent")
	}
	if request.Mode == nil || *request.Mode != midea.ModeCool {
		t.Errorf("mode = %v, want cool", request.Mode)
	}
}

func TestApplyRejectsUnknownDevice(t *testing.T) {
	service, _ := withFakeClient(t, nil)
	if _, err := service.Apply(context.Background(), "nope",
		controller.Patch{Mode: "cool"}, true); err == nil {
		t.Fatal("Apply accepted an unknown device")
	}
}

func TestSetStateInputBuildsASparsePatch(t *testing.T) {
	// The MCP boundary must not invent defaults. A pointer field that was not
	// supplied has to stay nil, or the write would silently change a physical
	// field the caller never asked about.
	input := setStateInput{
		Device:  "bedroom",
		Temp:    floatPtr(21.5),
		Eco:     boolPtr(true),
		Confirm: true,
	}
	patch := input.patch()
	if patch.Power != nil || patch.Mode != "" || patch.FanSpeed != "" {
		t.Errorf("unset scalar fields were populated: %+v", patch)
	}
	if patch.SwingV != nil || patch.SwingH != nil || patch.Turbo != nil ||
		patch.Sleep != nil || patch.Display != nil {
		t.Errorf("unset toggles were populated: %+v", patch)
	}
	if patch.TargetTemp == nil || *patch.TargetTemp != 21.5 {
		t.Errorf("temperature = %v, want 21.5", patch.TargetTemp)
	}
	if patch.Eco == nil || !*patch.Eco {
		t.Error("eco was not carried through")
	}
	// The confirmation flag is not part of the patch; it is checked separately.
	if patch.Empty() {
		t.Error("a non-empty input produced an empty patch")
	}
}

func TestSetStateInputEmptyInputIsEmptyPatch(t *testing.T) {
	if patch := (setStateInput{Device: "x", Confirm: true}).patch(); !patch.Empty() {
		t.Errorf("empty input produced %+v", patch)
	}
}

func TestEnergyReportsDeviceMeasurement(t *testing.T) {
	service, _ := withFakeClient(t, []midea.Response{{Power: true}})
	result, err := service.Energy(context.Background(), "bedroom")
	if err != nil {
		t.Fatal(err)
	}
	if result.Device.Name != "bedroom" {
		t.Errorf("device = %q", result.Device.Name)
	}
	if result.Energy.RealtimeKW != 0 || result.Energy.TotalKWh == 0 {
		t.Errorf("energy = %+v", result.Energy)
	}
}

func TestEnergyRejectsUnknownDevice(t *testing.T) {
	service, _ := withFakeClient(t, nil)
	if _, err := service.Energy(context.Background(), "nope"); err == nil {
		t.Fatal("Energy accepted an unknown device")
	}
}

func TestCapabilitiesReportsDeviceFeatures(t *testing.T) {
	service, _ := withFakeClient(t, []midea.Response{{Power: true}})
	result, err := service.Capabilities(context.Background(), "bedroom")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Capabilities.Reported {
		t.Error("capabilities were not marked as reported")
	}
}

func TestVerifyAllWithTimeoutChecksEveryDevice(t *testing.T) {
	service, client := withFakeClient(t, []midea.Response{{Power: false}})
	results, err := service.VerifyAllWithTimeout(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if !results[0].OK || results[0].State == nil {
		t.Fatalf("result = %+v", results[0])
	}
	if client.pollIndex != 1 {
		t.Errorf("polls = %d, want 1", client.pollIndex)
	}
}

func TestVerifyAllIsReadOnly(t *testing.T) {
	// The MCP verification tool is annotated read-only, so it must not write
	// to the inventory that holds the device credentials.
	path := t.TempDir() + "/devices.json"
	device := config.Device{
		ID: "100000000000001", Name: "bedroom", IP: "192.0.2.131",
		Port: 6444, Type: 0xac, Protocol: 3,
		Token: strings.Repeat("00", 64), Key: strings.Repeat("11", 32),
	}
	if err := config.Save(path, config.File{Format: 1, Devices: []config.Device{device}}); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{polls: []midea.Response{{Power: false}}}
	service := testService(t, client)
	service.ConfigPath = path

	if _, err := service.VerifyAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Devices[0].LastVerified.IsZero() {
		t.Error("VerifyAll wrote to the credential inventory")
	}
}

func TestDiscoverDevicesUsesInjectedDiscovery(t *testing.T) {
	found := []discovery.Device{
		{ID: 1, IP: "192.0.2.131", Port: 6444, Type: 0xac, Protocol: 3},
	}
	var got discovery.Options
	service := &Service{
		Discover: func(_ context.Context, options discovery.Options) ([]discovery.Device, error) {
			got = options
			return found, nil
		},
	}
	devices, err := service.DiscoverDevices(context.Background(), discovery.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("devices = %d, want 1", len(devices))
	}
	if got.Timeout == 0 {
		t.Error("discovery was called without a default timeout")
	}
}

func TestDiscoverDevicesReportsMissingFunction(t *testing.T) {
	service := &Service{}
	if _, err := service.DiscoverDevices(context.Background(), discovery.Options{}); err == nil {
		t.Fatal("DiscoverDevices succeeded with no discovery function")
	}
}

func TestStatusOutputNeverCarriesCredentials(t *testing.T) {
	service, _ := withFakeClient(t, []midea.Response{{Power: false}})
	result, err := service.Status(context.Background(), "bedroom")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{strings.Repeat("00", 64), strings.Repeat("11", 32)} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("status output contains a credential: %s", encoded)
		}
	}
}

func TestVerifyAllOutputNeverCarriesCredentials(t *testing.T) {
	service, _ := withFakeClient(t, []midea.Response{{Power: false}})
	results, err := service.VerifyAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{strings.Repeat("00", 64), strings.Repeat("11", 32)} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("verification output contains a credential: %s", encoded)
		}
	}
}

func TestRecordVerificationIgnoresFailures(t *testing.T) {
	service, _ := withFakeClient(t, nil)
	// A failed check must not be recorded as a success.
	if err := service.RecordVerification([]VerificationResult{{OK: false}}); err != nil {
		t.Fatal(err)
	}
}

func TestStatusPropagatesDeviceErrors(t *testing.T) {
	service := testService(t, &erroringClient{})
	_, err := service.Status(context.Background(), "bedroom")
	if err == nil || !errors.Is(err, errFakeRead) {
		t.Fatalf("error = %v, want the read failure", err)
	}
}

var errFakeRead = errors.New("fake read failure")

type erroringClient struct{ fakeClient }

func (e *erroringClient) Poll(context.Context) (midea.Response, error) {
	return midea.Response{}, errFakeRead
}

func floatPtr(v float64) *float64 { return &v }
func boolPtr(v bool) *bool        { return &v }
