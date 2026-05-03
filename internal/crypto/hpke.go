// Package crypto implements HPKE (RFC 9180) encryption helpers used voor
// E2E-encryptie van LXC create-passwords (#1476). De agent genereert een
// X25519 keypair, publiceert de public key in Supabase clusters.public_key,
// en decrypt op binnenkomst de password_enc-payloads die iOS sealt.
//
// Suite: DHKEM(X25519, HKDF-SHA256) + HKDF-SHA256 + ChaCha20Poly1305.
// Matcht 1:1 met Apple's `HPKE.Ciphersuite.Curve25519_SHA256_ChaChaPoly`.
package crypto

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/cloudflare/circl/hpke"
)

// suite is de gedeelde HPKE-ciphersuite tussen iOS en agent.
var suite = hpke.NewSuite(
	hpke.KEM_X25519_HKDF_SHA256,
	hpke.KDF_HKDF_SHA256,
	hpke.AEAD_ChaCha20Poly1305,
)

// kemPublicKeySize is de wire-grootte van een X25519 public-key encapsulation
// in HPKE — 32 bytes raw curve point. Apple's HPKE serialiseert idem.
const kemPublicKeySize = 32

// GenerateKeypair maakt een nieuwe X25519 keypair voor HPKE-decryptie.
// Retourneert raw bytes — de caller serialiseert ze (bv. base64 voor YAML
// of bytea voor Postgres).
func GenerateKeypair() (privateKey, publicKey []byte, err error) {
	pub, priv, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().GenerateKeyPair()
	if err != nil {
		return nil, nil, fmt.Errorf("hpke generate: %w", err)
	}
	privBytes, err := priv.MarshalBinary()
	if err != nil {
		return nil, nil, fmt.Errorf("hpke marshal private: %w", err)
	}
	pubBytes, err := pub.MarshalBinary()
	if err != nil {
		return nil, nil, fmt.Errorf("hpke marshal public: %w", err)
	}
	return privBytes, pubBytes, nil
}

// PublicKeyFromPrivate leidt de bijbehorende public key af uit een
// gepersisteerde private key. Gebruikt om bij --run te kunnen verifiëren
// of clusters.public_key matcht zonder een nieuwe keypair te genereren.
func PublicKeyFromPrivate(privateKey []byte) ([]byte, error) {
	priv, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().UnmarshalBinaryPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("hpke unmarshal private: %w", err)
	}
	pub := priv.Public()
	pubBytes, err := pub.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("hpke marshal public: %w", err)
	}
	return pubBytes, nil
}

// Decrypt opent een HPKE-sealed payload met de agent's private key. De
// payload is op iOS gegenereerd als `encapsulated_key (32 bytes) ||
// ciphertext (var)` — base64-decoding doet de caller.
//
// Geen `info` of `aad` (lege Data aan beide kanten); voor één AEAD-slot is
// dat voldoende. Als de ciphersuite ooit verandert komt er een password_enc_v2
// veld bij en geen format-bumps in deze decoder.
func Decrypt(privateKey []byte, payload []byte) ([]byte, error) {
	if len(payload) < kemPublicKeySize+1 {
		return nil, errors.New("hpke decrypt: payload too short")
	}
	priv, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().UnmarshalBinaryPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("hpke unmarshal private: %w", err)
	}
	encap := payload[:kemPublicKeySize]
	ciphertext := payload[kemPublicKeySize:]

	receiver, err := suite.NewReceiver(priv, nil)
	if err != nil {
		return nil, fmt.Errorf("hpke new receiver: %w", err)
	}
	opener, err := receiver.Setup(encap)
	if err != nil {
		return nil, fmt.Errorf("hpke setup: %w", err)
	}
	plaintext, err := opener.Open(ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("hpke open: %w", err)
	}
	return plaintext, nil
}

// EncryptForTest is alleen voor de roundtrip-test van Decrypt — zo kunnen
// we in dispatcher_test.go HPKE-sealed payloads bouwen zonder iOS-side
// crypto. Niet voor productie-gebruik (productie sealt iOS-side).
func EncryptForTest(publicKey []byte, plaintext []byte) ([]byte, error) {
	pub, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().UnmarshalBinaryPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("hpke unmarshal public: %w", err)
	}
	sender, err := suite.NewSender(pub, nil)
	if err != nil {
		return nil, fmt.Errorf("hpke new sender: %w", err)
	}
	encap, sealer, err := sender.Setup(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("hpke sender setup: %w", err)
	}
	ciphertext, err := sealer.Seal(plaintext, nil)
	if err != nil {
		return nil, fmt.Errorf("hpke seal: %w", err)
	}
	return append(encap, ciphertext...), nil
}
