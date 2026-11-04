package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
)

// seal encrypts plaintext to an instance's P-256 public key (base64 PKIX DER,
// as the agent prints it) in the format the agent's open_payload reads:
// base64(ephemeral public key DER || IV || AES-256-CBC ciphertext), where the
// AES key is SHA-256 of the ECDH shared secret.
func seal(instancePublicKey string, plaintext []byte) (string, error) {
	der, err := base64.StdEncoding.DecodeString(instancePublicKey)
	if err != nil {
		return "", fmt.Errorf("decode instance key: %w", err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return "", fmt.Errorf("parse instance key: %w", err)
	}
	ecdsaKey, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return "", fmt.Errorf("instance key is %T, not P-256", parsed)
	}
	peer, err := ecdsaKey.ECDH()
	if err != nil || peer.Curve() != ecdh.P256() {
		return "", fmt.Errorf("instance key is not P-256")
	}

	ephemeral, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	shared, err := ephemeral.ECDH(peer)
	if err != nil {
		return "", err
	}
	key := sha256.Sum256(shared)
	ephemeralDER, err := x509.MarshalPKIXPublicKey(ephemeral.PublicKey())
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(bytes.Clone(plaintext), bytes.Repeat([]byte{byte(pad)}, pad)...)
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	out := append(append(ephemeralDER, iv...), ciphertext...)
	return base64.StdEncoding.EncodeToString(out), nil
}
