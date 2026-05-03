// Package crypto implements HPKE (RFC 9180) encryption helpers used
// for E2E encryption of LXC create-passwords (#1476). The agent
// generates an X25519 keypair, publishes the public key in
// Supabase's clusters.public_key, and decrypts the password_enc
// payloads that iOS seals on incoming commands.
//
// Suite: DHKEM(X25519, HKDF-SHA256) + HKDF-SHA256 + ChaCha20Poly1305.
// Matches 1:1 with Apple's `HPKE.Ciphersuite.Curve25519_SHA256_ChachaPoly`.
package crypto

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/cloudflare/circl/hpke"
)

// suite is the HPKE ciphersuite shared between iOS and the agent.
var suite = hpke.NewSuite(
	hpke.KEM_X25519_HKDF_SHA256,
	hpke.KDF_HKDF_SHA256,
	hpke.AEAD_ChaCha20Poly1305,
)

// kemPublicKeySize is the wire size of an X25519 public-key
// encapsulation in HPKE — 32 raw curve-point bytes. Apple's HPKE
// serialises identically.
const kemPublicKeySize = 32

// GenerateKeypair creates a new X25519 keypair for HPKE decryption.
// Returns raw bytes — the caller serialises them (e.g. base64 for
// YAML or bytea for Postgres).
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

// PublicKeyFromPrivate derives the matching public key from a
// persisted private key. Used at --run time to check whether
// clusters.public_key matches without generating a fresh keypair.
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

// Decrypt opens an HPKE-sealed payload with the agent's private key.
// The payload is generated on iOS as `encapsulated_key (32 bytes) ||
// ciphertext (var)` — base64 decoding is done by the caller.
//
// No `info` or `aad` (empty Data on both sides); enough for one AEAD
// slot. If the ciphersuite ever changes a password_enc_v2 field is
// added rather than format-bumping this decoder.
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

// EncryptForTest is only for the Decrypt roundtrip test — that way
// dispatcher_test.go can build HPKE-sealed payloads without iOS-side
// crypto. Not for production use (production seals on the iOS side).
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
