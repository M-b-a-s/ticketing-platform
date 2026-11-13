package auth

import "testing"

// Cheap params so tests run fast.
var testParams = Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple", testParams)
	if err != nil {
		t.Fatal(err)
	}

	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("expected match, got ok=%v err=%v", ok, err)
	}

	ok, _ = VerifyPassword("wrong password", hash)
	if ok {
		t.Fatal("wrong password should not match")
	}
}

func TestSamePasswordDifferentHashes(t *testing.T) {
	a, _ := HashPassword("secret123", testParams)
	b, _ := HashPassword("secret123", testParams)
	if a == b {
		t.Fatal("salts should make every hash unique")
	}
}

func TestRejectsOverlongPassword(t *testing.T) {
	long := make([]byte, MaxPasswordLen+1)
	if _, err := HashPassword(string(long), testParams); err != ErrPasswordTooLong {
		t.Fatalf("expected ErrPasswordTooLong, got %v", err)
	}
}

func TestMalformedHash(t *testing.T) {
	if _, err := VerifyPassword("x", "not-a-hash"); err != ErrInvalidHash {
		t.Fatalf("expected ErrInvalidHash, got %v", err)
	}
}