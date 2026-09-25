package cloud

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The expected values below were produced by the reference implementation of
// the SmartHome cloud handshake (midea-local 12.1.0). They pin this port to the
// documented vendor behaviour; the reference is not used at runtime.
func TestDeviceID(t *testing.T) {
	cases := map[string]string{
		"alice@example.com": "449e1521cf154310",
		"":                  "a1e03a4721197ed7",
	}
	for input, want := range cases {
		if got := deviceID(input); got != want {
			t.Errorf("deviceID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPasswordHashes(t *testing.T) {
	sec := newSecurity(
		"ac21b9f9cbfe4ca5a88562ef25e2b768",
		"meicloud",
		"PROD_VnoClJI9aikS8dyy",
	)
	const (
		loginID  = "0123456789abcdef"
		password = "s3cret-pass"
	)
	if got, want := sec.encryptPassword(loginID, password),
		"90f02cffc9588773a0f9ec3ac51e4314b96ef7dce7d4a9300dec8f6b597e57ac"; got != want {
		t.Errorf("encryptPassword = %q, want %q", got, want)
	}
	if got, want := sec.encryptIAMPassword(loginID, password),
		"eadf8e872ac904e2cc12f3b9e08f50cad9ad49e56393a82131fad9270a13ab8b"; got != want {
		t.Errorf("encryptIAMPassword = %q, want %q", got, want)
	}
}

func TestSign(t *testing.T) {
	sec := newSecurity(
		"ac21b9f9cbfe4ca5a88562ef25e2b768",
		"meicloud",
		"PROD_VnoClJI9aikS8dyy",
	)
	got := sec.sign(`{"a":1,"b":"two"}`, "1700000000")
	const want = "b6a80d7cdf43a7ad3769965675f2da3a7e9c897f9aa949c9799052d4dfd3dacb"
	if got != want {
		t.Errorf("sign = %q, want %q", got, want)
	}
}

func TestUDPID(t *testing.T) {
	// A synthetic appliance id, so no real device identifier appears in the
	// repository. The expected values come from the reference implementation.
	const applianceID = 100000000000001
	cases := []struct {
		method UDPPIDMethod
		want   string
	}{
		{UDPPIDReversedBig, "7943167956c40050d78744b742b299c0"},
		{UDPPIDBig, "46d2f61bd1296aa5f513988f874f1441"},
		{UDPPIDLittle, "786b56584663758df81c59e425e658c5"},
	}
	for _, testCase := range cases {
		got, err := udpID(applianceID, testCase.method)
		if err != nil {
			t.Fatalf("udpID(method %d): %v", testCase.method, err)
		}
		if got != testCase.want {
			t.Errorf("udpID(method %d) = %q, want %q", testCase.method, got, testCase.want)
		}
	}
	if got, err := udpID(1, UDPPIDBig); err != nil || got != "13e129a6fd15446b720b556eb61c0c8b" {
		t.Errorf("udpID(1, big) = %q, %v", got, err)
	}
	if got, err := udpID(1<<48-1, UDPPIDBig); err != nil || got != "51162ae92fe9a326f235c3a5b198e213" {
		t.Errorf("udpID(2^48-1, big) = %q, %v", got, err)
	}
}

func TestUDPIDRejectsOversizeAndUnknown(t *testing.T) {
	if _, err := udpID(1<<48, UDPPIDBig); err == nil {
		t.Error("udpID accepted an id too large for the 6-byte encoding")
	}
	if _, err := udpID(1<<48, UDPPIDLittle); err == nil {
		t.Error("udpID accepted an id too large for the 6-byte encoding (little)")
	}
	if _, err := udpID(1, UDPPIDMethod(7)); err == nil {
		t.Error("udpID accepted an unknown method")
	}
}

// The decrypted session key and IV are used directly as AES material, so they
// must be valid key/IV lengths.
const (
	testSessionKey = "0123456789abcdef"
	testSessionIV  = "fedcba9876543210"
)

func TestSessionKeysRoundTrip(t *testing.T) {
	sec := newSecurity(
		"ac21b9f9cbfe4ca5a88562ef25e2b768",
		"meicloud",
		"PROD_VnoClJI9aikS8dyy",
	)
	digest := sha256.Sum256([]byte(sec.loginKey))
	hexDigest := hex.EncodeToString(digest[:])
	tempKey := []byte(hexDigest[:16])
	tempIV := []byte(hexDigest[16:32])

	// The decrypted session key and IV are used directly as AES material, so
	// they must be valid key/IV lengths.
	encryptedKey := encryptHex(t, testSessionKey, tempKey, tempIV)
	encryptedIV := encryptHex(t, testSessionIV, tempKey, tempIV)

	if err := sec.setSessionKeys(encryptedKey, encryptedIV); err != nil {
		t.Fatal(err)
	}
	if string(sec.aesKey) != testSessionKey {
		t.Errorf("session key = %q, want %q", sec.aesKey, testSessionKey)
	}
	if string(sec.aesIV) != testSessionIV {
		t.Errorf("session iv = %q, want %q", sec.aesIV, testSessionIV)
	}

	const plaintext = "SN00000P0000000Q18ABCDEFGH"
	got, err := sec.decrypt(encryptHex(t, plaintext, sec.aesKey, sec.aesIV))
	if err != nil {
		t.Fatal(err)
	}
	if got != plaintext {
		t.Errorf("decrypt = %q, want %q", got, plaintext)
	}
}

func TestDecryptWithoutSessionFails(t *testing.T) {
	sec := newSecurity("k", "i", "h")
	if _, err := sec.decrypt("aabbcc"); err == nil {
		t.Error("decrypt succeeded without session keys")
	}
}

func TestAESDecryptRejectsBadInput(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")
	if _, err := aesDecrypt("zz", key, iv); err == nil {
		t.Error("accepted non-hex input")
	}
	if _, err := aesDecrypt("aabb", key, iv); err == nil {
		t.Error("accepted misaligned ciphertext")
	}
	if _, err := aesDecrypt("", key, iv); err != nil {
		t.Errorf("empty input should be a no-op, got %v", err)
	}
	if _, err := aesDecrypt("aabbccddeeff00112233445566778899", key, []byte("0")); err == nil {
		t.Error("accepted a bad pad in ECB mode")
	}
}

// encryptHex mirrors aesDecrypt for round-trip tests.
func encryptHex(t *testing.T, plaintext string, key, iv []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append([]byte(plaintext), bytes.Repeat([]byte{byte(padding)}, padding)...)
	out := make([]byte, len(padded))
	if string(iv) == "0" || len(iv) == 0 {
		for offset := 0; offset < len(padded); offset += aes.BlockSize {
			block.Encrypt(out[offset:offset+aes.BlockSize], padded[offset:offset+aes.BlockSize])
		}
		return hex.EncodeToString(out)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return hex.EncodeToString(out)
}
