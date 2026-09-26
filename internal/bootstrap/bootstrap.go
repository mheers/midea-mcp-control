// Package bootstrap obtains V3 LAN credentials from the Midea cloud and
// verifies them against the devices on the local network.
//
// It is a one-time setup path: normal operation never needs the cloud or the
// account.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mheers/midea-mcp-control/internal/cloud"
	"github.com/mheers/midea-mcp-control/internal/config"
	"github.com/mheers/midea-mcp-control/internal/controller"
	"github.com/mheers/midea-mcp-control/internal/discovery"
)

const (
	defaultDiscoveryTimeout = 5 * time.Second
	defaultOperationTimeout = 20 * time.Second
)

// Runner performs a bootstrap. Use New to construct one.
type Runner struct {
	// ConfigPath is the inventory to create or update.
	ConfigPath string
	// CloudName selects the cloud identity; only "SmartHome" is implemented.
	CloudName string
	// Account and Password authenticate to the cloud. They are never stored
	// and never logged.
	Account  string
	Password string
	// Selector optionally limits the run to one device name, IP or id.
	Selector string
	// DiscoveryTimeout bounds the LAN discovery scan.
	DiscoveryTimeout time.Duration
	// OperationTimeout bounds each cloud and device operation.
	OperationTimeout time.Duration
	// DryRun performs every check but does not write the inventory.
	DryRun bool
	// Out receives human-readable progress. It never receives secrets.
	Out io.Writer

	// Seams, overridden in tests.
	discover func(context.Context, discovery.Options) ([]discovery.Device, error)
	newCloud func(cloudName, account, password string) (cloudClient, error)
	control  controlClient
}

// cloudClient is the subset of the cloud client the runner needs.
type cloudClient interface {
	Login(context.Context) error
	ListAppliances(context.Context) (map[uint64]cloud.Appliance, error)
	GetToken(context.Context, uint64, cloud.UDPPIDMethod) (cloud.Credential, bool, error)
}

// controlClient is the subset of the controller the runner needs.
type controlClient interface {
	Poll(context.Context, config.Device) (controller.State, error)
}

// Result is the outcome for a single device.
type Result struct {
	Device   config.PublicDevice
	Skipped  bool
	Verified bool
	Changed  bool
	Err      error
}

// New returns a Runner with production dependencies.
func New() *Runner {
	return &Runner{
		CloudName:        "SmartHome",
		DiscoveryTimeout: defaultDiscoveryTimeout,
		OperationTimeout: defaultOperationTimeout,
		Out:              io.Discard,
		discover:         discovery.Discover,
		newCloud:         defaultCloudFactory,
		control:          controller.New(),
	}
}

// defaultCloudFactory adapts cloud.New to the runner's seam signature.
func defaultCloudFactory(cloudName, account, password string) (cloudClient, error) {
	return cloud.New(cloudName, account, password)
}

// Run discovers devices, fetches a credential for each, verifies it over the
// LAN, and merges the results into the inventory unless DryRun is set.
//
// Every device is attempted: one failure never hides the others.
func (r *Runner) Run(ctx context.Context) ([]Result, error) {
	r.applyDefaults()
	if r.Account == "" || r.Password == "" {
		return nil, errors.New("bootstrap: an account and password are required")
	}

	existing := config.File{Format: 1}
	loaded, err := config.Load(r.ConfigPath)
	switch {
	case err == nil:
		existing = loaded
	case errors.Is(err, fs.ErrNotExist):
		// First run: start from an empty inventory.
	default:
		// A malformed or too-permissive existing file must not be silently
		// replaced, because that could destroy working credentials.
		return nil, fmt.Errorf("bootstrap: existing inventory is unusable: %w", err)
	}

	found, err := r.discoverDevices(ctx)
	if err != nil {
		return nil, err
	}

	client, err := r.newCloud(r.CloudName, r.Account, r.Password)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	operationCtx, cancel := context.WithTimeout(ctx, r.OperationTimeout)
	err = client.Login(operationCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("bootstrap: cloud login failed: %w", err)
	}
	fmt.Fprintln(r.Out, "cloud login succeeded")

	operationCtx, cancel = context.WithTimeout(ctx, r.OperationTimeout)
	appliances, err := client.ListAppliances(operationCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("bootstrap: cloud inventory failed: %w", err)
	}

	results := make([]Result, 0, len(found))
	updates := make(map[string]config.Device, len(found))
	for _, local := range found {
		result := Result{Device: local.public()}
		if r.Selector != "" && !local.matches(r.Selector, appliances) {
			result.Skipped = true
			results = append(results, result)
			continue
		}
		device, err := r.fetchCredential(ctx, client, local, appliances)
		if err != nil {
			result.Err = err
			fmt.Fprintf(r.Out, "  %s: %v\n", local.describe(), err)
			results = append(results, result)
			continue
		}
		result.Verified = true
		result.Device = device.Public()
		result.Changed = !sameCredential(existing, device)
		updates[device.ID] = device
		if result.Changed {
			fmt.Fprintf(r.Out, "  %s: credential fetched, verified over LAN (rotated)\n", local.describe())
		} else {
			fmt.Fprintf(r.Out, "  %s: credential verified over LAN (unchanged)\n", local.describe())
		}
		results = append(results, result)
	}

	verified := 0
	for _, result := range results {
		if result.Verified {
			verified++
		}
	}
	if verified == 0 {
		return results, errors.New("bootstrap: no device credentials could be established")
	}
	if r.DryRun {
		fmt.Fprintf(r.Out, "dry run: %d credential(s) verified, inventory not written\n", verified)
		return results, nil
	}

	if err := config.Save(r.ConfigPath, merge(existing, updates)); err != nil {
		return results, fmt.Errorf("bootstrap: save inventory: %w", err)
	}
	fmt.Fprintf(r.Out, "wrote %d verified credential(s) to %s\n", verified, r.ConfigPath)
	return results, nil
}

func (r *Runner) applyDefaults() {
	if r.ConfigPath == "" {
		r.ConfigPath = config.DefaultPath()
	}
	if r.CloudName == "" {
		r.CloudName = "SmartHome"
	}
	if r.DiscoveryTimeout <= 0 {
		r.DiscoveryTimeout = defaultDiscoveryTimeout
	}
	if r.OperationTimeout <= 0 {
		r.OperationTimeout = defaultOperationTimeout
	}
	if r.Out == nil {
		r.Out = io.Discard
	}
	if r.discover == nil {
		r.discover = discovery.Discover
	}
	if r.newCloud == nil {
		r.newCloud = defaultCloudFactory
	}
	if r.control == nil {
		r.control = controller.New()
	}
}

// fetchCredential gets a working token/key for one device and verifies it
// against the unit before returning it.
func (r *Runner) fetchCredential(
	ctx context.Context,
	client cloudClient,
	local discoveredDevice,
	appliances map[uint64]cloud.Appliance,
) (config.Device, error) {
	for _, method := range []cloud.UDPPIDMethod{cloud.UDPPIDBig, cloud.UDPPIDLittle} {
		operationCtx, cancel := context.WithTimeout(ctx, r.OperationTimeout)
		credential, found, err := client.GetToken(operationCtx, local.ID, method)
		cancel()
		if err != nil {
			return config.Device{}, fmt.Errorf("cloud token request failed: %w", err)
		}
		if !found {
			continue
		}

		candidate := local.toConfig(local.name(appliances), credential, method)
		operationCtx, cancel = context.WithTimeout(ctx, r.OperationTimeout)
		_, err = r.control.Poll(operationCtx, candidate)
		cancel()
		if err != nil {
			// This encoding is not the one the unit accepts; try the next.
			continue
		}
		candidate.LastVerified = time.Now().UTC()
		return candidate, nil
	}
	return config.Device{}, errors.New("no cloud token verified against the device")
}

func (r *Runner) discoverDevices(ctx context.Context) ([]discoveredDevice, error) {
	scanCtx, cancel := context.WithTimeout(ctx, r.DiscoveryTimeout+time.Second)
	defer cancel()
	found, err := r.discover(scanCtx, discovery.Options{Timeout: r.DiscoveryTimeout})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: LAN discovery failed: %w", err)
	}
	devices := make([]discoveredDevice, 0, len(found))
	for _, item := range found {
		if item.Type != 0xac || item.Protocol != 3 {
			// Only V3 air conditioners have cloud-issued LAN credentials.
			continue
		}
		devices = append(devices, discoveredDevice{ID: item.ID, item: item})
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].item.IP < devices[j].item.IP })
	return devices, nil
}

// discoveredDevice pairs LAN discovery data with the numeric device id.
type discoveredDevice struct {
	ID   uint64
	item discovery.Device
}

func (d discoveredDevice) public() config.PublicDevice {
	return config.PublicDevice{
		ID:       strconv.FormatUint(d.ID, 10),
		Name:     d.item.IP,
		IP:       d.item.IP,
		Port:     d.item.Port,
		Type:     d.item.Type,
		Model:    d.item.Model,
		Protocol: d.item.Protocol,
	}
}

func (d discoveredDevice) describe() string {
	return fmt.Sprintf("%s (id %d)", d.item.IP, d.ID)
}

func (d discoveredDevice) name(appliances map[uint64]cloud.Appliance) string {
	if name := appliances[d.ID].Name; name != "" {
		return name
	}
	return d.item.IP
}

func (d discoveredDevice) matches(selector string, appliances map[uint64]cloud.Appliance) bool {
	selector = strings.ToLower(selector)
	return selector == strings.ToLower(d.item.IP) ||
		selector == strconv.FormatUint(d.ID, 10) ||
		selector == strings.ToLower(appliances[d.ID].Name)
}

func (d discoveredDevice) toConfig(name string, credential cloud.Credential, method cloud.UDPPIDMethod) config.Device {
	return config.Device{
		ID:               strconv.FormatUint(d.ID, 10),
		Name:             name,
		IP:               d.item.IP,
		Port:             d.item.Port,
		Type:             d.item.Type,
		Model:            d.item.Model,
		Protocol:         d.item.Protocol,
		Token:            credential.Token,
		Key:              credential.Key,
		CredentialMethod: int(method),
		ObtainedAt:       time.Now().UTC(),
	}
}

// sameCredential reports whether the stored credential already matches the
// freshly issued one. The cloud mints a new pair per request, so this is
// usually false for an already-configured device.
func sameCredential(existing config.File, candidate config.Device) bool {
	for _, device := range existing.Devices {
		if device.ID == candidate.ID {
			return device.Token == candidate.Token && device.Key == candidate.Key
		}
	}
	return true // new device
}

// merge folds freshly verified devices into the existing inventory, keeping
// devices that were not part of this run and keeping names unique.
func merge(existing config.File, updates map[string]config.Device) config.File {
	merged := make(map[string]config.Device, len(existing.Devices)+len(updates))
	for _, device := range existing.Devices {
		merged[device.ID] = device
	}
	for id, device := range updates {
		merged[id] = device
	}

	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	result := config.File{Format: 1, Devices: make([]config.Device, 0, len(ids))}
	used := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		device := merged[id]
		name := strings.TrimSpace(device.Name)
		if name == "" {
			name = device.IP
		}
		base := name
		for suffix := 2; ; suffix++ {
			if _, taken := used[strings.ToLower(name)]; !taken {
				break
			}
			name = fmt.Sprintf("%s (%d)", base, suffix)
		}
		device.Name = name
		used[strings.ToLower(name)] = struct{}{}
		result.Devices = append(result.Devices, device)
	}
	return result
}
