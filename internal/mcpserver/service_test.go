package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	midea "github.com/thekondor/midea-porta-split"

	"github.com/mheers/midea-mcp-control/internal/config"
	"github.com/mheers/midea-mcp-control/internal/controller"
)

type fakeClient struct {
	polls     []midea.Response
	pollIndex int
	updates   []midea.Request
}

func (f *fakeClient) Connect(context.Context) error { return nil }
func (f *fakeClient) Close() error                  { return nil }
func (f *fakeClient) Poll(context.Context) (midea.Response, error) {
	if f.pollIndex >= len(f.polls) {
		return midea.Response{}, nil
	}
	result := f.polls[f.pollIndex]
	f.pollIndex++
	return result, nil
}
func (f *fakeClient) Update(_ context.Context, request midea.Request) (midea.Response, error) {
	f.updates = append(f.updates, request)
	return f.polls[len(f.polls)-1], nil
}

// Capabilities and Energy make the fake satisfy the controller's optional
// extension interfaces, so those code paths are exercised rather than skipped.
func (f *fakeClient) Capabilities(context.Context) (midea.Capabilities, error) {
	return midea.Capabilities{}, nil
}

func (f *fakeClient) Energy(context.Context) (midea.Energy, error) {
	return midea.Energy{TotalKWh: 6.9}, nil
}

func testService(t *testing.T, client controller.Client) *Service {
	t.Helper()
	device := config.Device{
		ID:       "100000000000001",
		Name:     "bedroom",
		IP:       "192.0.2.131",
		Port:     6444,
		Type:     0xac,
		Model:    "00000Q18",
		Protocol: 3,
		Token:    strings.Repeat("00", 64),
		Key:      strings.Repeat("11", 32),
	}
	file := config.File{Format: 1, Devices: []config.Device{device}}
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := config.Save(path, file); err != nil {
		t.Fatal(err)
	}
	return &Service{
		ConfigPath: path,
		Control: &controller.Controller{
			NewClient: func(config.Device, controller.Operation) (controller.Client, error) { return client, nil },
		},
	}
}

func TestListDevicesDoesNotExposeCredentials(t *testing.T) {
	service := testService(t, &fakeClient{})
	devices, err := service.ListDevices()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(devices)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), strings.Repeat("00", 64)) || strings.Contains(string(data), strings.Repeat("11", 32)) {
		t.Fatalf("device list contains credentials: %s", data)
	}
}

func TestSetPowerRequiresExplicitConfirmation(t *testing.T) {
	client := &fakeClient{}
	service := testService(t, client)
	if _, err := service.SetPower(context.Background(), "bedroom", true, false); err == nil {
		t.Fatal("SetPower without confirmation succeeded")
	}
	if len(client.updates) != 0 {
		t.Fatalf("unconfirmed write reached client: %+v", client.updates)
	}
}

func TestSetPowerDelegatesConfirmedWrite(t *testing.T) {
	client := &fakeClient{polls: []midea.Response{
		{Power: false, Mode: midea.ModeHeat, TargetTemp: 22, FanSpeed: midea.FanAuto},
		{Power: true, Mode: midea.ModeHeat, TargetTemp: 22, FanSpeed: midea.FanAuto},
	}}
	service := testService(t, client)
	result, err := service.SetPower(context.Background(), "bedroom", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.State.Power || result.Device.Name != "bedroom" {
		t.Fatalf("result = %+v", result)
	}
	if len(client.updates) != 1 || client.updates[0].Power == nil || !*client.updates[0].Power {
		t.Fatalf("updates = %+v", client.updates)
	}
}

func TestVerifyAllUsesStoredCredentials(t *testing.T) {
	client := &fakeClient{polls: []midea.Response{{Power: false, Mode: midea.ModeHeat, TargetTemp: 22}}}
	service := testService(t, client)
	results, err := service.VerifyAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].OK || results[0].State == nil || results[0].State.Mode != "heat" || client.pollIndex != 1 {
		t.Fatalf("results = %+v, polls = %d", results, client.pollIndex)
	}
}

func TestVerifyAllReportsFailuresPerDevice(t *testing.T) {
	first := config.Device{
		ID: "100000000000001", Name: "first", IP: "192.0.2.1", Port: 6444,
		Type: 0xac, Protocol: 3, Token: strings.Repeat("00", 64), Key: strings.Repeat("11", 32),
	}
	second := first
	second.ID = "152832118193787"
	second.Name = "second"
	second.IP = "192.0.2.2"
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := config.Save(path, config.File{Format: 1, Devices: []config.Device{first, second}}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		ConfigPath: path,
		Control: &controller.Controller{NewClient: func(device config.Device, _ controller.Operation) (controller.Client, error) {
			if device.Name == "second" {
				return nil, errors.New("simulated expired credential")
			}
			return &fakeClient{polls: []midea.Response{{Power: false}}}, nil
		}},
	}
	results, err := service.VerifyAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || !results[0].OK || results[1].OK || results[1].Error == "" {
		t.Fatalf("results = %+v", results)
	}
	beforeMetadata, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !beforeMetadata.Devices[0].LastVerified.IsZero() || !beforeMetadata.Devices[1].LastVerified.IsZero() {
		t.Fatal("read-only VerifyAll changed verification metadata")
	}
	if err := service.RecordVerification(results); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Devices[0].LastVerified.IsZero() || !loaded.Devices[1].LastVerified.IsZero() {
		t.Fatalf("verification metadata = %+v", loaded.Devices)
	}
}

func TestSaveKeepsConfigPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	device := config.Device{
		ID: "1", Name: "test", IP: "192.0.2.1", Port: 6444, Type: 0xac, Protocol: 3,
		Token: strings.Repeat("00", 64), Key: strings.Repeat("11", 32),
	}
	if err := config.Save(path, config.File{Format: 1, Devices: []config.Device{device}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o, want 0600", info.Mode().Perm())
	}
}
