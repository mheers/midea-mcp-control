// Package controller wraps the local Midea V3 protocol behind a small,
// safety-oriented interface.
package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	midea "github.com/thekondor/midea-porta-split"

	"github.com/mheers/midea-mcp-control/internal/config"
)

// Client is the subset of the protocol client used by Controller.
type Client interface {
	Connect(context.Context) error
	Close() error
	Poll(context.Context) (midea.Response, error)
	Update(context.Context, midea.Request) (midea.Response, error)
}

// capabilityClient is an optional extension implemented by clients that can
// query a device's feature report. Keeping it separate means a client without
// capability support still works, falling back to conservative limits.
type capabilityClient interface {
	Capabilities(context.Context) (midea.Capabilities, error)
}

// Operation selects protocol retry behavior for a client.
type Operation bool

const (
	// ReadOperation allows a small number of read retries.
	ReadOperation Operation = false
	// WriteOperation disables automatic retries so a physical write is sent once.
	WriteOperation Operation = true
)

// ClientFactory creates a protocol client for a configured device and operation.
type ClientFactory func(config.Device, Operation) (Client, error)

// State is the public, credential-free state returned by the controller.
type State struct {
	Power              bool    `json:"power"`
	Mode               string  `json:"mode"`
	TargetTemperature  float64 `json:"target_temperature"`
	IndoorTemperature  float64 `json:"indoor_temperature"`
	OutdoorTemperature float64 `json:"outdoor_temperature"`
	FanSpeed           string  `json:"fan_speed"`
	SwingVertical      bool    `json:"swing_vertical"`
	SwingHorizontal    bool    `json:"swing_horizontal"`
	Eco                bool    `json:"eco"`
	Turbo              bool    `json:"turbo"`
	Sleep              bool    `json:"sleep"`
	DisplayOn          bool    `json:"display_on"`
	Humidity           int     `json:"humidity"`
	Error              bool    `json:"error"`
	ErrorCode          int     `json:"error_code,omitempty"`
}

// Controller serializes operations per physical device while allowing
// independent devices to be polled concurrently.
type Controller struct {
	NewClient ClientFactory

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New returns a controller using the pinned V3 protocol implementation.
func New() *Controller {
	return &Controller{NewClient: newProtocolClient}
}

func newProtocolClient(device config.Device, operation Operation) (Client, error) {
	if device.Protocol != 3 {
		return nil, fmt.Errorf("protocol %d is not supported by the native adapter", device.Protocol)
	}
	token, err := hex.DecodeString(device.Token)
	if err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}
	key, err := hex.DecodeString(device.Key)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	id, err := strconv.ParseUint(device.ID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse device id: %w", err)
	}
	client, err := midea.NewClient(net.JoinHostPort(device.IP, strconv.Itoa(device.Port)), token, key, id, protocolOptions(operation))
	if err != nil {
		return nil, err
	}
	return client, nil
}

func protocolOptions(operation Operation) midea.Options {
	options := midea.DefaultOptions()
	// The upstream client retries timed-out commands by rebuilding and
	// resending them. Reads may retry a couple of times; physical writes never
	// do, so a set command cannot be transmitted more than once.
	if operation == WriteOperation {
		options.CmdMaxAttempts = 1
	} else {
		options.CmdMaxAttempts = 2
	}
	return options
}

// Poll reads the current state from one configured device.
func (c *Controller) Poll(ctx context.Context, device config.Device) (State, error) {
	if c == nil {
		return State{}, errors.New("controller is nil")
	}
	unlock := c.lockDevice(device.ID)
	defer unlock()
	return c.pollLocked(ctx, device)
}

// pollLocked performs a read. The caller must already hold the device lock.
func (c *Controller) pollLocked(ctx context.Context, device config.Device) (State, error) {
	client, err := c.client(device, ReadOperation)
	if err != nil {
		return State{}, err
	}
	defer client.Close()
	if err := client.Connect(ctx); err != nil {
		return State{}, fmt.Errorf("connect %s: %w", device.Name, err)
	}
	response, err := client.Poll(ctx)
	if err != nil {
		return State{}, fmt.Errorf("poll %s: %w", device.Name, err)
	}
	return stateFromResponse(response), nil
}

// Capabilities returns the device's advertised feature flags. Firmware may
// leave fields unreported, in which case they come back as zero values.
func (c *Controller) Capabilities(ctx context.Context, device config.Device) (Capabilities, error) {
	if c == nil {
		return Capabilities{}, errors.New("controller is nil")
	}
	unlock := c.lockDevice(device.ID)
	defer unlock()
	return c.capabilitiesLocked(ctx, device)
}

// capabilitiesLocked queries the feature report. The caller must hold the lock.
func (c *Controller) capabilitiesLocked(ctx context.Context, device config.Device) (Capabilities, error) {
	client, err := c.client(device, ReadOperation)
	if err != nil {
		return Capabilities{}, err
	}
	defer client.Close()
	if err := client.Connect(ctx); err != nil {
		return Capabilities{}, fmt.Errorf("connect %s: %w", device.Name, err)
	}
	capabilityQuery, ok := client.(capabilityClient)
	if !ok {
		return Capabilities{}, errors.New("client does not support capability queries")
	}
	capabilities, err := capabilityQuery.Capabilities(ctx)
	if err != nil {
		return Capabilities{}, fmt.Errorf("capabilities %s: %w", device.Name, err)
	}
	return fromProtocolCapabilities(capabilities), nil
}

// energyClient is an optional extension for clients that can measure power.
type energyClient interface {
	Energy(context.Context) (midea.Energy, error)
}

// Energy returns a device's power measurement. Firmware that does not support
// the query reports zero values rather than failing.
func (c *Controller) Energy(ctx context.Context, device config.Device) (Energy, error) {
	if c == nil {
		return Energy{}, errors.New("controller is nil")
	}
	unlock := c.lockDevice(device.ID)
	defer unlock()
	client, err := c.client(device, ReadOperation)
	if err != nil {
		return Energy{}, err
	}
	defer client.Close()
	if err := client.Connect(ctx); err != nil {
		return Energy{}, fmt.Errorf("connect %s: %w", device.Name, err)
	}
	query, ok := client.(energyClient)
	if !ok {
		return Energy{}, errors.New("client does not support energy queries")
	}
	energy, err := query.Energy(ctx)
	if err != nil {
		return Energy{}, fmt.Errorf("energy %s: %w", device.Name, err)
	}
	return fromProtocolEnergy(energy), nil
}

// SetPower changes only the power bit and verifies the resulting state.
func (c *Controller) SetPower(ctx context.Context, device config.Device, power bool) (State, error) {
	return c.Apply(ctx, device, Patch{Power: &power})
}

// Apply sends a sparse patch, seeded with the device's current state, and
// verifies every requested field in an independent read-back.
//
// The command is transmitted at most once. If the device accepts the frame but
// the read-back does not show the requested values, the error says so
// explicitly; callers must not blindly retry, because a physical device has no
// exactly-once semantics here.
func (c *Controller) Apply(ctx context.Context, device config.Device, patch Patch) (State, error) {
	if c == nil {
		return State{}, errors.New("controller is nil")
	}
	if patch.Empty() {
		return State{}, errors.New("no fields to change")
	}

	unlock := c.lockDevice(device.ID)
	defer unlock()

	// Validate against the device's own reported limits when it publishes any.
	// This runs on a read connection so it cannot interfere with the
	// single-attempt write policy below.
	if patch.TargetTemp != nil {
		if capabilities, err := c.capabilitiesLocked(ctx, device); err == nil {
			patch.limits = limits{
				min: minPositive(capabilities.MinHeatTemp, capabilities.MinCoolTemp),
				max: maxPositive(capabilities.MaxHeatTemp, capabilities.MaxCoolTemp),
			}
		}
	}
	request, want, err := patch.request(patch.limits)
	if err != nil {
		return State{}, err
	}

	// Write on a connection that is allowed exactly one attempt: a set frame
	// must never be transmitted twice.
	writeClient, err := c.client(device, WriteOperation)
	if err != nil {
		return State{}, err
	}
	if err := writeClient.Connect(ctx); err != nil {
		_ = writeClient.Close()
		return State{}, fmt.Errorf("connect %s: %w", device.Name, err)
	}
	if _, err := writeClient.Poll(ctx); err != nil {
		_ = writeClient.Close()
		return State{}, fmt.Errorf("seed state for %s: %w", device.Name, err)
	}
	commandResponse, updateErr := writeClient.Update(ctx, request)
	closeErr := writeClient.Close()
	if updateErr != nil {
		return State{}, fmt.Errorf("apply %v to %s: %w", patch.FieldNames(), device.Name, updateErr)
	}
	if closeErr != nil {
		return State{}, fmt.Errorf("apply %v to %s: close: %w", patch.FieldNames(), device.Name, closeErr)
	}
	if commandResponse.Error {
		return State{}, fmt.Errorf(
			"device %s reported error code %d while applying %v",
			device.Name, commandResponse.ErrCode, patch.FieldNames())
	}

	// Verify on a fresh read connection. A unit can drop the response to a
	// query that immediately follows a set frame, so verification gets the
	// read retry budget; this never re-sends the command itself.
	state, err := c.pollLocked(ctx, device)
	if err != nil {
		return State{}, fmt.Errorf(
			"command sent to %s but read-back failed; delivery unknown: %w", device.Name, err)
	}
	if mismatch := want.mismatch(state); mismatch != "" {
		return state, fmt.Errorf(
			"command sent to %s but read-back disagrees: %s", device.Name, mismatch)
	}
	return state, nil
}

// capabilities queries the device's feature report on a short-lived read
// connection.
func (c *Controller) capabilities(ctx context.Context, device config.Device) (Capabilities, error) {
	unlock := c.lockDevice(device.ID)
	defer unlock()
	return c.capabilitiesLocked(ctx, device)
}

func minPositive(values ...float64) float64 {
	result := 0.0
	for _, value := range values {
		if value > 0 && (result == 0 || value < result) {
			result = value
		}
	}
	return result
}

func maxPositive(values ...float64) float64 {
	result := 0.0
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}

func (c *Controller) lockDevice(id string) func() {
	c.mu.Lock()
	if c.locks == nil {
		c.locks = make(map[string]*sync.Mutex)
	}
	lock := c.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		c.locks[id] = lock
	}
	c.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (c *Controller) client(device config.Device, operation Operation) (Client, error) {
	if c == nil || c.NewClient == nil {
		return nil, errors.New("controller has no client factory")
	}
	return c.NewClient(device, operation)
}

func stateFromResponse(response midea.Response) State {
	return State{
		Power:              response.Power,
		Mode:               modeName(response.Mode),
		TargetTemperature:  response.TargetTemp,
		IndoorTemperature:  response.IndoorTemp,
		OutdoorTemperature: response.OutdoorTemp,
		FanSpeed:           fanName(response.FanSpeed),
		SwingVertical:      response.SwingV,
		SwingHorizontal:    response.SwingH,
		Eco:                response.Eco,
		Turbo:              response.Turbo,
		Sleep:              response.Sleep,
		DisplayOn:          response.DisplayOn,
		Humidity:           response.Humidity,
		Error:              response.Error,
		ErrorCode:          response.ErrCode,
	}
}
