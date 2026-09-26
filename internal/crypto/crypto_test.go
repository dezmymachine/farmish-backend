package crypto

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCrypto_RoundTrip(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encrypt([]byte("GHA-123456789-0"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "v1:") {
		t.Errorf("ciphertext %q lacks the v1 prefix", enc)
	}
	plain, err := c.Decrypt(enc)
	if err != nil || string(plain) != "GHA-123456789-0" {
		t.Errorf("round trip = %q, %v", plain, err)
	}
}

func TestCrypto_TamperFails(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encrypt([]byte("GHA-123456789-0"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tampered := range []string{
		enc[:len(enc)-1] + "A",
		"v2:" + enc[len("v1:"):],
		"v1:!!!not-base64!!!",
		"v1:" + "AAAA",
		"",
	} {
		if _, err := c.Decrypt(tampered); !errors.Is(err, ErrDecrypt) {
			t.Errorf("Decrypt(%q) = %v, want ErrDecrypt", tampered, err)
		}
	}
}

func TestCrypto_WrongKeyFails(t *testing.T) {
	c1, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c1.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Decrypt(enc); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong-key Decrypt = %v, want ErrDecrypt", err)
	}
}

func TestCrypto_UniqueNonces(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Encrypt([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encrypt([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two encryptions of the same plaintext are identical (nonce reused)")
	}
}

func TestNew_RejectsShortKey(t *testing.T) {
	if _, err := New([]byte("too short")); err == nil {
		t.Error("short key accepted")
	}
}
