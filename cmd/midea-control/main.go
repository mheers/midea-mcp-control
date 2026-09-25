// Command midea-control controls locally configured Midea air conditioners.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"midea-control/internal/bootstrap"
	"midea-control/internal/config"
	"midea-control/internal/controller"
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
	case "set":
		return runSet(args[1:])
	case "capabilities":
		return runCapabilities(args[1:])
	case "energy":
		return runEnergy(args[1:])
	case "bootstrap":
		return runBootstrap(args[1:])
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
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	timeout := flags.Duration("timeout", 30*time.Second, "per-device operation timeout")
	asJSON := flags.Bool("json", false, "print JSON")
	selectors, flagArgs, _ := splitArgs(flags, args, true)
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
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	timeout := flags.Duration("timeout", 30*time.Second, "operation timeout")
	confirm := flags.Bool("confirm", false, "required confirmation for a physical write")
	selectors, flagArgs, err := splitArgs(flags, args, false)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	selector := ""
	if len(selectors) == 1 {
		selector = selectors[0]
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
// before parsing. The FlagSet is consulted to learn which flags consume a
// following value, so this works for any command.
func splitArgs(flags *flag.FlagSet, args []string, allowMultipleSelectors bool) ([]string, []string, error) {
	var selectors []string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			selectors = append(selectors, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			// Accept both "--flag" and "--flag true" for boolean flags: the
			// standard flag package only understands "--flag=true".
			if booleanValueFollows(flags, arg, args, i) {
				flagArgs = append(flagArgs, arg+"="+args[i+1])
				i++
				continue
			}
			flagArgs = append(flagArgs, arg)
			if i+1 < len(args) && flagConsumesValue(flags, arg) {
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

// flagConsumesValue reports whether a flag takes the next argument as its
// value. Boolean flags do not.
func flagConsumesValue(flags *flag.FlagSet, arg string) bool {
	entry := lookupFlag(flags, arg)
	if entry == nil {
		return false // unknown flag: let the parser report it
	}
	boolean, ok := entry.Value.(interface{ IsBoolFlag() bool })
	return !ok || !boolean.IsBoolFlag()
}

// booleanValueFollows detects the "--flag true" spelling for a boolean flag.
func booleanValueFollows(flags *flag.FlagSet, arg string, args []string, i int) bool {
	if i+1 >= len(args) {
		return false
	}
	value := args[i+1]
	if value != "true" && value != "false" {
		return false
	}
	entry := lookupFlag(flags, arg)
	if entry == nil {
		return false
	}
	boolean, ok := entry.Value.(interface{ IsBoolFlag() bool })
	return ok && boolean.IsBoolFlag()
}

func lookupFlag(flags *flag.FlagSet, arg string) *flag.Flag {
	name := strings.TrimLeft(arg, "-")
	if index := strings.Index(name, "="); index >= 0 {
		return nil // the value is already attached
	}
	return flags.Lookup(name)
}

// runBootstrap performs the one-time cloud credential bootstrap. It is
// deliberately not exposed through MCP: it needs account credentials.
func runBootstrap(args []string) error {
	flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	account := flags.String("account", "", "cloud account (or set MIDEA_ACCOUNT)")
	cloudName := flags.String("cloud", "SmartHome", "cloud identity to use")
	device := flags.String("device", "", "limit to one device name, IP or id")
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	timeout := flags.Duration("timeout", 20*time.Second, "per-operation timeout")
	dryRun := flags.Bool("dry-run", false, "verify everything but do not write the inventory")
	if err := flags.Parse(args); err != nil {
		return err
	}

	resolvedAccount := *account
	if resolvedAccount == "" {
		resolvedAccount = os.Getenv("MIDEA_ACCOUNT")
	}
	if resolvedAccount == "" {
		fmt.Fprint(os.Stderr, "Midea account: ")
		value, err := readLine(os.Stdin)
		if err != nil {
			return fmt.Errorf("read account: %w", err)
		}
		resolvedAccount = value
	}
	if resolvedAccount == "" {
		return errors.New("an account is required (--account or MIDEA_ACCOUNT)")
	}

	password, err := readPassword()
	if err != nil {
		return err
	}

	runner := bootstrap.New()
	runner.ConfigPath = *path
	runner.CloudName = *cloudName
	runner.Account = resolvedAccount
	runner.Password = password
	runner.Selector = *device
	runner.OperationTimeout = *timeout
	runner.DryRun = *dryRun
	runner.Out = os.Stdout

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	_, err = runner.Run(ctx)
	return err
}

// readPassword takes the password from MIDEA_PASSWORD or a hidden prompt. It
// never echoes the value and never writes it anywhere.
func readPassword() (string, error) {
	if password := os.Getenv("MIDEA_PASSWORD"); password != "" {
		return password, nil
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Midea password: ")
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if len(password) == 0 {
			return "", errors.New("an empty password is not accepted")
		}
		return string(password), nil
	}
	fmt.Fprintln(os.Stderr, "Midea password (set MIDEA_PASSWORD to avoid echoing):")
	return readLine(os.Stdin)
}

func readLine(reader io.Reader) (string, error) {
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// optionalBool is a tri-state flag: unset, explicitly true, or explicitly
// false. It is needed because a plain bool flag cannot express "leave this
// field alone".
type optionalBool struct {
	set   bool
	value bool
}

func (o *optionalBool) String() string {
	if !o.set {
		return ""
	}
	return strconv.FormatBool(o.value)
}

func (o *optionalBool) Set(text string) error {
	value, err := strconv.ParseBool(text)
	if err != nil {
		return fmt.Errorf("expected true or false, got %q", text)
	}
	o.set, o.value = true, value
	return nil
}

// IsBoolFlag tells the flag package this flag does not consume a value, so
// `--eco true` and `--eco=true` both work.
func (o *optionalBool) IsBoolFlag() bool { return true }

func (o optionalBool) pointer() *bool {
	if !o.set {
		return nil
	}
	value := o.value
	return &value
}

// runSet applies a sparse set of state changes to one configured device.
func runSet(args []string) error {
	flags := flag.NewFlagSet("set", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	timeout := flags.Duration("timeout", 30*time.Second, "operation timeout")
	confirm := flags.Bool("confirm", false, "required confirmation for a physical write")
	mode := flags.String("mode", "", "operating mode: "+strings.Join(controller.Modes(), ", "))
	fan := flags.String("fan", "", "fan speed: "+strings.Join(controller.FanSpeeds(), ", "))
	temp := flags.Float64("temp", 0, "target temperature in °C (0.5 steps)")
	var power, swingV, swingH, eco, turbo, sleepMode, display optionalBool
	for _, entry := range []struct {
		name  string
		usage string
		value *optionalBool
	}{
		{"power", "turn the unit on or off", &power},
		{"swing-v", "vertical louver swing", &swingV},
		{"swing-h", "horizontal louver swing", &swingH},
		{"eco", "eco mode", &eco},
		{"turbo", "turbo mode", &turbo},
		{"sleep", "sleep mode", &sleepMode},
		{"display", "indoor display", &display},
	} {
		flags.Var(entry.value, entry.name, entry.usage+" (true/false)")
	}
	selectors, flagArgs, err := splitArgs(flags, args, false)
	if err != nil {
		return fmt.Errorf("set: %w", err)
	}
	if len(selectors) != 1 {
		return errors.New("set requires exactly one device selector")
	}
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if !*confirm {
		return errors.New("refusing physical write: pass -confirm")
	}

	patch := controller.Patch{
		Mode:     *mode,
		FanSpeed: *fan,
		Power:    power.pointer(),
		SwingV:   swingV.pointer(),
		SwingH:   swingH.pointer(),
		Eco:      eco.pointer(),
		Turbo:    turbo.pointer(),
		Sleep:    sleepMode.pointer(),
		Display:  display.pointer(),
	}
	if flags.Lookup("temp") != nil && isFlagPassed(flagArgs, "temp") {
		value := *temp
		patch.TargetTemp = &value
	}
	if patch.Empty() {
		return errors.New("nothing to change: pass at least one of --power, --mode, --fan, --temp, --eco, --turbo, --sleep, --display, --swing-v, --swing-h")
	}

	service := mcpserver.NewService()
	service.ConfigPath = *path
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := service.Apply(ctx, selectors[0], patch, true)
	if err != nil {
		return err
	}
	fmt.Printf("%s: applied %s\n", result.Device.Name, strings.Join(patch.FieldNames(), ", "))
	printStatus(result)
	return nil
}

// isFlagPassed reports whether the user actually supplied a flag, so that a
// zero value can be distinguished from "not requested".
func isFlagPassed(args []string, name string) bool {
	for _, arg := range args {
		if arg == "-"+name || arg == "--"+name ||
			strings.HasPrefix(arg, "-"+name+"=") || strings.HasPrefix(arg, "--"+name+"=") {
			return true
		}
	}
	return false
}

func runCapabilities(args []string) error {
	flags := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	timeout := flags.Duration("timeout", 30*time.Second, "per-device operation timeout")
	asJSON := flags.Bool("json", false, "print JSON")
	selectors, flagArgs, _ := splitArgs(flags, args, true)
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}

	file, err := config.Load(*path)
	if err != nil {
		return err
	}
	if len(selectors) == 0 {
		for _, device := range file.PublicDevices() {
			selectors = append(selectors, device.Name)
		}
	}
	service := mcpserver.NewService()
	service.ConfigPath = *path
	results := make([]mcpserver.CapabilityResult, 0, len(selectors))
	for _, selector := range selectors {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		result, err := service.Capabilities(ctx, selector)
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
		fmt.Printf("%s (%s): reported=%t custom_fan=%t humidity=%t swing_angle=%t temp_range=%s\n",
			result.Device.Name, result.Device.IP, result.Capabilities.Reported,
			result.Capabilities.CustomFanSpeed, result.Capabilities.HumidityControl,
			result.Capabilities.SwingAngle, describeRange(result.Capabilities))
	}
	return nil
}

func describeRange(c controller.Capabilities) string {
	min, max := c.MinCoolTemp, c.MaxCoolTemp
	if min == 0 || max == 0 {
		min, max = c.MinHeatTemp, c.MaxHeatTemp
	}
	if min == 0 || max == 0 {
		return "not reported (using 17-30°C default)"
	}
	return fmt.Sprintf("%.1f-%.1f°C", min, max)
}

// runEnergy samples a device's power draw. Sampling matters: an instantaneous
// reading is not enough to tell a ramping compressor from a bare fan.
func runEnergy(args []string) error {
	flags := flag.NewFlagSet("energy", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	timeout := flags.Duration("timeout", 30*time.Second, "per-read timeout")
	samples := flags.Int("samples", 1, "number of samples to take")
	interval := flags.Duration("interval", 5*time.Second, "delay between samples")
	asJSON := flags.Bool("json", false, "print JSON")
	selectors, flagArgs, _ := splitArgs(flags, args, true)
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if *samples < 1 {
		return errors.New("samples must be at least 1")
	}

	file, err := config.Load(*path)
	if err != nil {
		return err
	}
	if len(selectors) == 0 {
		for _, device := range file.PublicDevices() {
			selectors = append(selectors, device.Name)
		}
	}
	service := mcpserver.NewService()
	service.ConfigPath = *path

	// Energy sample loop. The unit's realtime power field is zero on this
	// hardware, so the useful number is the average power derived from the
	// lifetime-consumption delta between samples.
	type reading struct {
		Sample    int                    `json:"sample"`
		Result    mcpserver.EnergyResult `json:"result"`
		DeltaKWh  *float64               `json:"delta_kwh,omitempty"`
		AverageKW *float64               `json:"average_kw,omitempty"`
	}
	readings := make([]reading, 0, len(selectors)**samples)
	previousKWh := map[string]float64{}
	previousAt := map[string]time.Time{}
	for _, selector := range selectors {
		for sample := 1; sample <= *samples; sample++ {
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			result, err := service.Energy(ctx, selector)
			cancel()
			if err != nil {
				return err
			}
			entry := reading{Sample: sample, Result: result}
			if previous, ok := previousKWh[result.Device.ID]; ok {
				elapsed := time.Since(previousAt[result.Device.ID])
				delta := result.Energy.TotalKWh - previous
				entry.DeltaKWh = &delta
				if elapsed > 0 {
					average := delta / elapsed.Hours()
					entry.AverageKW = &average
				}
			}
			previousKWh[result.Device.ID] = result.Energy.TotalKWh
			sampledAt := time.Now()
			previousAt[result.Device.ID] = sampledAt
			readings = append(readings, entry)
			if *asJSON {
				continue
			}
			if entry.AverageKW != nil {
				fmt.Printf("%-14s %2d/%d  avg=%6.3f kW  delta=%+6.3f kWh  (unit reported %6.3f kW)\n",
					result.Device.Name, sample, *samples, *entry.AverageKW, *entry.DeltaKWh,
					result.Energy.RealtimeKW)
			} else {
				fmt.Printf("%-14s %2d/%d  baseline  total=%8.3f kWh  (unit reported %6.3f kW)\n",
					result.Device.Name, sample, *samples,
					result.Energy.TotalKWh, result.Energy.RealtimeKW)
			}
			if sample < *samples {
				time.Sleep(*interval)
			}
		}
	}
	if *asJSON {
		return printJSON(readings)
	}
	return nil
}

func runMCP(args []string) error {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", config.DefaultPath(), "device inventory path")
	useHTTP := flags.Bool("http", false, "serve Streamable HTTP instead of stdio")
	httpAddr := flags.String("http-addr", "127.0.0.1:8765", "HTTP listen address (loopback unless --http-allow-remote)")
	httpTokenFile := flags.String("http-token-file", "", "file holding the bearer token (default: alongside the inventory)")
	httpStateless := flags.Bool("http-stateless", false, "run without server-side sessions")
	allowRemote := flags.Bool("http-allow-remote", false, "permit a non-loopback listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}

	service := mcpserver.NewService()
	service.ConfigPath = *path

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !*useHTTP {
		return service.Run(ctx)
	}

	token, tokenFile, err := mcpserver.LoadOrCreateToken(os.Getenv("MIDEA_CONTROL_MCP_TOKEN"), *httpTokenFile)
	if err != nil {
		return err
	}
	server, err := mcpserver.NewHTTPServer(service, mcpserver.HTTPConfig{
		Addr:        *httpAddr,
		Token:       token,
		Stateless:   *httpStateless,
		AllowRemote: *allowRemote,
	})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *httpAddr, err)
	}
	if tokenFile != "" {
		fmt.Fprintf(os.Stderr, "mcp http listening on %s (bearer token in %s, mode 0600)\n",
			listener.Addr(), tokenFile)
	} else {
		fmt.Fprintf(os.Stderr, "mcp http listening on %s (bearer token from MIDEA_CONTROL_MCP_TOKEN)\n",
			listener.Addr())
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
  midea-control capabilities [SELECTOR ...] [--config PATH] [--json]
  midea-control verify     [--config PATH] [--json]
  midea-control set        SELECTOR --confirm [--power BOOL] [--mode MODE]
                           [--fan SPEED] [--temp C] [--eco BOOL] [--turbo BOOL]
                           [--sleep BOOL] [--display BOOL]
                           [--swing-v BOOL] [--swing-h BOOL]
  midea-control bootstrap  [--account EMAIL] [--device SELECTOR] [--dry-run]
  midea-control on         SELECTOR --confirm [--config PATH]
  midea-control off        SELECTOR --confirm [--config PATH]
  midea-control mcp        [--config PATH]

Selectors are exact configured names, IP addresses, or numeric device IDs.
Flags may appear before or after selectors; use -- before a selector that
starts with a dash. The mcp command speaks the Model Context Protocol over
stdio; no HTTP listener is opened. The bootstrap command needs account
credentials and is never reachable through MCP. Tokens and keys are read only
from the mode-0600 inventory file and are never printed.
`, version.Value)
}
