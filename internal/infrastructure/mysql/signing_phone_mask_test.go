package mysql

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func setSigningPhoneMaskTestKey(t *testing.T) {
	t.Helper()
	t.Setenv(contract.SigningPhoneKeyEnv, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)))
}

// SEC-D7：读路径掩码投影——密文解密后掩码、存量明文直接掩码、
// 解密失败返回占位；任何分支都不得回传完整号码或密文本身。
func TestMaskStoredSigningPhone(t *testing.T) {
	setSigningPhoneMaskTestKey(t)

	stored, err := contract.EncryptSigningPhone("CON-1", "13800005678")
	if err != nil {
		t.Fatalf("EncryptSigningPhone error = %v", err)
	}
	tests := []struct {
		name     string
		contract string
		stored   string
		want     string
	}{
		{name: "ciphertext", contract: "CON-1", stored: stored, want: "138****5678"},
		{name: "legacy plaintext", contract: "CON-1", stored: "13800005678", want: "138****5678"},
		{name: "ciphertext bound to another contract", contract: "CON-2", stored: stored, want: "***"},
		{name: "empty", contract: "CON-1", stored: "", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := maskStoredSigningPhone(test.contract, test.stored)
			if got != test.want {
				t.Fatalf("maskStoredSigningPhone = %q, want %q", got, test.want)
			}
			if strings.Contains(got, "13800005678") || strings.Contains(got, "enc:v1:") {
				t.Fatalf("masked value %q leaks plaintext or ciphertext", got)
			}
		})
	}
}
