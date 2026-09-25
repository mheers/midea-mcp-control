// Package cloud implements the small subset of the Midea SmartHome app-facing
// cloud API needed to fetch long-lived V3 LAN credentials.
//
// The protocol is reverse-engineered and undocumented; it is implemented from
// the behaviour of the maintained open-source clients and is pinned to the
// "SmartHome" (MSmartHome) app identity. Treat the endpoints as unstable.
package cloud

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5" // #nosec G501 -- required by the vendor protocol, not used for security decisions
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// UDPPIDMethod selects the byte encoding of an appliance ID before hashing.
type UDPPIDMethod int

const (
	// UDPPIDReversedBig encodes the ID as 8 big-endian bytes, reversed.
	UDPPIDReversedBig UDPPIDMethod = 0
	// UDPPIDBig encodes the ID as 6 big-endian bytes.
	UDPPIDBig UDPPIDMethod = 1
	// UDPPIDLittle encodes the ID as 6 little-endian bytes.
	UDPPIDLittle UDPPIDMethod = 2

	// maxSixByteID is the largest value representable in the 6-byte encodings.
	maxSixByteID = 1<<48 - 1
)

var errNoAESSession = errors.New("cloud: AES session keys are not established")

// security holds the keys for one app identity and the negotiated AES session.
type security struct {
	loginKey string
	iotKey   string
	hmacKey  string

	aesKey []byte
	aesIV  []byte
}

func newSecurity(loginKey, iotKey, hmacKey string) *security {
	return &security{loginKey: loginKey, iotKey: iotKey, hmacKey: hmacKey}
}

// deviceID derives the pseudo-device identifier the cloud expects from the
// account name.
func deviceID(username string) string {
	sum := sha256.Sum256([]byte("Hello, " + username + "!"))
	return hex.EncodeToString(sum[:])[:16]
}

// sign returns the HMAC-SHA256 signature the cloud checks for each request.
func (s *security) sign(data, random string) string {
	mac := hmac.New(sha256.New, []byte(s.hmacKey))
	mac.Write([]byte(s.iotKey + data + random))
	return hex.EncodeToString(mac.Sum(nil))
}

// encryptPassword returns the login password hash bound to the login ID.
func (s *security) encryptPassword(loginID, password string) string {
	sum := sha256.Sum256([]byte(password))
	loginHash := loginID + hex.EncodeToString(sum[:]) + s.loginKey
	result := sha256.Sum256([]byte(loginHash))
	return hex.EncodeToString(result[:])
}

// encryptIAMPassword returns the IAM password hash bound to the login ID.
func (s *security) encryptIAMPassword(loginID, password string) string {
	first := md5.Sum([]byte(password)) // #nosec G401 -- required by the vendor protocol
	second := md5.Sum([]byte(hex.EncodeToString(first[:])))
	loginHash := loginID + hex.EncodeToString(second[:]) + s.loginKey
	result := sha256.Sum256([]byte(loginHash))
	return hex.EncodeToString(result[:])
}

// udpID derives the 32-character hex "udpid" that identifies a device to the
// cloud for a given encoding method.
func udpID(applianceID uint64, method UDPPIDMethod) (string, error) {
	var raw []byte
	switch method {
	case UDPPIDReversedBig:
		var buffer [8]byte
		binary.BigEndian.PutUint64(buffer[:], applianceID)
		raw = []byte{
			buffer[7], buffer[6], buffer[5], buffer[4],
			buffer[3], buffer[2], buffer[1], buffer[0],
		}
	case UDPPIDBig, UDPPIDLittle:
		if applianceID > maxSixByteID {
			return "", fmt.Errorf("cloud: appliance id %d exceeds the 6-byte encoding", applianceID)
		}
		var buffer [8]byte
		if method == UDPPIDBig {
			binary.BigEndian.PutUint64(buffer[:], applianceID)
			raw = buffer[2:]
		} else {
			binary.LittleEndian.PutUint64(buffer[:], applianceID)
			raw = buffer[:6]
		}
	default:
		return "", fmt.Errorf("cloud: unknown udpid method %d", method)
	}

	sum := sha256.Sum256(raw)
	var out [16]byte
	for i := range out {
		out[i] = sum[i] ^ sum[i+16]
	}
	return hex.EncodeToString(out[:]), nil
}

// setSessionKeys unwraps the AES key material returned by a successful login.
func (s *security) setSessionKeys(encryptedKey, encryptedIV string) error {
	digest := sha256.Sum256([]byte(s.loginKey))
	hexDigest := hex.EncodeToString(digest[:])
	tempKey := []byte(hexDigest[:16])
	tempIV := []byte(hexDigest[16:32])

	key, err := aesDecrypt(encryptedKey, tempKey, tempIV)
	if err != nil {
		return fmt.Errorf("cloud: decrypt session key: %w", err)
	}
	iv, err := aesDecrypt(encryptedIV, tempKey, tempIV)
	if err != nil {
		return fmt.Errorf("cloud: decrypt session iv: %w", err)
	}
	s.aesKey = []byte(key)
	s.aesIV = []byte(iv)
	return nil
}

// decrypt uses the negotiated session keys to decrypt a cloud value.
func (s *security) decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(s.aesKey) == 0 {
		return "", errNoAESSession
	}
	return aesDecrypt(value, s.aesKey, s.aesIV)
}

// aesDecrypt decrypts a hex-encoded AES-CBC (or ECB when iv is "0") payload and
// removes PKCS#7 padding.
func aesDecrypt(value string, key, iv []byte) (string, error) {
	ciphertext, err := hex.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	if len(ciphertext) == 0 {
		return "", nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return "", fmt.Errorf("ciphertext length %d is not block aligned", len(ciphertext))
	}

	var plaintext []byte
	switch {
	case len(iv) == 0 || string(iv) == "0":
		plaintext = make([]byte, len(ciphertext))
		for offset := 0; offset < len(ciphertext); offset += aes.BlockSize {
			block.Decrypt(plaintext[offset:offset+aes.BlockSize], ciphertext[offset:offset+aes.BlockSize])
		}
	default:
		if len(iv) != aes.BlockSize {
			return "", fmt.Errorf("invalid iv length %d", len(iv))
		}
		plaintext = make([]byte, len(ciphertext))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
	}

	unpadded, err := unpad(plaintext, aes.BlockSize)
	if err != nil {
		return "", err
	}
	return string(unpadded), nil
}

func unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("cloud: empty plaintext")
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > blockSize || padding > len(data) {
		return nil, fmt.Errorf("cloud: invalid padding length %d", padding)
	}
	for _, value := range data[len(data)-padding:] {
		if int(value) != padding {
			return nil, errors.New("cloud: invalid padding bytes")
		}
	}
	return data[:len(data)-padding], nil
}
