package mcpserver

import (
	"context"
	"errors"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"midea-control/internal/discovery"
	"midea-control/internal/version"
)

// NewMCPServer builds the stdio MCP server for a service.
func NewMCPServer(service *Service) *sdkmcp.Server {
	openWorld := true
	readOnly := &sdkmcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}
	destructive := true
	powerWrite := &sdkmcp.ToolAnnotations{
		DestructiveHint: &destructive,
		IdempotentHint:  true,
		OpenWorldHint:   &openWorld,
	}

	server := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:        "midea-control",
		Version:     version.Value,
		Description: "Credential-free status and explicitly confirmed power control for locally configured Midea air conditioners.",
	}, nil)

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "list_devices",
		Description: "List configured Midea devices without returning tokens or keys.",
		Annotations: readOnly,
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, _ emptyInput) (*sdkmcp.CallToolResult, listOutput, error) {
		devices, err := service.ListDevices()
		return nil, listOutput{Devices: devices}, err
	})

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "verify_credentials",
		Description: "Read-only check: authenticate every configured Midea device using its stored LAN token/key and return per-device results.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ emptyInput) (*sdkmcp.CallToolResult, verifyOutput, error) {
		results, err := service.VerifyAll(ctx)
		return nil, verifyOutput{Results: results}, err
	})

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "discover_devices",
		Description: "Discover Midea devices on the local LAN. This is read-only and does not use cloud credentials.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, input discoverInput) (*sdkmcp.CallToolResult, discoverOutput, error) {
		timeout := 5
		if input.Timeout != 0 {
			timeout = input.Timeout
		}
		if timeout < 1 || timeout > 30 {
			return nil, discoverOutput{}, errors.New("timeout_seconds must be between 1 and 30")
		}
		devices, err := service.DiscoverDevices(ctx, discovery.Options{
			Target:  input.Target,
			Timeout: time.Duration(timeout) * time.Second,
		})
		return nil, discoverOutput{Devices: devices}, err
	})

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "get_status",
		Description: "Read one configured Midea device's current state over the LAN.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, input deviceInput) (*sdkmcp.CallToolResult, statusOutput, error) {
		result, err := service.Status(ctx, input.Device)
		return nil, statusOutput{Result: result}, err
	})

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "set_power",
		Description: "Turn a configured Midea device on or off. This is a physical write; confirm must be true and the state is read back.",
		Annotations: powerWrite,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, input powerInput) (*sdkmcp.CallToolResult, powerOutput, error) {
		result, err := service.SetPower(ctx, input.Device, input.Power, input.Confirm)
		return nil, powerOutput{Result: result}, err
	})

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "set_state",
		Description: "Change temperature, mode, fan speed, swing, eco, turbo, sleep or display on a configured device. Sparse: omitted fields are left as they are. This is a physical write; confirm must be true and every requested field is verified by read-back.",
		Annotations: powerWrite,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, input setStateInput) (*sdkmcp.CallToolResult, powerOutput, error) {
		result, err := service.Apply(ctx, input.Device, input.patch(), input.Confirm)
		return nil, powerOutput{Result: result}, err
	})

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "get_capabilities",
		Description: "Read a configured device's advertised feature report, including temperature limits when it reports any.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, input deviceInput) (*sdkmcp.CallToolResult, capabilityOutput, error) {
		result, err := service.Capabilities(ctx, input.Device)
		return nil, capabilityOutput{Result: result}, err
	})

	return server
}

// Run serves MCP over stdin/stdout until the client disconnects.
func (s *Service) Run(ctx context.Context) error {
	return NewMCPServer(s).Run(ctx, &sdkmcp.StdioTransport{})
}
