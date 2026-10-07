package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"openai-mail-transaction/internal/provider"
	"strings"
)

const providerKeyMetadata = "smsbower_api_key_v1"

func validateAPIKey(key string) error {
	if len(key) < 8 || len(key) > 512 {
		return errors.New("API 密钥格式无效")
	}
	for _, ch := range []byte(key) {
		if ch < 33 || ch > 126 {
			return errors.New("API 密钥格式无效")
		}
	}
	return nil
}

func (a *App) credentialCipher() (cipher.AEAD, error) {
	// Separate the credential encryption key from the order-token HMAC key.
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("smsbower-api-key-at-rest-v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (a *App) encryptAPIKey(key string) (string, error) {
	gcm, err := a.credentialCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(key), []byte(providerKeyMetadata))
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (a *App) decryptAPIKey(value string) (string, error) {
	invalid := errors.New("无法读取已保存的 API 密钥，请检查数据目录及 secret.key")
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", invalid
	}
	gcm, err := a.credentialCipher()
	if err != nil || len(data) < gcm.NonceSize()+gcm.Overhead() {
		return "", invalid
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte(providerKeyMetadata))
	if err != nil || validateAPIKey(string(plain)) != nil {
		return "", invalid
	}
	return string(plain), nil
}

func (a *App) initProvider() error {
	var encrypted string
	err := a.db.QueryRow("SELECT value FROM metadata WHERE key=?", providerKeyMetadata).Scan(&encrypted)
	var key string
	if err == nil {
		key, err = a.decryptAPIKey(encrypted)
	} else if err == sql.ErrNoRows {
		// Import legacy environment configuration once; saved settings take precedence.
		key = strings.TrimSpace(a.opts.APIKey)
		err = nil
		if key != "" {
			if err = validateAPIKey(key); err != nil {
				return err
			}
			encrypted, err = a.encryptAPIKey(key)
			if err == nil {
				_, err = a.db.Exec("INSERT INTO metadata(key,value) VALUES(?,?)", providerKeyMetadata, encrypted)
			}
		}
	}
	if err != nil {
		return err
	}
	a.opts.APIKey = ""
	a.providerKey = key
	a.client = a.opts.Client
	if a.client == nil {
		a.client, err = a.configuredClient(key)
	}
	return err
}

func (a *App) configuredClient(key string) (provider.Client, error) {
	if a.opts.Mode == "demo" {
		if a.opts.Client != nil {
			return a.opts.Client, nil
		}
		return provider.NewDemo(), nil
	}
	if key == "" {
		return nil, nil
	}
	return provider.NewSMSBower(a.opts.APIBase, key)
}

func (a *App) currentClient() provider.Client {
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	return a.client
}

func (a *App) currentAPIKey() string {
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	return a.providerKey
}

type settingsView struct {
	Settings
	Mode          string `json:"mode"`
	APIConfigured bool   `json:"api_configured"`
	APIBase       string `json:"api_base"`
}

func (a *App) settingsResponse(s Settings) settingsView {
	return settingsView{s, a.opts.Mode, a.currentAPIKey() != "", a.opts.APIBase}
}
