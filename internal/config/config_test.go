package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDevice() Device {
	return Device{
		ID:               "100000000000001",
		Name:             "bedroom",
		IP:               "192.0.2.131",
		Port:             6444,
		Type:             0xac,
		Model:            "00000Q18",
		Protocol:         3,
		Token:            strings.Repeat("00", 64),
		Key:              strings.Repeat("11", 32),
		CredentialMethod: 1,
		Expiry:           nil,
	}
}

func writeTestConfig(t *testing.T, mode os.FileMode, file File) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devices.json")
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAndFind(t *testing.T) {
	path := writeTestConfig(t, 0o600, File{Format: 1, Devices: []Device{testDevice()}})
	file, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	device, err := file.Find("bedroom")
	if err != nil {
		t.Fatal(err)
	}
	if device.ID != "100000000000001" {
		t.Fatalf("Find returned id %q", device.ID)
	}
}

func TestLoadRejectsBroadPermissions(t *testing.T) {
	path := writeTestConfig(t, 0o644, File{Format: 1, Devices: []Device{testDevice()}})
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("Load error = %v, want permissions error", err)
	}
}

func TestValidateRejectsMalformedV3Credential(t *testing.T) {
	device := testDevice()
	device.Token = "not-a-token"
	if err := device.Validate(); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("Validate error = %v, want token error", err)
	}
}

func TestPublicDoesNotExposeCredentials(t *testing.T) {
	public := testDevice().Public()
	data, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, testDevice().Token) || strings.Contains(text, testDevice().Key) {
		t.Fatalf("public device contains credentials: %s", text)
	}
}

func TestSaveUsesRestrictivePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "devices.json")
	file := File{Format: 1, Devices: []Device{testDevice()}}
	if err := Save(path, file); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("saved mode = %04o, want 0600", got)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsDuplicateNames(t *testing.T) {
	first := testDevice()
	second := testDevice()
	second.ID = "152832118193787"
	second.IP = "192.0.2.132"
	file := File{Format: 1, Devices: []Device{first, second}}
	if err := file.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate device name") {
		t.Fatalf("Validate error = %v, want duplicate-name error", err)
	}
}

func TestFindRejectsAmbiguousSelector(t *testing.T) {
	first := testDevice()
	second := testDevice()
	second.ID = "152832118193787"
	second.IP = "192.0.2.132"
	file := File{Format: 1, Devices: []Device{first, second}}
	if _, err := file.Find("bedroom"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("Find error = %v, want ambiguous error", err)
	}
}

func TestSavePreservesBootstrapVerificationState(t *testing.T) {
	device := testDevice()
	device.StateAtVerification = map[string]any{"power": false, "mode": 4}
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := Save(path, File{Format: 1, Devices: []Device{device}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Devices[0].StateAtVerification["mode"]; got != float64(4) {
		t.Fatalf("saved state = %#v, want mode 4", loaded.Devices[0].StateAtVerification)
	}
}

func TestExpiryCanBeRecordedWithoutExposingSecrets(t *testing.T) {
	device := testDevice()
	expires := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	device.Expiry = &expires
	if got := device.Public().Expiry; got == nil || !got.Equal(expires) {
		t.Fatalf("public expiry = %v, want %v", got, expires)
	}
}
