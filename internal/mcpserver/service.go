// Package mcpserver exposes safe, local Midea operations over MCP stdio.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"midea-control/internal/config"
	"midea-control/internal/controller"
	"midea-control/internal/discovery"
)

const defaultOperationTimeout = 30 * time.Second

// Service contains the read/write operations shared by the CLI and MCP server.
type Service struct {
	ConfigPath string
	Control    *controller.Controller
	Discover   func(context.Context, discovery.Options) ([]discovery.Device, error)
}

// NewService creates a service using the default local configuration path.
func NewService() *Service {
	return &Service{
		ConfigPath: config.DefaultPath(),
		Control:    controller.New(),
		Discover:   discovery.Discover,
	}
}

// StatusResult combines public device metadata with a credential-free state.
type StatusResult struct {
	Device config.PublicDevice `json:"device"`
	State  controller.State    `json:"state"`
}

// VerificationResult reports each device independently so one expired or
// unreachable credential does not hide the other units.
type VerificationResult struct {
	Device config.PublicDevice `json:"device"`
	OK     bool                `json:"ok"`
	State  *controller.State   `json:"state,omitempty"`
	Error  string              `json:"error,omitempty"`
}

// ListDevices returns configured devices without exposing credentials.
func (s *Service) ListDevices() ([]config.PublicDevice, error) {
	file, err := config.Load(s.ConfigPath)
	if err != nil {
		return nil, err
	}
	return file.PublicDevices(), nil
}

// Status reads one configured device over the LAN.
func (s *Service) Status(ctx context.Context, selector string) (StatusResult, error) {
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	device, err := s.find(selector)
	if err != nil {
		return StatusResult{}, err
	}
	state, err := s.control().Poll(ctx, device)
	if err != nil {
		return StatusResult{}, err
	}
	return StatusResult{Device: device.Public(), State: state}, nil
}

// SetPower changes only the power bit after an explicit confirmation.
func (s *Service) SetPower(ctx context.Context, selector string, power, confirm bool) (StatusResult, error) {
	if !confirm {
		return StatusResult{}, errors.New("write refused: confirm must be true")
	}
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	device, err := s.find(selector)
	if err != nil {
		return StatusResult{}, err
	}
	state, err := s.control().SetPower(ctx, device, power)
	if err != nil {
		return StatusResult{}, err
	}
	return StatusResult{Device: device.Public(), State: state}, nil
}

// CapabilityResult pairs a device with its feature report.
type CapabilityResult struct {
	Device       config.PublicDevice     `json:"device"`
	Capabilities controller.Capabilities `json:"capabilities"`
}

// Apply changes device state after an explicit confirmation. The patch is
// sparse: unmentioned fields keep their current values.
func (s *Service) Apply(ctx context.Context, selector string, patch controller.Patch, confirm bool) (StatusResult, error) {
	if !confirm {
		return StatusResult{}, errors.New("write refused: confirm must be true")
	}
	if patch.Empty() {
		return StatusResult{}, errors.New("write refused: no fields to change")
	}
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	device, err := s.find(selector)
	if err != nil {
		return StatusResult{}, err
	}
	state, err := s.control().Apply(ctx, device, patch)
	if err != nil {
		return StatusResult{}, err
	}
	return StatusResult{Device: device.Public(), State: state}, nil
}

// Capabilities reports what a device says it supports.
func (s *Service) Capabilities(ctx context.Context, selector string) (CapabilityResult, error) {
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	device, err := s.find(selector)
	if err != nil {
		return CapabilityResult{}, err
	}
	capabilities, err := s.control().Capabilities(ctx, device)
	if err != nil {
		return CapabilityResult{}, err
	}
	return CapabilityResult{Device: device.Public(), Capabilities: capabilities}, nil
}

// VerifyAll authenticates every configured device using only stored LAN
// credentials and returns an independent result for each one. It is read-only.
func (s *Service) VerifyAll(ctx context.Context) ([]VerificationResult, error) {
	return s.verifyAll(ctx, defaultOperationTimeout)
}

// VerifyAllWithTimeout is the CLI-facing variant of VerifyAll. It is also
// read-only; the CLI may explicitly persist metadata with RecordVerification.
func (s *Service) VerifyAllWithTimeout(ctx context.Context, timeout time.Duration) ([]VerificationResult, error) {
	if timeout <= 0 {
		timeout = defaultOperationTimeout
	}
	return s.verifyAll(ctx, timeout)
}

func (s *Service) verifyAll(ctx context.Context, timeout time.Duration) ([]VerificationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	file, err := config.Load(s.ConfigPath)
	if err != nil {
		return nil, err
	}
	results := make([]VerificationResult, 0, len(file.Devices))
	for _, device := range file.Devices {
		operationCtx, cancel := context.WithTimeout(ctx, timeout)
		state, pollErr := s.control().Poll(operationCtx, device)
		cancel()
		result := VerificationResult{Device: device.Public(), OK: pollErr == nil}
		if pollErr != nil {
			result.Error = pollErr.Error()
		} else {
			stateCopy := state
			result.State = &stateCopy
		}
		results = append(results, result)
	}
	return results, nil
}

// RecordVerification persists successful verification timestamps without
// changing any credential. It is intentionally separate from read-only MCP
// verification.
func (s *Service) RecordVerification(results []VerificationResult) error {
	successful := make(map[string]bool)
	for _, result := range results {
		if result.OK {
			successful[result.Device.ID] = true
		}
	}
	if len(successful) == 0 {
		return nil
	}
	file, err := config.Load(s.ConfigPath)
	if err != nil {
		return err
	}
	verifiedAt := time.Now().UTC()
	for index, device := range file.Devices {
		if successful[device.ID] {
			file.Devices[index].LastVerified = verifiedAt
		}
	}
	if err := config.Save(s.ConfigPath, file); err != nil {
		return fmt.Errorf("save verification metadata: %w", err)
	}
	return nil
}

// DiscoverDevices performs a credential-free LAN broadcast scan.
func (s *Service) DiscoverDevices(ctx context.Context, options discovery.Options) ([]discovery.Device, error) {
	if options.Timeout <= 0 {
		options.Timeout = 5 * time.Second
	}
	if s.Discover == nil {
		return nil, errors.New("discovery function is not configured")
	}
	return s.Discover(ctx, options)
}

func (s *Service) find(selector string) (config.Device, error) {
	file, err := config.Load(s.ConfigPath)
	if err != nil {
		return config.Device{}, err
	}
	return file.Find(selector)
}

func boundedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultOperationTimeout)
}

func (s *Service) control() *controller.Controller {
	if s.Control != nil {
		return s.Control
	}
	return controller.New()
}

// ToolInput types are intentionally small: the MCP boundary never accepts raw
// tokens, keys, or protocol frames. The optional discovery target is a
// credential-free, read-only probe and is not usable for control.
type emptyInput struct{}

type deviceInput struct {
	Device string `json:"device" jsonschema:"configured device name, IP address, or numeric device id"`
}

type discoverInput struct {
	Target  string `json:"target,omitempty" jsonschema:"optional private IPv4 unicast or broadcast address; empty means local subnet broadcast"`
	Timeout int    `json:"timeout_seconds,omitempty" jsonschema:"discovery timeout in seconds, 1 to 30"`
}

type powerInput struct {
	Device  string `json:"device" jsonschema:"configured device name, IP address, or numeric device id"`
	Power   bool   `json:"power" jsonschema:"true to turn on, false to turn off"`
	Confirm bool   `json:"confirm" jsonschema:"must be true; confirms this physical write"`
}

// setStateInput carries a sparse patch. Omitted fields are left unchanged.
type setStateInput struct {
	Device  string   `json:"device" jsonschema:"configured device name, IP address, or numeric device id"`
	Power   *bool    `json:"power,omitempty" jsonschema:"turn the unit on or off"`
	Mode    string   `json:"mode,omitempty" jsonschema:"operating mode: auto, cool, dry, heat, fan, smart_dry"`
	Temp    *float64 `json:"temp_c,omitempty" jsonschema:"target temperature in °C, 0.5 steps, 17-30"`
	Fan     string   `json:"fan,omitempty" jsonschema:"fan speed: auto, silent, low, medium, high, full"`
	SwingV  *bool    `json:"swing_v,omitempty" jsonschema:"vertical louver swing"`
	SwingH  *bool    `json:"swing_h,omitempty" jsonschema:"horizontal louver swing"`
	Eco     *bool    `json:"eco,omitempty" jsonschema:"eco mode"`
	Turbo   *bool    `json:"turbo,omitempty" jsonschema:"turbo mode"`
	Sleep   *bool    `json:"sleep,omitempty" jsonschema:"sleep mode"`
	Display *bool    `json:"display,omitempty" jsonschema:"indoor display"`
	Confirm bool     `json:"confirm" jsonschema:"must be true; confirms this physical write"`
}

func (i setStateInput) patch() controller.Patch {
	return controller.Patch{
		Power:      i.Power,
		Mode:       i.Mode,
		TargetTemp: i.Temp,
		FanSpeed:   i.Fan,
		SwingV:     i.SwingV,
		SwingH:     i.SwingH,
		Eco:        i.Eco,
		Turbo:      i.Turbo,
		Sleep:      i.Sleep,
		Display:    i.Display,
	}
}

type listOutput struct {
	Devices []config.PublicDevice `json:"devices" jsonschema:"configured Midea devices; credentials are never returned"`
}

type statusOutput struct {
	Result StatusResult `json:"result" jsonschema:"current device state"`
}

type discoverOutput struct {
	Devices []discovery.Device `json:"devices" jsonschema:"devices found on the local Midea broadcast domain"`
}

type verifyOutput struct {
	Results []VerificationResult `json:"results" jsonschema:"per-device read-back results proving which stored LAN credentials still authenticate"`
}

type powerOutput struct {
	Result StatusResult `json:"result" jsonschema:"state read back after the power write"`
}

type capabilityOutput struct {
	Result CapabilityResult `json:"result" jsonschema:"device feature report"`
}
