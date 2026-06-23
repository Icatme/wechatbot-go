package crypto

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key, _ := GenerateAESKey()
	plaintext := []byte("Hello, WeChat!")
	ciphertext, err := EncryptAESECB(plaintext, key)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := DecryptAESECB(ciphertext, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("got %q, want %q", decrypted, plaintext)
	}
}

func TestEncryptedSize(t *testing.T) {
	tests := []struct{ raw, want int }{
		{14, 16}, {16, 32}, {100, 112},
	}
	for _, tt := range tests {
		got := EncryptedSize(tt.raw)
		if got != tt.want {
			t.Errorf("EncryptedSize(%d) = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

func TestDecodeAESKeyFormatA(t *testing.T) {
	// base64(raw 16 bytes)
	raw := []byte{0, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	encoded := "ABEiM0RVZneImaq7zN3u/w=="
	decoded, err := DecodeAESKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatalf("got %x, want %x", decoded, raw)
	}
}

func TestDecodeAESKeyDirectHex(t *testing.T) {
	hex := "00112233445566778899aabbccddeeff"
	decoded, err := DecodeAESKey(hex)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 16 {
		t.Fatalf("got len %d, want 16", len(decoded))
	}
}

func TestWrongKeyLength(t *testing.T) {
	_, err := EncryptAESECB([]byte("test"), make([]byte, 8))
	if err == nil {
		t.Fatal("expected error for wrong key length")
	}
}
