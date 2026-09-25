// Command midea-control controls locally configured Midea air conditioners.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"midea-control/internal/config"
	"midea-control/internal/discovery"
	"midea-control/internal/mcpserver"
	"midea-control/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return nil
	case "version", "--version":
		fmt.Println(version.Value)
		return nil
	case "list":
		return runList(args[1:])
	case "audit":
		return runAudit(args[1:])
	case "discover":
		return runDiscover(args[1:])
	case "status":
		return runStatus(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "on":
		return runPower(args[1:], true)
	case "off":
		return runPower(args[1:], false)
	case "mcp":
		return runMCP(args[1:])
	default:
		printUsage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runList(args []string) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	file, err := config.Load(*path)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(file.PublicDevices())
	}
	fmt.Println("NAME                 IP             PORT  TYPE  PROTO  MODEL       CREDS   EXPIRY")
	for _, device := range file.PublicDevices() {
		expiry := "none reported"
		if device.Expiry != nil {
			expiry = device.Expiry.Format(time.RFC3339)
		}
		fmt.Printf("%-20s %-15s %5d  0x%02x  V%d     %-10s  %-7s  %s\n",
			device.Name, device.IP, device.Port, device.Type, device.Protocol,
			device.Model, device.CredentialState, expiry)
	}
	return nil
}

func runAudit(args []string) error {
	flags := flag.NewFlagSet("audit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	file, err := config.Load(*path)
	if err != nil {
		return err
	}
	info, err := os.Stat(*path)
	if err != nil {
		return err
	}
	fmt.Printf("config: %s (mode %04o)\n", *path, info.Mode().Perm())
	for _, device := range file.Devices {
		expiry := "none reported (client must re-authenticate with stored token/key)"
		if device.Expiry != nil {
			expiry = device.Expiry.Format(time.RFC3339)
		}
		fmt.Printf("%s %s protocol=V%d token=%d-hex key=%d-hex expiry=%s\n",
			device.Name, joinHostPort(device.IP, device.Port), device.Protocol, len(device.Token), len(device.Key), expiry)
	}
	return nil
}

func runDiscover(args []string) error {
	flags := flag.NewFlagSet("discover", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	target := flags.String("target", "", "private IPv4 unicast or broadcast address; empty scans local subnets")
	timeout := flags.Duration("timeout", 5*time.Second, "discovery timeout")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+time.Second)
	defer cancel()
	devices, err := discovery.Discover(ctx, discovery.Options{Target: *target, Timeout: *timeout})
	if err != nil {
		return err
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].IP < devices[j].IP })
	if *asJSON {
		return printJSON(devices)
	}
	if len(devices) == 0 {
		fmt.Println("no Midea devices found")
		return nil
	}
	for _, device := range devices {
		fmt.Printf("%s  id=%d  port=%d  type=0x%02x  protocol=V%d  model=%s\n",
			joinHostPort(device.IP, device.Port), device.ID,
			device.Port, device.Type, device.Protocol, device.Model)
	}
	return nil
}

func runStatus(args []string) error {
	selectors, flagArgs, _ := splitArgs(args, true)
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	timeout := flags.Duration("timeout", 30*time.Second, "per-device operation timeout")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	service := mcpserver.NewService()
	service.ConfigPath = *path
	file, err := config.Load(*path)
	if err != nil {
		return err
	}
	if len(selectors) == 0 {
		for _, device := range file.PublicDevices() {
			selectors = append(selectors, device.Name)
		}
	}
	results := make([]mcpserver.StatusResult, 0, len(selectors))
	for _, selector := range selectors {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		result, err := service.Status(ctx, selector)
		cancel()
		if err != nil {
			return err
		}
		results = append(results, result)
	}
	if *asJSON {
		return printJSON(results)
	}
	for _, result := range results {
		printStatus(result)
	}
	return nil
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	timeout := flags.Duration("timeout", 30*time.Second, "per-device operation timeout")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	service := mcpserver.NewService()
	service.ConfigPath = *path
	results, err := service.VerifyAllWithTimeout(context.Background(), *timeout)
	if err != nil {
		return err
	}
	if err := service.RecordVerification(results); err != nil {
		return err
	}
	if *asJSON {
		if err := printJSON(results); err != nil {
			return err
		}
	} else {
		for _, result := range results {
			if result.OK && result.State != nil {
				fmt.Printf("verified %s: LAN authentication succeeded, reachable=yes power=%t\n",
					result.Device.Name, result.State.Power)
				continue
			}
			fmt.Fprintf(os.Stderr, "verification failed for %s: %s\n", result.Device.Name, result.Error)
		}
	}
	for _, result := range results {
		if !result.OK {
			return fmt.Errorf("one or more devices could not be verified")
		}
	}
	return nil
}

func runPower(args []string, power bool) error {
	name := "on"
	if !power {
		name = "off"
	}
	selectors, flagArgs, err := splitArgs(args, false)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	selector := ""
	if len(selectors) == 1 {
		selector = selectors[0]
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	timeout := flags.Duration("timeout", 30*time.Second, "operation timeout")
	confirm := flags.Bool("confirm", false, "required confirmation for a physical write")
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if !*confirm {
		return fmt.Errorf("refusing physical write: pass -confirm")
	}
	if selector == "" {
		return fmt.Errorf("%s requires exactly one device selector", name)
	}
	service := mcpserver.NewService()
	service.ConfigPath = *path
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := service.SetPower(ctx, selector, power, true)
	if err != nil {
		return err
	}
	printStatus(result)
	return nil
}

// splitArgs accepts flags before or after positional selectors. The standard
// flag package stops at the first positional argument, so reorder the two forms
// before parsing.
func splitArgs(args []string, allowMultipleSelectors bool) ([]string, []string, error) {
	var selectors []string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			selectors = append(selectors, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			if (arg == "-config" || arg == "--config" || arg == "-timeout" || arg == "--timeout") && i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
			continue
		}
		selectors = append(selectors, arg)
	}
	if !allowMultipleSelectors && len(selectors) > 1 {
		return nil, nil, errors.New("requires exactly one device selector")
	}
	return selectors, flagArgs, nil
}

func runMCP(args []string) error {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	service := mcpserver.NewService()
	service.ConfigPath = *path
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return service.Run(ctx)
}

func printStatus(result mcpserver.StatusResult) {
	fmt.Printf("%s (%s): power=%t mode=%s target=%.1f°C indoor=%.1f°C outdoor=%.1f°C fan=%s\n",
		result.Device.Name, joinHostPort(result.Device.IP, result.Device.Port), result.State.Power, result.State.Mode,
		result.State.TargetTemperature, result.State.IndoorTemperature, result.State.OutdoorTemperature,
		result.State.FanSpeed)
}

func joinHostPort(ip string, port int) string {
	return fmt.Sprintf("%s:%d", ip, port)
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printUsage(out io.Writer) {
	fmt.Fprintf(out, `midea-control %s

Usage:
  midea-control list       [--config PATH] [--json]
  midea-control audit      [--config PATH]
  midea-control discover   [--target IP] [--timeout DURATION] [--json]
  midea-control status     [SELECTOR ...] [--config PATH] [--json]
  midea-control verify     [--config PATH] [--json]
  midea-control on         SELECTOR --confirm [--config PATH]
  midea-control off        SELECTOR --confirm [--config PATH]
  midea-control mcp        [--config PATH]

Selectors are exact configured names, IP addresses, or numeric device IDs.
Flags may appear before or after selectors; use -- before a selector that
starts with a dash. The mcp command speaks the Model Context Protocol over
stdio; no HTTP listener is opened. Tokens and keys are read only from the
mode-0600 inventory file and are never printed.
`, version.Value)
}
