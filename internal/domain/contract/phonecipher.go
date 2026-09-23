package contract

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// 签署人手机号列加密与掩码（SEC-D7）。
//
// 安全理由：recipient_phone 是收件人 PII，明文落库会让 DB dump/备份与任何
// 合同阅读者直接看到完整号码，与平台约定（CRM phone_cipher+mask、平台
// email_digest/mobile_digest）不一致。这里实现：
//   - AES-GCM 列加密（密钥走既有 *_KEY_BASE64 必填模式，32 字节 Base64，
//     与 OIDC_SESSION_ENCRYPTION_KEY_BASE64 一致的校验语义）；
//   - 读路径掩码投影（138****5678），API 永远不回传完整号码；
//   - 密文带 "enc:v1:" 前缀以区分 000016 之后写入的存量明文（懒迁移用）。
const (
	// SigningPhoneKeyEnv 是列加密密钥环境变量：Base64 解码后必须恰好 32 字节，必填。
	SigningPhoneKeyEnv = "SIGNING_PHONE_ENCRYPTION_KEY_BASE64"
	// signingPhoneCipherPrefix 标识密文，读路径据此区分密文与存量明文。
	signingPhoneCipherPrefix = "enc:v1:"
	// MaxSigningPhonePlaintextLen 是明文手机号长度上限（字符数）。
	// 密文长度 = 前缀(7) + Base64(Nonce 12 + 明文 + GCM tag 16)：
	// 明文 20 字符时密文 71 字节，稳定落在 000016 迁移的 VARCHAR(80) 内。
	MaxSigningPhonePlaintextLen = 20
	// signingPhoneAADPrefix 绑定上下文：AAD 固定前缀 + 合同 ID，
	// 防止把某行密文搬到另一行（或篡改 contract_id）后仍然解密成功。
	signingPhoneAADPrefix = "signing-phone:"
)

// signingPhoneKey 读取并校验列加密密钥（*_KEY_BASE64 必填模式）。
// 每次调用都重新读取环境，便于测试注入，也保证密钥轮换后无需重启读取路径。
func signingPhoneKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(SigningPhoneKeyEnv))
	if raw == "" {
		return nil, fmt.Errorf("%s is required", SigningPhoneKeyEnv)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", SigningPhoneKeyEnv, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to exactly 32 bytes", SigningPhoneKeyEnv)
	}
	return key, nil
}

// SigningPhoneKeyStatus 供就绪检查使用：返回 nil 表示密钥可用。
func SigningPhoneKeyStatus() error {
	_, err := signingPhoneKey()
	return err
}

func newSigningPhoneAEAD() (cipher.AEAD, error) {
	key, err := signingPhoneKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// EncryptSigningPhone 用 AES-GCM 加密签署人手机号，输出 "enc:v1:"+Base64(Nonce||密文)。
// 空输入返回空串（无 PII 可加密）；密钥缺失/非法直接返回错误（写入失败关闭，
// 绝不回退明文存储）。
func EncryptSigningPhone(contractID, plaintext string) (string, error) {
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" {
		return "", nil
	}
	aead, err := newSigningPhoneAEAD()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, nonce, []byte(plaintext), signingPhoneAAD(contractID))
	return signingPhoneCipherPrefix + base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// DecryptSigningPhone 解密列密文并校验 AAD（合同 ID 绑定）。
// 非本格式的值（如存量明文）返回错误，调用方据此走掩码而不是解密路径。
func DecryptSigningPhone(contractID, stored string) (string, error) {
	if !IsSigningPhoneCiphertext(stored) {
		return "", fmt.Errorf("not a signing phone ciphertext")
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, signingPhoneCipherPrefix))
	if err != nil {
		return "", err
	}
	aead, err := newSigningPhoneAEAD()
	if err != nil {
		return "", err
	}
	nonceSize := aead.NonceSize()
	if len(payload) < nonceSize+aead.Overhead() {
		return "", fmt.Errorf("signing phone ciphertext is truncated")
	}
	plain, err := aead.Open(nil, payload[:nonceSize], payload[nonceSize:], signingPhoneAAD(contractID))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// IsSigningPhoneCiphertext 判断值是否为本格式密文（含存量明文在内的其余值返回 false）。
func IsSigningPhoneCiphertext(value string) bool {
	return strings.HasPrefix(value, signingPhoneCipherPrefix)
}

func signingPhoneAAD(contractID string) []byte {
	return []byte(signingPhoneAADPrefix + contractID)
}

// MaskPhone 掩码手机号（读路径投影，如 138****5678）：
//   - 空值原样返回；
//   - 4 个字符及以下整体打码，不泄露任何片段；
//   - 5~7 个字符保留前 2 位，其余打码；
//   - 8 个字符及以上保留前 3 后 4，中间以 **** 隔开。
func MaskPhone(phone string) string {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return ""
	}
	runes := []rune(phone)
	if len(runes) <= 4 {
		return strings.Repeat("*", len(runes))
	}
	if len(runes) <= 7 {
		return string(runes[:2]) + strings.Repeat("*", len(runes)-2)
	}
	return string(runes[:3]) + "****" + string(runes[len(runes)-4:])
}
