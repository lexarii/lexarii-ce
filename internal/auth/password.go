// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters.
//
// argon2id rather than bcrypt or scrypt: it is the current password-hashing
// competition winner and resists both GPU and side-channel attack. These are the
// OWASP-recommended baseline - 19 MiB of memory, one iteration - which is chosen
// for memory hardness rather than time, and is comfortable on the modest
// hardware a self-hosted instance actually runs on.
const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrWeakPassword reports a password that fails the minimum policy.
var ErrWeakPassword = errors.New("password is too weak")

// MinPasswordLength is the shortest password accepted.
//
// Length only, with no composition rules. Requiring a digit and a symbol
// reliably produces "Password1!" and nothing safer; length is the property that
// actually costs an attacker anything.
const MinPasswordLength = 10

// HashPassword derives a storable hash in the standard PHC string format, so
// the parameters travel with the hash and can be raised later without
// invalidating existing passwords.
func HashPassword(password string) (string, error) {
	if err := CheckPasswordPolicy(password); err != nil {
		return "", err
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}

	parallelism := uint8(runtime.NumCPU())
	if parallelism > 4 {
		parallelism = 4
	}
	if parallelism == 0 {
		parallelism = 1
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, parallelism, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether a password matches a stored hash.
//
// Comparison is constant time. A byte-by-byte comparison leaks how much of the
// hash matched through timing, which is enough to reconstruct it.
func VerifyPassword(password, encoded string) bool {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false
	}

	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// CheckPasswordPolicy reports whether a password is acceptable.
func CheckPasswordPolicy(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("%w: must be at least %d characters", ErrWeakPassword, MinPasswordLength)
	}
	// A very long password is a denial-of-service vector: argon2 will happily
	// hash a megabyte and take the CPU with it.
	if len(password) > 1024 {
		return fmt.Errorf("%w: must be under 1024 bytes", ErrWeakPassword)
	}
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("%w: must not be only whitespace", ErrWeakPassword)
	}
	return nil
}

type argonParams struct {
	memory      uint32
	time        uint32
	parallelism uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, errors.New("not an argon2id hash")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argonParams{}, nil, nil, err
	}
	if version != argon2.Version {
		return argonParams{}, nil, nil, errors.New("unsupported argon2 version")
	}

	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.parallelism); err != nil {
		return argonParams{}, nil, nil, err
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, err
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return argonParams{}, nil, nil, err
	}

	return p, salt, key, nil
}
