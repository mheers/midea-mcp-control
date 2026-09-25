// Package config loads and validates the local Midea device inventory.
package config

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const currentFormat = 1

// Device is a configured V3 Midea air conditioner.
type Device struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	IP                  string         `json:"ip"`
	Port                int            `json:"port"`
	Type                int            `json:"type"`
	Model               string         `json:"model"`
	Protocol            int            `json:"protocol"`
	MAC                 string         `json:"mac,omitempty"`
	Token               string         `json:"token,omitempty"`
	Key                 string         `json:"key,omitempty"`
	CredentialMethod    int            `json:"credential_method,omitempty"`
	ObtainedAt          time.Time      `json:"obtained_at,omitempty"`
	LastVerified        time.Time      `json:"last_verified,omitempty"`
	Expiry              *time.Time     `json:"expiry"`
	StateAtVerification map[string]any `json:"state_at_verification,omitempty"`
}

// File is the on-disk device inventory.
type File struct {
	Format  int      `json:"format"`
	Devices []Device `json:"devices"`
}

// PublicDevice is safe to print or return from an API.
type PublicDevice struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	IP               string     `json:"ip"`
	Port             int        `json:"port"`
	Type             int        `json:"type"`
	Model            string     `json:"model"`
	Protocol         int        `json:"protocol"`
	MAC              string     `json:"mac,omitempty"`
	CredentialMethod int        `json:"credential_method,omitempty"`
	CredentialState  string     `json:"credential_state,omitempty"`
	ObtainedAt       time.Time  `json:"obtained_at,omitempty"`
	LastVerified     time.Time  `json:"last_verified,omitempty"`
	Expiry           *time.Time `json:"expiry"`
}

// DefaultPath returns the user-local inventory path.
func DefaultPath() string {
	if path := os.Getenv("MIDEA_CONTROL_CONFIG"); path != "" {
		return path
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(".config", "midea-control", "devices.json")
	}
	return filepath.Join(dir, "midea-control", "devices.json")
}

// Load reads and validates an inventory file.
func Load(path string) (File, error) {
	if path == "" {
		path = DefaultPath()
	}
	info, err := os.Lstat(path)
	if err != nil {
		return File{}, fmt.Errorf("stat config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return File{}, errors.New("config must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return File{}, fmt.Errorf("config permissions %04o are too broad; use 0600", info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read config: %w", err)
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return File{}, fmt.Errorf("decode config: %w", err)
	}
	if file.Format == 0 {
		file.Format = currentFormat
	}
	if err := file.Validate(); err != nil {
		return File{}, err
	}
	return file, nil
}

// Save atomically writes an inventory with restrictive permissions.
func Save(path string, file File) error {
	if path == "" {
		path = DefaultPath()
	}
	if file.Format == 0 {
		file.Format = currentFormat
	}
	if err := file.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".devices-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	if directory, err := os.Open(filepath.Dir(path)); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

// Validate checks the inventory and all device credentials.
func (f File) Validate() error {
	if f.Format != currentFormat {
		return fmt.Errorf("unsupported config format %d", f.Format)
	}
	if len(f.Devices) == 0 {
		return errors.New("config contains no devices")
	}
	seenID := make(map[string]struct{}, len(f.Devices))
	seenName := make(map[string]struct{}, len(f.Devices))
	seenIP := make(map[string]struct{}, len(f.Devices))
	for _, device := range f.Devices {
		if err := device.Validate(); err != nil {
			return err
		}
		if _, ok := seenID[device.ID]; ok {
			return fmt.Errorf("duplicate device id %s", device.ID)
		}
		seenID[device.ID] = struct{}{}
		nameKey := strings.ToLower(strings.TrimSpace(device.Name))
		if _, ok := seenName[nameKey]; ok {
			return fmt.Errorf("duplicate device name %q", device.Name)
		}
		seenName[nameKey] = struct{}{}
		ipKey := net.JoinHostPort(device.IP, strconv.Itoa(device.Port))
		if _, ok := seenIP[ipKey]; ok {
			return fmt.Errorf("duplicate device address %s", ipKey)
		}
		seenIP[ipKey] = struct{}{}
	}
	return nil
}

// Validate checks one device and its V3 credential sizes.
func (d Device) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return errors.New("device name is required")
	}
	if _, err := strconv.ParseUint(d.ID, 10, 64); err != nil || d.ID == "0" {
		return fmt.Errorf("invalid device id %q", d.ID)
	}
	ip := net.ParseIP(d.IP)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid device IPv4 address %q", d.IP)
	}
	if d.Port < 1 || d.Port > 65535 {
		return fmt.Errorf("invalid device port %d", d.Port)
	}
	if d.Type != 0xac && d.Type != 0xcc {
		return fmt.Errorf("unsupported device type 0x%02x", d.Type)
	}
	if d.Protocol != 3 {
		return fmt.Errorf("unsupported LAN protocol %d; the native controller supports V3 only", d.Protocol)
	}
	if len(d.Token) != 128 {
		return fmt.Errorf("device %s has invalid token length %d", d.Name, len(d.Token))
	}
	if len(d.Key) != 64 {
		return fmt.Errorf("device %s has invalid key length %d", d.Name, len(d.Key))
	}
	if _, err := hex.DecodeString(d.Token); err != nil {
		return fmt.Errorf("device %s token is not hexadecimal", d.Name)
	}
	if _, err := hex.DecodeString(d.Key); err != nil {
		return fmt.Errorf("device %s key is not hexadecimal", d.Name)
	}
	return nil
}

func credentialState(d Device) string {
	if len(d.Token) == 128 && len(d.Key) == 64 {
		return "stored"
	}
	return "missing"
}

// Public removes secrets from a device before output.
func (d Device) Public() PublicDevice {
	return PublicDevice{
		ID:               d.ID,
		Name:             d.Name,
		IP:               d.IP,
		Port:             d.Port,
		Type:             d.Type,
		Model:            d.Model,
		Protocol:         d.Protocol,
		MAC:              d.MAC,
		CredentialMethod: d.CredentialMethod,
		CredentialState:  credentialState(d),
		ObtainedAt:       d.ObtainedAt,
		LastVerified:     d.LastVerified,
		Expiry:           d.Expiry,
	}
}

// Find resolves an exact name, IP address, or device ID.
func (f File) Find(selector string) (Device, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return Device{}, errors.New("device selector is required")
	}
	var matches []Device
	for _, device := range f.Devices {
		if strings.EqualFold(device.Name, selector) || device.IP == selector || device.ID == selector {
			matches = append(matches, device)
		}
	}
	switch len(matches) {
	case 0:
		return Device{}, fmt.Errorf("device %q not found", selector)
	case 1:
		return matches[0], nil
	default:
		return Device{}, fmt.Errorf("device selector %q is ambiguous", selector)
	}
}

// PublicDevices returns all devices without credentials.
func (f File) PublicDevices() []PublicDevice {
	result := make([]PublicDevice, 0, len(f.Devices))
	for _, device := range f.Devices {
		result = append(result, device.Public())
	}
	return result
}
