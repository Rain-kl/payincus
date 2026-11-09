// Port of server/src/lib/node-security/vault.ts.
//
// Seals a node's ZFS volume key at rest so a database dump alone cannot decrypt
// node storage. The envelope is `v1:<iv>:<tag>:<ciphertext>`, all hex, and the
// identity triple (hostId, dataset, version) is bound as AES-GCM additional data —
// so a sealed key copied onto another host, dataset or version fails
// authentication instead of silently decrypting the wrong volume.
//
// The master key is derived with HKDF-SHA256 from ENCRYPTION_KEY, using
// 'payincus-node-storage' as salt and 'vault-v1' as info. Those strings are the
// compatibility contract with the Node implementation: changing either makes every
// existing envelope undecryptable.
package lib

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// NodeStorageKeyIdentity mirrors KeyIdentity.
type NodeStorageKeyIdentity struct {
	HostID  int32
	Dataset string
	Version int32
}

// nodeStorageDatasetPattern mirrors datasetPattern.
var nodeStorageDatasetPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)*$`)

// nodeStorageHex64Pattern mirrors /^[0-9a-f]{64}$/.
var nodeStorageHex64Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NodeStorageKeyFingerprint mirrors fingerprint(key): sha256 of the 32 raw key
// bytes, hex-encoded. It rejects anything that is not exactly 64 lowercase hex
// chars, so a malformed key cannot be silently truncated.
func NodeStorageKeyFingerprint(key string) (string, error) {
	if !nodeStorageHex64Pattern.MatchString(key) {
		return "", errors.New("需要 64 位十六进制随机 ZFS 密钥")
	}
	raw, err := hex.DecodeString(key)
	if err != nil {
		return "", errors.New("需要 64 位十六进制随机 ZFS 密钥")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// nodeStorageIdentity mirrors the private identity(): it serialises the triple as
// JSON `[hostId, dataset, version]` for use as AES-GCM additional data.
func nodeStorageIdentity(value NodeStorageKeyIdentity) ([]byte, error) {
	if value.HostID < 1 || value.Version < 1 {
		return nil, errors.New("无效的存储身份")
	}
	if !nodeStorageDatasetPattern.MatchString(value.Dataset) ||
		strings.Contains(value.Dataset, "..") || len(value.Dataset) > 200 {
		return nil, errors.New("无效的存储身份")
	}
	return json.Marshal([]any{value.HostID, value.Dataset, value.Version})
}

// nodeStorageMaster mirrors master(secret): HKDF-SHA256 → 32 bytes.
func nodeStorageMaster(secret string) ([]byte, error) {
	if len(secret) < 32 {
		// The TS checks `!secret || secret.length < 32` first, so an empty secret
		// and a short one produce the same message.
		return nil, errors.New("控制端 ENCRYPTION_KEY 未配置或强度不足")
	}

	derived := make([]byte, 32)
	reader := hkdf.New(sha256.New, []byte(secret), []byte("payincus-node-storage"), []byte("vault-v1"))
	if _, err := reader.Read(derived); err != nil {
		return nil, fmt.Errorf("无法派生存储密钥: %w", err)
	}
	return derived, nil
}

// nodeStorageSecret resolves the master secret, mirroring the TS default parameter
// `process.env.ENCRYPTION_KEY || ”`.
func nodeStorageSecret() string {
	return os.Getenv("ENCRYPTION_KEY")
}

// SealNodeStorageKey mirrors seal(key, value, secret).
//
// The returned envelope is `v1:<iv hex>:<tag hex>:<ciphertext hex>`, where the
// ciphertext is AES-256-GCM over the RAW key bytes (the hex is decoded first), so
// the plaintext inside the envelope is never the hex string.
func SealNodeStorageKey(key string, value NodeStorageKeyIdentity) (string, error) {
	return SealNodeStorageKeyWithSecret(key, value, nodeStorageSecret())
}

// SealNodeStorageKeyWithSecret is SealNodeStorageKey with an explicit secret
// (used by tests so they do not depend on the ambient environment).
func SealNodeStorageKeyWithSecret(key string, value NodeStorageKeyIdentity, secret string) (string, error) {
	if _, err := NodeStorageKeyFingerprint(key); err != nil {
		return "", err
	}

	additionalData, err := nodeStorageIdentity(value)
	if err != nil {
		return "", err
	}

	master, err := nodeStorageMaster(secret)
	if err != nil {
		return "", err
	}
	defer wipe(master)

	block, err := aes.NewCipher(master)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	raw, err := hex.DecodeString(key)
	if err != nil {
		return "", errors.New("需要 64 位十六进制随机 ZFS 密钥")
	}
	defer wipe(raw)

	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}

	sealed := aead.Seal(nil, iv, raw, additionalData)
	// Go's GCM output is ciphertext||tag; the TS stores them separately.
	tagOffset := len(sealed) - aead.Overhead()
	ciphertext := sealed[:tagOffset]
	tag := sealed[tagOffset:]

	return strings.Join([]string{
		"v1",
		hex.EncodeToString(iv),
		hex.EncodeToString(tag),
		hex.EncodeToString(ciphertext),
	}, ":"), nil
}

// nodeStorageEnvelopeIVPattern mirrors /^[0-9a-f]{24}$/.
var nodeStorageEnvelopeIVPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)

// nodeStorageEnvelopeTagPattern mirrors /^[0-9a-f]{32}$/.
var nodeStorageEnvelopeTagPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// UnsealNodeStorageKey mirrors unseal(envelope, value, secret).
//
// A wrong identity, a tampered ciphertext or a wrong master key all fail the GCM
// tag check and surface as 存储密钥校验失败 — the caller must not distinguish them.
func UnsealNodeStorageKey(envelope string, value NodeStorageKeyIdentity) (string, error) {
	return UnsealNodeStorageKeyWithSecret(envelope, value, nodeStorageSecret())
}

// UnsealNodeStorageKeyWithSecret is UnsealNodeStorageKey with an explicit secret.
func UnsealNodeStorageKeyWithSecret(envelope string, value NodeStorageKeyIdentity, secret string) (string, error) {
	parts := strings.Split(envelope, ":")
	if len(parts) != 4 || parts[0] != "v1" ||
		!nodeStorageEnvelopeIVPattern.MatchString(parts[1]) ||
		!nodeStorageEnvelopeTagPattern.MatchString(parts[2]) ||
		!nodeStorageHex64Pattern.MatchString(parts[3]) {
		return "", errors.New("存储密钥格式不兼容")
	}

	additionalData, err := nodeStorageIdentity(value)
	if err != nil {
		return "", err
	}

	master, err := nodeStorageMaster(secret)
	if err != nil {
		return "", err
	}
	defer wipe(master)

	block, err := aes.NewCipher(master)
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}

	iv, err := hex.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("存储密钥格式不兼容")
	}
	tag, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("存储密钥格式不兼容")
	}
	ciphertext, err := hex.DecodeString(parts[3])
	if err != nil {
		return "", errors.New("存储密钥格式不兼容")
	}

	// Go expects ciphertext||tag in one slice.
	sealed := append(ciphertext, tag...)
	plain, err := aead.Open(nil, iv, sealed, additionalData)
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}
	defer wipe(plain)

	return hex.EncodeToString(plain), nil
}

// wipe zeroes a byte slice so key material does not linger in memory, mirroring
// the TS `buffer.fill(0)` calls in finally blocks.
func wipe(buffer []byte) {
	for index := range buffer {
		buffer[index] = 0
	}
}
