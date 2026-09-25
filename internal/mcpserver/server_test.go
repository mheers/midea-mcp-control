package mcpserver

import (
	"context"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPServerExposesToolsOverMemoryTransport(t *testing.T) {
	service := testService(t, &fakeClient{})
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	server := NewMCPServer(service)
	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 5 {
		t.Fatalf("tool count = %d, want 5", len(tools.Tools))
	}
	result, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{Name: "list_devices"})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("list_devices returned tool error: %+v", result)
	}
	text := contentText(result)
	if !strings.Contains(text, "bedroom") {
		t.Fatalf("list_devices output = %q, want device name", text)
	}

	powerResult, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "set_power",
		Arguments: map[string]any{
			"device":  "bedroom",
			"power":   true,
			"confirm": false,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !powerResult.IsError {
		t.Fatal("set_power without confirmation was not reported as a tool error")
	}

	discoveryResult, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "discover_devices",
		Arguments: map[string]any{"timeout_seconds": 31},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !discoveryResult.IsError {
		t.Fatal("discover_devices accepted an out-of-range timeout")
	}
}

func contentText(result *sdkmcp.CallToolResult) string {
	var builder strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*sdkmcp.TextContent); ok {
			builder.WriteString(text.Text)
		}
	}
	return builder.String()
}
