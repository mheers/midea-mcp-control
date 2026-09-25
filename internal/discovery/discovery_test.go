package discovery

import (
	"crypto/aes"
	"encoding/binary"
	"testing"
)

func encryptForTest(t *testing.T, key, plain []byte) []byte {
	t.Helper()
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte(nil), plain...), bytesRepeat(byte(padding), padding)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]byte, len(padded))
	for offset := 0; offset < len(padded); offset += aes.BlockSize {
		block.Encrypt(result[offset:offset+aes.BlockSize], padded[offset:offset+aes.BlockSize])
	}
	return result
}

func bytesRepeat(value byte, count int) []byte {
	result := make([]byte, count)
	for i := range result {
		result[i] = value
	}
	return result
}

func makeResponse(t *testing.T, protocol int) []byte {
	t.Helper()
	ssid := "net_ac_A292"
	reply := make([]byte, minReplySize+len(ssid))
	binary.LittleEndian.PutUint32(reply[4:8], 6444)
	copy(reply[8:40], "000000P0000000Q18TEST000000000000")
	copy(reply[17:25], "00000Q18")
	reply[40] = byte(len(ssid))
	copy(reply[minReplySize:], ssid)
	encrypted := encryptForTest(t, discoveryAESKey, reply)
	packet := make([]byte, 40+len(encrypted)+16)
	packet[0], packet[1] = 0x5a, 0x5a
	for i, value := range []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66} {
		packet[20+i] = value
	}
	copy(packet[40:], encrypted)
	if protocol == 2 {
		return packet
	}
	outer := append(make([]byte, 8), packet...)
	outer[0], outer[1] = 0x83, 0x70
	return append(outer, make([]byte, 16)...)
}

func TestParseResponseV2(t *testing.T) {
	device, ok, err := ParseResponse(makeResponse(t, 2), "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ParseResponse did not recognize V2 response")
	}
	if device.ID != 0x665544332211 || device.Port != 6444 || device.Type != 0xac || device.Model != "00000Q18" {
		t.Fatalf("device = %+v", device)
	}
}

func TestParseResponseV3(t *testing.T) {
	device, ok, err := ParseResponse(makeResponse(t, 3), "192.0.2.11")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ParseResponse did not recognize V3 response")
	}
	if device.Protocol != 3 || device.SSID != "net_ac_A292" || device.IP != "192.0.2.11" {
		t.Fatalf("device = %+v", device)
	}
}

func TestTargetsForRejectsPublicAndMalformedTargets(t *testing.T) {
	if _, err := targetsFor("8.8.8.8"); err == nil {
		t.Fatal("targetsFor accepted a public target")
	}
	if _, err := targetsFor("not-an-ip"); err == nil {
		t.Fatal("targetsFor accepted a malformed target")
	}
}

func TestDecryptRejectsInvalidPadding(t *testing.T) {
	if _, err := decryptECB(discoveryAESKey, make([]byte, aes.BlockSize)); err == nil {
		t.Fatal("decryptECB accepted invalid padding")
	}
}

func TestParseResponseRejectsMalformedPackets(t *testing.T) {
	if _, ok, err := ParseResponse([]byte{0x83, 0x70}, "192.0.2.1"); ok || err != nil {
		t.Fatalf("short packet = ok:%v err:%v, want ignored", ok, err)
	}
	if _, ok, err := ParseResponse(make([]byte, 128), "192.0.2.1"); ok || err != nil {
		t.Fatalf("unknown packet = ok:%v err:%v, want ignored", ok, err)
	}
}
