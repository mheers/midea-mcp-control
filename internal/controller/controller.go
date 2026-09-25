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

	"midea-control/internal/config"
)

// Client is the subset of the protocol client used by Controller.
type Client interface {
	Connect(context.Context) error
	Close() error
	Poll(context.Context) (midea.Response, error)
	Update(context.Context, midea.Request) (midea.Response, error)
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

// SetPower changes only the power bit and verifies the resulting state.
//
// The command is sent once. If the device accepts the command but the
// independent read-back fails, the returned error explicitly says delivery is
// unknown; callers must not blindly retry.
func (c *Controller) SetPower(ctx context.Context, device config.Device, power bool) (State, error) {
	if c == nil {
		return State{}, errors.New("controller is nil")
	}
	unlock := c.lockDevice(device.ID)
	defer unlock()
	client, err := c.client(device, WriteOperation)
	if err != nil {
		return State{}, err
	}
	defer client.Close()
	if err := client.Connect(ctx); err != nil {
		return State{}, fmt.Errorf("connect %s: %w", device.Name, err)
	}
	if _, err := client.Poll(ctx); err != nil {
		return State{}, fmt.Errorf("seed state for %s: %w", device.Name, err)
	}
	request := midea.Request{Power: &power}
	commandResponse, err := client.Update(ctx, request)
	if err != nil {
		return State{}, fmt.Errorf("set power for %s: %w", device.Name, err)
	}
	if commandResponse.Error {
		return State{}, fmt.Errorf("device %s reported error code %d while setting power", device.Name, commandResponse.ErrCode)
	}
	response, err := client.Poll(ctx)
	if err != nil {
		return State{}, fmt.Errorf("power command sent to %s but read-back failed; delivery unknown: %w", device.Name, err)
	}
	state := stateFromResponse(response)
	if state.Power != power {
		return state, fmt.Errorf("power command sent to %s but read-back reported power=%t, requested %t", device.Name, state.Power, power)
	}
	return state, nil
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
		Mode:               response.Mode.String(),
		TargetTemperature:  response.TargetTemp,
		IndoorTemperature:  response.IndoorTemp,
		OutdoorTemperature: response.OutdoorTemp,
		FanSpeed:           response.FanSpeed.String(),
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
