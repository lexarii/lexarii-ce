// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash = %q, want PHC argon2id format", hash)
	}
	// The parameters travel with the hash so they can be raised later without
	// invalidating every existing password.
	if !strings.Contains(hash, "m=") || !strings.Contains(hash, "t=") || !strings.Contains(hash, "p=") {
		t.Errorf("hash does not carry its parameters: %q", hash)
	}
	if strings.Contains(hash, password) {
		t.Fatal("the hash contains the password")
	}

	if !VerifyPassword(password, hash) {
		t.Error("the correct password did not verify")
	}
	for _, wrong := range []string{
		"", "wrong", password + " ", " " + password,
		strings.ToUpper(password), password[:len(password)-1],
	} {
		if VerifyPassword(wrong, hash) {
			t.Errorf("a wrong password verified: %q", wrong)
		}
	}
}

// Two accounts with the same password must not produce the same hash, or a
// leaked database reveals which users share one.
func TestHashesAreSalted(t *testing.T) {
	const password = "the same password twice"

	a, err := HashPassword(password)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := HashPassword(password)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if a == b {
		t.Fatal("identical passwords produced identical hashes; the salt is not working")
	}
	if !VerifyPassword(password, a) || !VerifyPassword(password, b) {
		t.Fatal("a salted hash failed to verify")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"", "not a hash", "$argon2id$", "$bcrypt$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=99$m=1,t=1,p=1$c2FsdA$aGFzaA", // wrong version
		"$argon2id$v=19$m=1,t=1,p=1$!!!$aGFzaA",    // bad base64
	} {
		t.Run(bad, func(t *testing.T) {
			if VerifyPassword("anything", bad) {
				t.Fatalf("a malformed hash verified: %q", bad)
			}
		})
	}
}

func TestPasswordPolicy(t *testing.T) {
	tooShort := strings.Repeat("a", MinPasswordLength-1)
	if err := CheckPasswordPolicy(tooShort); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("a short password was accepted: %v", err)
	}
	if err := CheckPasswordPolicy("          "); !errors.Is(err, ErrWeakPassword) {
		t.Error("an all-whitespace password was accepted")
	}
	// Hashing a megabyte would take the CPU with it.
	if err := CheckPasswordPolicy(strings.Repeat("x", 4096)); !errors.Is(err, ErrWeakPassword) {
		t.Error("an absurdly long password was accepted")
	}
	if err := CheckPasswordPolicy(strings.Repeat("a", MinPasswordLength)); err != nil {
		t.Errorf("a password at the minimum length was rejected: %v", err)
	}
	// Length only: composition rules reliably produce "Password1!" and nothing
	// safer.
	if err := CheckPasswordPolicy("all lowercase and no digits at all"); err != nil {
		t.Errorf("a long passphrase was rejected for composition: %v", err)
	}
}

func TestHashPasswordRejectsWeakPasswords(t *testing.T) {
	if _, err := HashPassword("short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("HashPassword accepted a weak password: %v", err)
	}
}
