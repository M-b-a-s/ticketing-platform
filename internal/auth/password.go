package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

type Params struct {
	Memory  uint32 // in KiB
	Time    uint32 // iterations
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

var DefaultParams = Params{
	Memory:  64 * 1024,
	Time:    3,
	Threads: 2,
	SaltLen: 16,
	KeyLen:  32,
}

const MaxPasswordLen = 128

var (
	ErrPasswordTooLong = errors.New("auth: password too long")
	ErrInvalidHash     = errors.New("auth: invalid password hash format")
	ErrBadVersion      = errors.New("auth: incompatible argon2 version")
)

var b64 = base64.RawStdEncoding

func HashPassword(password string, p Params) (string, error) {
	if len(password) > MaxPasswordLen {
		return "", ErrPasswordTooLong
	}

	// Generate a random salt.
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	// Derive the key using Argon2id.
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks if the provided password matches the encoded hash.
func VerifyPassword(password, encoded string) (bool, error) {
	if len(password) > MaxPasswordLen {
		return false, nil
	}

	// Decode the hash to get the parameters, salt, and key.
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	other := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(key)))
	return subtle.ConstantTimeCompare(key, other) == 1, nil
}

// decodeHash decodes a hash string into its parameters, salt, and key.
func decodeHash(encoded string) (Params, []byte, []byte, error) {
	var p Params

	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, ErrBadVersion
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	return p, salt, key, nil
}