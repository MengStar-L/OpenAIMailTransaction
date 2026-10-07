package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

const cdkCipherDomain = "cdk-code-at-rest-v1"

func (a *App) cdkCipher() (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(cdkCipherDomain))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (a *App) encryptCDKCode(code string) (string, error) {
	code = normalizeCDK(code)
	if code == "" || len(code) > 200 {
		return "", errors.New("兑换码无效")
	}
	gcm, err := a.cdkCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(code), []byte(cdkCipherDomain))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (a *App) decryptCDKCode(value string) (string, error) {
	invalid := errors.New("无法读取已保存的兑换码")
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", invalid
	}
	gcm, err := a.cdkCipher()
	if err != nil || len(data) < gcm.NonceSize()+gcm.Overhead() {
		return "", invalid
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte(cdkCipherDomain))
	if err != nil || len(plain) == 0 || len(plain) > 200 {
		return "", invalid
	}
	return string(plain), nil
}

// Decrypt only at an authenticated administration boundary, never for public
// order responses. Legacy hash-only vouchers deliberately have no recoverable code.
func (a *App) hydrateCDKCode(c *CDK) error {
	c.Code = ""
	if c.CodeCipher == "" {
		return nil
	}
	code, err := a.decryptCDKCode(c.CodeCipher)
	if err != nil {
		return err
	}
	if hash(code) != c.Hash {
		return errors.New("已保存的兑换码校验失败")
	}
	c.Code = code
	return nil
}

// A legitimate redemption supplies an old voucher's original code. Preserve it
// only when its hash matches, and never overwrite an existing encrypted copy.
func (a *App) rememberCDKCode(cdkID, code string) error {
	code = normalizeCDK(code)
	encrypted, err := a.encryptCDKCode(code)
	if err != nil {
		return err
	}
	_, err = a.db.Exec("UPDATE cdks SET code_cipher=? WHERE id=? AND hash=? AND code_cipher=''", encrypted, cdkID, hash(code))
	return err
}
