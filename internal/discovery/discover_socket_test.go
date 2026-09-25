package discovery

import (
	"context"
	"net"
	"testing"
	"time"
)

// fakeResponder answers the first discovery request on a loopback port with a
// synthetic V3 reply, so the real socket path is exercised without a LAN.
func fakeResponder(t *testing.T, reply []byte) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("cannot bind a loopback UDP socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 2048)
		for {
			_, addr, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP(reply, addr)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

func TestDiscoverFindsAndDecodesARealReply(t *testing.T) {
	// A wrapped V3 packet, as a unit would send it.
	packet := makeResponse(t, 3)
	port := fakeResponder(t, packet)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devices, err := Discover(ctx, Options{
		Target:  "127.0.0.1",
		Ports:   []int{port},
		Timeout: 500 * time.Millisecond,
		Packets: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("found %d devices, want 1", len(devices))
	}
	device := devices[0]
	if device.ID != 0x665544332211 {
		t.Errorf("id = %#x, want 0x665544332211", device.ID)
	}
	if device.Port != 6444 || device.Type != 0xac || device.Protocol != 3 {
		t.Errorf("device = %+v", device)
	}
	if device.Model != "00000Q18" {
		t.Errorf("model = %q", device.Model)
	}
	if device.IP != "127.0.0.1" {
		t.Errorf("ip = %q, want the responder address", device.IP)
	}
}

func TestDiscoverDecodesAV2Reply(t *testing.T) {
	port := fakeResponder(t, makeResponse(t, 2))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devices, err := Discover(ctx, Options{
		Target: "127.0.0.1", Ports: []int{port}, Timeout: 500 * time.Millisecond, Packets: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Protocol != 2 {
		t.Fatalf("devices = %+v", devices)
	}
}

func TestDiscoverDeduplicatesRepeatedReplies(t *testing.T) {
	port := fakeResponder(t, makeResponse(t, 3))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devices, err := Discover(ctx, Options{
		Target: "127.0.0.1", Ports: []int{port}, Timeout: 800 * time.Millisecond, Packets: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("found %d devices; a repeated reply must not be counted twice", len(devices))
	}
}

func TestDiscoverIgnoresUnrelatedTraffic(t *testing.T) {
	port := fakeResponder(t, []byte("not a midea packet at all, but long enough to pass the size check......"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devices, err := Discover(ctx, Options{
		Target: "127.0.0.1", Ports: []int{port}, Timeout: 500 * time.Millisecond, Packets: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 0 {
		t.Fatalf("garbage was decoded as a device: %+v", devices)
	}
}

func TestDiscoverReturnsNothingWhenNobodyAnswers(t *testing.T) {
	// Port 1 on loopback has no responder.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devices, err := Discover(ctx, Options{
		Target: "127.0.0.1", Ports: []int{1}, Timeout: 300 * time.Millisecond, Packets: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 0 {
		t.Fatalf("found %+v", devices)
	}
}

func TestDiscoverRefusesAPublicTarget(t *testing.T) {
	// The scan must not become a model-controlled egress primitive, so a
	// non-private target is rejected before any packet is sent.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Discover(ctx, Options{
		Target: "8.8.8.8", Timeout: time.Second, Packets: 1,
	}); err == nil {
		t.Fatal("Discover accepted a public target")
	}
}

func TestDiscoverHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, Options{Timeout: time.Second, Packets: 1}); err == nil {
		t.Fatal("Discover ignored a cancelled context")
	}
}

func TestDefaultPortsAreTheMideaPorts(t *testing.T) {
	got := (Options{}).ports()
	if len(got) != 2 || got[0] != 6445 || got[1] != 20086 {
		t.Fatalf("default ports = %v, want [6445 20086]", got)
	}
	if got := (Options{Ports: []int{1234}}).ports(); len(got) != 1 || got[0] != 1234 {
		t.Fatalf("override ports = %v", got)
	}
}

func TestTargetsForKeepsLoopbackTargets(t *testing.T) {
	targets, err := targetsFor("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != "127.0.0.1" {
		t.Fatalf("targets = %v", targets)
	}
	if ip := net.ParseIP(targets[0]); ip == nil || ip.To4() == nil {
		t.Fatalf("target %q is not a bare IPv4 address", targets[0])
	}
}
