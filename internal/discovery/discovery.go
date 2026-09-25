// Package discovery finds Midea Wi-Fi appliances on the local broadcast domain.
package discovery

import (
	"context"
	"crypto/aes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout  = 5 * time.Second
	defaultPackets  = 3
	minResponseSize = 104
	minReplySize    = 41
)

var discoveryMessage = []byte{
	0x5a, 0x5a, 0x01, 0x11, 0x48, 0x00, 0x92, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x7f, 0x75, 0xbd, 0x6b, 0x3e, 0x4f, 0x8b, 0x76,
	0x2e, 0x84, 0x9c, 0x6e, 0x57, 0x8d, 0x65, 0x90,
	0x03, 0x6e, 0x9d, 0x43, 0x42, 0xa5, 0x0f, 0x1f,
	0x56, 0x9e, 0xb8, 0xec, 0x91, 0x8e, 0x92, 0xe5,
}

var discoveryAESKey = []byte{
	0x6a, 0x92, 0xef, 0x40, 0x6b, 0xad, 0x2f, 0x03,
	0x59, 0xba, 0xad, 0x99, 0x41, 0x71, 0xea, 0x6d,
}

// Options controls a discovery scan.
type Options struct {
	Target  string
	Timeout time.Duration
	Packets int
}

// Device is the non-secret information returned by a LAN discovery.
type Device struct {
	ID       uint64 `json:"id"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	Type     int    `json:"type"`
	Model    string `json:"model"`
	Protocol int    `json:"protocol"`
	SSID     string `json:"ssid,omitempty"`
}

// Discover broadcasts Midea's discovery request and returns unique devices.
func Discover(ctx context.Context, options Options) ([]Device, error) {
	if options.Timeout <= 0 {
		options.Timeout = defaultTimeout
	}
	if options.Packets <= 0 {
		options.Packets = defaultPackets
	}

	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("open discovery socket: %w", err)
	}
	defer conn.Close()
	if err := setBroadcast(conn); err != nil {
		return nil, err
	}

	targets, err := targetsFor(options.Target)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(target, "6445"))
		if err != nil {
			return nil, fmt.Errorf("resolve discovery target %s: %w", target, err)
		}
		if err := sendPackets(conn, addr, options.Packets); err != nil {
			return nil, err
		}
		addr, err = net.ResolveUDPAddr("udp4", net.JoinHostPort(target, "20086"))
		if err != nil {
			return nil, fmt.Errorf("resolve discovery target %s: %w", target, err)
		}
		if err := sendPackets(conn, addr, options.Packets); err != nil {
			return nil, err
		}
	}

	deadline := time.Now().Add(options.Timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	found := make(map[uint64]Device)
	buffer := make([]byte, 2048)
	for {
		if err := ctx.Err(); err != nil {
			return devices(found), err
		}
		if time.Now().After(deadline) {
			return devices(found), nil
		}
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, addr, err := conn.ReadFrom(buffer)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return devices(found), ctx.Err()
			}
			return devices(found), fmt.Errorf("read discovery response: %w", err)
		}
		device, ok, err := ParseResponse(buffer[:n], addrIP(addr))
		if err != nil || !ok {
			continue
		}
		found[device.ID] = device
	}
}

// ParseResponse decodes one Midea V2 or V3 discovery response.
func ParseResponse(data []byte, sourceIP string) (Device, bool, error) {
	if len(data) < minResponseSize {
		return Device{}, false, nil
	}

	protocol := 0
	packet := data
	switch {
	case data[0] == 0x5a && data[1] == 0x5a:
		protocol = 2
	case data[0] == 0x83 && data[1] == 0x70 && len(data) > 24 && data[8] == 0x5a && data[9] == 0x5a:
		protocol = 3
		packet = data[8 : len(data)-16]
	default:
		return Device{}, false, nil
	}
	if len(packet) < 56 {
		return Device{}, false, errors.New("discovery packet is too short")
	}

	reply, err := decryptECB(discoveryAESKey, packet[40:len(packet)-16])
	if err != nil {
		return Device{}, false, err
	}
	if len(reply) < minReplySize {
		return Device{}, false, errors.New("discovery reply is too short")
	}
	ssidLength := int(reply[40])
	if ssidLength <= 0 || minReplySize+ssidLength > len(reply) {
		return Device{}, false, errors.New("discovery reply has invalid SSID length")
	}
	ssid := strings.TrimRight(string(reply[minReplySize:minReplySize+ssidLength]), "\x00")
	parts := strings.Split(ssid, "_")
	if len(parts) < 2 {
		return Device{}, false, errors.New("discovery reply has invalid SSID")
	}
	deviceType, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return Device{}, false, fmt.Errorf("parse device type: %w", err)
	}
	var id uint64
	for i, value := range packet[20:26] {
		id |= uint64(value) << (8 * i)
	}
	if id == 0 {
		return Device{}, false, errors.New("discovery reply has zero device id")
	}
	port := int(binary.LittleEndian.Uint32(reply[4:8]))
	if port == 0 {
		port = 6444
	}
	model := strings.TrimRight(string(reply[17:25]), "\x00")
	return Device{
		ID:       id,
		IP:       sourceIP,
		Port:     port,
		Type:     int(deviceType),
		Model:    model,
		Protocol: protocol,
		SSID:     ssid,
	}, true, nil
}

func decryptECB(key, data []byte) ([]byte, error) {
	if len(data)%aes.BlockSize != 0 {
		return nil, errors.New("encrypted discovery reply is not block aligned")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create discovery cipher: %w", err)
	}
	plain := make([]byte, len(data))
	for offset := 0; offset < len(data); offset += aes.BlockSize {
		block.Decrypt(plain[offset:offset+aes.BlockSize], data[offset:offset+aes.BlockSize])
	}
	if len(plain) == 0 {
		return nil, errors.New("encrypted discovery reply is empty")
	}
	padding := int(plain[len(plain)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(plain) {
		return nil, errors.New("invalid discovery reply padding")
	}
	for _, value := range plain[len(plain)-padding:] {
		if int(value) != padding {
			return nil, errors.New("invalid discovery reply padding")
		}
	}
	return plain[:len(plain)-padding], nil
}

func sendPackets(conn net.PacketConn, addr *net.UDPAddr, count int) error {
	for i := 0; i < count; i++ {
		if _, err := conn.WriteTo(discoveryMessage, addr); err != nil {
			return fmt.Errorf("send discovery to %s: %w", addr, err)
		}
	}
	return nil
}

func targetsFor(target string) ([]string, error) {
	if target != "" && target != "255.255.255.255" {
		ip := net.ParseIP(target)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("discovery target must be an IPv4 address: %q", target)
		}
		ip = ip.To4()
		if !ip.IsPrivate() && !ip.IsLoopback() {
			return nil, fmt.Errorf("discovery target must be private or loopback: %q", target)
		}
		return []string{ip.String()}, nil
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("enumerate network interfaces: %w", err)
	}
	seen := make(map[string]struct{})
	var targets []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ipNet, ok := address.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			mask := ipNet.Mask
			if len(mask) == net.IPv6len {
				mask = mask[12:]
			}
			if len(mask) != net.IPv4len {
				continue
			}
			broadcast := make(net.IP, net.IPv4len)
			copy(broadcast, ip)
			for i := range broadcast {
				broadcast[i] |= ^mask[i]
			}
			value := broadcast.String()
			if _, ok := seen[value]; !ok {
				seen[value] = struct{}{}
				targets = append(targets, value)
			}
		}
	}
	if len(targets) == 0 {
		return []string{"255.255.255.255"}, nil
	}
	return targets, nil
}

func addrIP(addr net.Addr) string {
	switch value := addr.(type) {
	case *net.UDPAddr:
		return value.IP.String()
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err == nil {
			return host
		}
		return addr.String()
	}
}

func devices(found map[uint64]Device) []Device {
	result := make([]Device, 0, len(found))
	for _, device := range found {
		result = append(result, device)
	}
	return result
}
