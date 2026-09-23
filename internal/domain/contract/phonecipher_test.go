package contract

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func setSigningPhoneTestKey(t *testing.T) {
	t.Helper()
	t.Setenv(SigningPhoneKeyEnv, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32)))
}

// SEC-D7：AES-GCM 列加密往返——密文带前缀、不含明文、长度落在 VARCHAR(80) 内。
func TestSigningPhoneEncryptDecryptRoundTrip(t *testing.T) {
	setSigningPhoneTestKey(t)
	for _, plain := range []string{"13800005678", "+86 138 0000 5678", "010-8888-6666"} {
		stored, err := EncryptSigningPhone("CON-1", plain)
		if err != nil {
			t.Fatalf("EncryptSigningPhone(%q) error = %v", plain, err)
		}
		if !IsSigningPhoneCiphertext(stored) {
			t.Fatalf("stored = %q, want enc:v1: prefix", stored)
		}
		if strings.Contains(stored, plain) {
			t.Fatalf("stored = %q leaks plaintext %q", stored, plain)
		}
		if len(stored) > 80 {
			t.Fatalf("stored length = %d, want <= 80 (VARCHAR(80) from migration 000016)", len(stored))
		}
		got, err := DecryptSigningPhone("CON-1", stored)
		if err != nil {
			t.Fatalf("DecryptSigningPhone error = %v", err)
		}
		if got != plain {
			t.Fatalf("round trip = %q, want %q", got, plain)
		}
	}
}

// SEC-D7：AAD 绑定合同 ID——把密文搬到另一合同行后必须解密失败。
func TestSigningPhoneCiphertextBindsContractID(t *testing.T) {
	setSigningPhoneTestKey(t)
	stored, err := EncryptSigningPhone("CON-1", "13800005678")
	if err != nil {
		t.Fatalf("EncryptSigningPhone error = %v", err)
	}
	if _, err := DecryptSigningPhone("CON-2", stored); err == nil {
		t.Fatal("decrypt with a different contract id must fail (AAD mismatch)")
	}
	if _, err := DecryptSigningPhone("CON-1", "13800005678"); err == nil {
		t.Fatal("decrypting a legacy plaintext value must fail")
	}
}

// SEC-D7：20 字符上限内的密文始终能存入 VARCHAR(80)。
func TestSigningPhoneMaxLengthCiphertextFitsColumn(t *testing.T) {
	setSigningPhoneTestKey(t)
	stored, err := EncryptSigningPhone("CON-1", strings.Repeat("1", MaxSigningPhonePlaintextLen))
	if err != nil {
		t.Fatalf("EncryptSigningPhone error = %v", err)
	}
	if len(stored) > 80 {
		t.Fatalf("max-length ciphertext = %d bytes, want <= 80", len(stored))
	}
}

// SEC-D7：密钥必填（*_KEY_BASE64 模式）——缺失/非法密钥必须报错而不是静默降级。
func TestSigningPhoneKeyIsRequired(t *testing.T) {
	t.Setenv(SigningPhoneKeyEnv, "")
	if _, err := EncryptSigningPhone("CON-1", "13800005678"); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("missing key error = %v, want %q", err, SigningPhoneKeyEnv+" is required")
	}
	if err := SigningPhoneKeyStatus(); err == nil {
		t.Fatal("SigningPhoneKeyStatus() = nil with missing key, want error")
	}

	t.Setenv(SigningPhoneKeyEnv, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, 16)))
	if _, err := EncryptSigningPhone("CON-1", "13800005678"); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("short key error = %v, want exactly 32 bytes", err)
	}

	t.Setenv(SigningPhoneKeyEnv, "not-base64!!")
	if err := SigningPhoneKeyStatus(); err == nil {
		t.Fatal("SigningPhoneKeyStatus() = nil with invalid base64, want error")
	}
}

// SEC-D7：读路径掩码规则（138****5678 形态）。
func TestMaskPhone(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"123", "***"},
		{"1234", "****"},
		{"12345", "12***"},
		{"1381234", "13*****"},
		{"13800005", "138****0005"},
		{"13800005678", "138****5678"},
		{"+8613800005678", "+86****5678"},
		{" 13800005678 ", "138****5678"},
	}
	for _, test := range tests {
		if got := MaskPhone(test.in); got != test.want {
			t.Fatalf("MaskPhone(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}
