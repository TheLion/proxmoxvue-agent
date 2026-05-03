package crypto

import (
	"bytes"
	"testing"
)

func TestRoundtrip_GeneratedKeypair(t *testing.T) {
	priv, pub, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if len(pub) != kemPublicKeySize {
		t.Errorf("public key size = %d, want %d", len(pub), kemPublicKeySize)
	}

	plaintext := []byte("super-secret-pw-1234")
	payload, err := EncryptForTest(pub, plaintext)
	if err != nil {
		t.Fatalf("EncryptForTest: %v", err)
	}
	if len(payload) <= len(plaintext) {
		t.Errorf("payload should be larger than plaintext (encap-key + AEAD-tag)")
	}

	got, err := Decrypt(priv, payload)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Decrypt = %q, want %q", got, plaintext)
	}
}

func TestPublicKeyFromPrivate_Idempotent(t *testing.T) {
	priv, pub, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	derived, err := PublicKeyFromPrivate(priv)
	if err != nil {
		t.Fatalf("PublicKeyFromPrivate: %v", err)
	}
	if !bytes.Equal(derived, pub) {
		t.Errorf("derived public key != original public key")
	}
}

func TestDecrypt_WrongKey_Fails(t *testing.T) {
	_, pubA, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("kp A: %v", err)
	}
	privB, _, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("kp B: %v", err)
	}
	payload, err := EncryptForTest(pubA, []byte("hello"))
	if err != nil {
		t.Fatalf("EncryptForTest: %v", err)
	}
	if _, err := Decrypt(privB, payload); err == nil {
		t.Errorf("Decrypt with wrong key should fail")
	}
}

func TestDecrypt_TooShort_Fails(t *testing.T) {
	priv, _, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if _, err := Decrypt(priv, []byte("short")); err == nil {
		t.Errorf("Decrypt with truncated payload should fail")
	}
}
