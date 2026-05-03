// Package keysync centralises HPKE key lifecycle management for the agent.
//
// The agent stores a base64-encoded X25519 private key in
// `supabase.private_key` (config.yml) and the matching public key in
// the `clusters.public_key` column of the Supabase backend. iOS uses
// the public key to seal LXC root passwords (#1476).
//
// Two operations are exposed:
//
//   - EnsurePrivateKey: returns the existing private key from config,
//     or generates a fresh keypair and persists it. Used at agent
//     startup (auto-heal for installs from before #1476) and by
//     `--rotate-key`.
//
//   - UploadPublicKey: derives the public key from a private key and
//     writes it to clusters.public_key. Idempotent (PATCH overwrites
//     on each call) so retry is safe.
//
// Same primitives are reused by `--register`, `--run` (auto-heal),
// and `--rotate-key`.
package keysync

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	agentcrypto "github.com/TheLion/proxmoxvue-agent/internal/crypto"
	"github.com/TheLion/proxmoxvue-agent/internal/config"
	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
)

// EnsurePrivateKey returns the base64-encoded private key from cfg,
// generating and persisting a fresh keypair if the field is empty.
// `generated` is true when a new keypair was created and saved.
//
// On generation: the keypair is written to `configPath` immediately so
// a subsequent process restart picks up the same key. The matching
// public key still needs to be pushed to Supabase via UploadPublicKey.
func EnsurePrivateKey(cfg *config.File, configPath string) (privateKeyB64 string, generated bool, err error) {
	if cfg.Supabase.PrivateKey != "" {
		return cfg.Supabase.PrivateKey, false, nil
	}
	privBytes, _, err := agentcrypto.GenerateKeypair()
	if err != nil {
		return "", false, fmt.Errorf("generate keypair: %w", err)
	}
	privateKeyB64 = base64.StdEncoding.EncodeToString(privBytes)
	cfg.Supabase.PrivateKey = privateKeyB64
	if err := config.Save(configPath, *cfg); err != nil {
		return "", false, fmt.Errorf("save config with new private key: %w", err)
	}
	return privateKeyB64, true, nil
}

// RotateKey unconditionally generates a fresh keypair, persists the
// private key to configPath, and uploads the matching public key via
// the supplied Supabase client. Existing private key is overwritten.
func RotateKey(ctx context.Context, cfg *config.File, configPath string, sb *supabase.Client) (privateKeyB64 string, err error) {
	privBytes, _, err := agentcrypto.GenerateKeypair()
	if err != nil {
		return "", fmt.Errorf("generate keypair: %w", err)
	}
	privateKeyB64 = base64.StdEncoding.EncodeToString(privBytes)
	cfg.Supabase.PrivateKey = privateKeyB64
	if err := config.Save(configPath, *cfg); err != nil {
		return "", fmt.Errorf("save config with rotated private key: %w", err)
	}
	if err := UploadPublicKey(ctx, sb, cfg.Supabase.ClusterID, privateKeyB64); err != nil {
		return privateKeyB64, fmt.Errorf("upload rotated public key: %w", err)
	}
	return privateKeyB64, nil
}

// UploadPublicKey derives the X25519 public key from privateKeyB64 and
// PATCHes clusters.public_key for clusterID. Idempotent.
//
// Caller-provided sb client must already be authenticated against the
// project. Use a 30s timeout context if the caller has none.
func UploadPublicKey(ctx context.Context, sb *supabase.Client, clusterID string, privateKeyB64 string) error {
	privBytes, err := base64.StdEncoding.DecodeString(privateKeyB64)
	if err != nil {
		return fmt.Errorf("decode private key: %w", err)
	}
	pubBytes, err := agentcrypto.PublicKeyFromPrivate(privBytes)
	if err != nil {
		return fmt.Errorf("derive public key: %w", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	return sb.UploadClusterPublicKey(ctx, clusterID, pubBytes)
}
