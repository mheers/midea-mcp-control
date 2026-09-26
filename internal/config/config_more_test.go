package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultPathHonoursTheEnvironment(t *testing.T) {
	t.Setenv("MIDEA_MCP_CONTROL_CONFIG", "/tmp/custom-devices.json")
	if got := DefaultPath(); got != "/tmp/custom-devices.json" {
		t.Fatalf("DefaultPath = %q, want the environment value", got)
	}
}

func TestDefaultPathFallsBackToUserConfigDir(t *testing.T) {
	t.Setenv("MIDEA_MCP_CONTROL_CONFIG", "")
	got := DefaultPath()
	if !strings.Contains(got, filepath.Join("midea-mcp-control", "devices.json")) {
		t.Fatalf("DefaultPath = %q, want the standard location", got)
	}
}

func TestPublicDevicesStripsCredentials(t *testing.T) {
	device := testDevice()
	device.Token = "s3cret-token"
	device.Key = "s3cret-key"
	device.MAC = "aabbccddeeff"
	file := File{Format: 1, Devices: []Device{device}}

	public := file.PublicDevices()
	if len(public) != 1 {
		t.Fatalf("PublicDevices = %d entries", len(public))
	}
	if public[0].MAC != "aabbccddeeff" || public[0].ID != device.ID {
		t.Errorf("public view lost non-secret fields: %+v", public[0])
	}
	// The guarantee is structural, not a matter of clearing values:
	// PublicDevice has no credential fields to leak.
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"s3cret-token", "s3cret-key"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("public view leaked a credential: %s", encoded)
		}
	}
	if public[0].CredentialState != "missing" {
		t.Errorf("credential state = %q, want missing for a short token", public[0].CredentialState)
	}

	device.Token = strings.Repeat("00", 64)
	device.Key = strings.Repeat("11", 32)
	public = (File{Format: 1, Devices: []Device{device}}).PublicDevices()
	if public[0].CredentialState != "stored" {
		t.Errorf("credential state = %q, want stored", public[0].CredentialState)
	}
}

func TestLoadRejectsAMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("Load accepted a missing file")
	}
}

func TestLoadRejectsADirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Fatal("Load accepted a directory")
	}
}

func TestLoadRejectsASymlink(t *testing.T) {
	// A symlink could point somewhere unexpected; the inventory must be a real
	// file with checked permissions.
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte(`{"format":1,"devices":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := Load(link); err == nil {
		t.Fatal("Load followed a symlink")
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted malformed JSON")
	}
}

func TestSaveRejectsAnInvalidInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	// No devices at all.
	if err := Save(path, File{Format: 1}); err == nil {
		t.Fatal("Save accepted an empty inventory")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a rejected inventory was still written")
	}
}

func TestSaveRejectsAnUnsupportedFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := Save(path, File{Format: 99, Devices: []Device{testDevice()}}); err == nil {
		t.Fatal("Save accepted an unknown format version")
	}
}

func TestSaveOverwritesAnExistingFileAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")
	first := testDevice()
	if err := Save(path, File{Format: 1, Devices: []Device{first}}); err != nil {
		t.Fatal(err)
	}
	second := testDevice()
	second.Name = "renamed"
	if err := Save(path, File{Format: 1, Devices: []Device{second}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Devices) != 1 || loaded.Devices[0].Name != "renamed" {
		t.Errorf("loaded = %+v", loaded.Devices)
	}
	// No temporary files may be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".devices-") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestValidateRejectsBadFields(t *testing.T) {
	cases := map[string]func(*Device){
		"empty name":     func(d *Device) { d.Name = "  " },
		"non-numeric id": func(d *Device) { d.ID = "abc" },
		"zero id":        func(d *Device) { d.ID = "0" },
		"bad ip":         func(d *Device) { d.IP = "999.1.1.1" },
		"ipv6 ip":        func(d *Device) { d.IP = "::1" },
		"port too low":   func(d *Device) { d.Port = 0 },
		"port too high":  func(d *Device) { d.Port = 70000 },
		"bad type":       func(d *Device) { d.Type = 0x99 },
		"unsupported v2": func(d *Device) { d.Protocol = 2 },
		"short token":    func(d *Device) { d.Token = "00" },
		"short key":      func(d *Device) { d.Key = "11" },
		"non-hex token":  func(d *Device) { d.Token = strings.Repeat("zz", 64) },
		"non-hex key":    func(d *Device) { d.Key = strings.Repeat("zz", 32) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			device := testDevice()
			mutate(&device)
			if err := device.Validate(); err == nil {
				t.Errorf("Validate accepted %s", name)
			}
		})
	}
}

func TestValidateRejectsDuplicateAddresses(t *testing.T) {
	first := testDevice()
	second := testDevice()
	second.ID = "152832118193787"
	second.Name = "other"
	if err := (File{Format: 1, Devices: []Device{first, second}}).Validate(); err == nil {
		t.Fatal("Validate accepted two devices at the same address")
	}
}

func TestValidateRejectsDuplicateIDs(t *testing.T) {
	first := testDevice()
	second := testDevice()
	second.IP = "192.0.2.132"
	second.Name = "other"
	if err := (File{Format: 1, Devices: []Device{first, second}}).Validate(); err == nil {
		t.Fatal("Validate accepted two devices with the same id")
	}
}

func TestFindMatchesCaseInsensitively(t *testing.T) {
	file := File{Format: 1, Devices: []Device{testDevice()}}
	for _, selector := range []string{"bedroom", "BEDROOM", "  bedroom  "} {
		if _, err := file.Find(selector); err != nil {
			t.Errorf("Find(%q) failed: %v", selector, err)
		}
	}
}

func TestFindRejectsAnEmptySelector(t *testing.T) {
	file := File{Format: 1, Devices: []Device{testDevice()}}
	if _, err := file.Find("   "); err == nil {
		t.Fatal("Find accepted an empty selector")
	}
}

func TestCredentialStateFollowsTheStoredMaterial(t *testing.T) {
	device := testDevice()
	device.Token = strings.Repeat("ab", 64)
	device.Key = strings.Repeat("cd", 32)
	if got := credentialState(device); got != "stored" {
		t.Errorf("credentialState = %q, want stored", got)
	}
	device.Key = ""
	if got := credentialState(device); got != "missing" {
		t.Errorf("credentialState = %q, want missing", got)
	}
}

func TestStateAtVerificationSurvivesARoundTrip(t *testing.T) {
	device := testDevice()
	device.StateAtVerification = map[string]any{"power": false, "target_temperature": 22.0}
	device.LastVerified = time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := Save(path, File{Format: 1, Devices: []Device{device}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Devices[0].StateAtVerification) != 2 {
		t.Errorf("state_at_verification = %+v", loaded.Devices[0].StateAtVerification)
	}
	if !loaded.Devices[0].LastVerified.Equal(device.LastVerified) {
		t.Errorf("last_verified = %v, want %v", loaded.Devices[0].LastVerified, device.LastVerified)
	}
}
