package keys

import "testing"

func TestEncryptDecryptRoundtrip(t *testing.T) {
	mk, err := MasterKeyFromHex("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("sk-deepseek-real-key")
	blob, err := Encrypt(plaintext, mk)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) == string(plaintext) {
		t.Fatal("ciphertext should not equal plaintext")
	}
	got, err := Decrypt(blob, mk)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("roundtrip mismatch: got %q want %q", got, plaintext)
	}
}

func TestMasterKeyFromHexBadLength(t *testing.T) {
	if _, err := MasterKeyFromHex("deadbeef"); err == nil {
		t.Fatal("expected error for short hex")
	}
}

func TestSHA256HashLength(t *testing.T) {
	if got := len(SHA256Hash("gw-abc")); got != 64 {
		t.Fatalf("hash length %d, want 64", got)
	}
}

func TestGenerateKeyUnique(t *testing.T) {
	k1, err1 := GenerateKey()
	k2, err2 := GenerateKey()
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if k1 == k2 {
		t.Fatal("keys should be unique")
	}
	if len(k1) < 40 {
		t.Fatalf("key too short: %d", len(k1))
	}
}
