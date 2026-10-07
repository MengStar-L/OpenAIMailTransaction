package app

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const passwordIterations = 210000

func (a *App) adminInitialized() (bool, error) {
	var exists bool
	err := a.db.QueryRow("SELECT EXISTS(SELECT 1 FROM admin_credentials WHERE id=1)").Scan(&exists)
	return exists, err
}

// Preserve existing administrators during upgrade. New installations never
// create a password file or use an environment variable to establish ownership.
func (a *App) migrateAdmin() error {
	ready, err := a.adminInitialized()
	if err != nil || ready {
		return err
	}
	data, err := os.ReadFile(filepath.Join(a.opts.DataDir, "admin.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var legacy struct {
		Salt []byte `json:"salt"`
		Hash []byte `json:"hash"`
	}
	if err = json.Unmarshal(data, &legacy); err != nil {
		return errors.New("invalid legacy admin credential file")
	}
	if len(legacy.Salt) != 16 || len(legacy.Hash) != 32 {
		return errors.New("invalid legacy admin credential file")
	}
	_, err = a.db.Exec("INSERT INTO admin_credentials(id,salt,hash,created_at) VALUES(1,?,?,?) ON CONFLICT(id) DO NOTHING", legacy.Salt, legacy.Hash, time.Now().UnixMilli())
	return err
}

func (a *App) bootstrapStatus(w http.ResponseWriter, r *http.Request) {
	ready, err := a.adminInitialized()
	if err != nil {
		a.databaseError(w, err)
		return
	}
	respond(w, 200, map[string]bool{"setup_required": !ready})
}

func (a *App) setupAdmin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password        string `json:"password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	ready, err := a.adminInitialized()
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if ready {
		fail(w, 409, "管理员已设置，请使用密码登录")
		return
	}
	n := utf8.RuneCountInString(in.Password)
	if !utf8.ValidString(in.Password) || n < 12 || n > 128 || strings.TrimSpace(in.Password) == "" {
		fail(w, 400, "密码需为 12–128 个字符")
		return
	}
	if in.Password != in.ConfirmPassword {
		fail(w, 400, "两次输入的密码不一致")
		return
	}
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		fail(w, 500, "无法创建管理员，请重试")
		return
	}
	digest, err := pbkdf2.Key(sha256.New, in.Password, salt, passwordIterations, 32)
	if err != nil {
		fail(w, 500, "无法创建管理员，请重试")
		return
	}
	// Singleton primary key makes the ownership claim atomic, even when several
	// browsers submit valid passwords concurrently. Losing requests cannot reset it.
	result, err := a.db.Exec("INSERT INTO admin_credentials(id,salt,hash,created_at) VALUES(1,?,?,?) ON CONFLICT(id) DO NOTHING", salt, digest, time.Now().UnixMilli())
	if err != nil {
		a.databaseError(w, err)
		return
	}
	nrows, err := result.RowsAffected()
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if nrows != 1 {
		fail(w, 409, "管理员已设置，请使用密码登录")
		return
	}
	a.audit("setup_admin", "1", "")
	a.startAdminSession(w, r)
	respond(w, 200, map[string]bool{"ok": true})
}

func (a *App) passwordOK(password string) (bool, error) {
	if !utf8.ValidString(password) || len(password) > 1024 {
		return false, nil
	}
	var salt, digest []byte
	err := a.db.QueryRow("SELECT salt,hash FROM admin_credentials WHERE id=1").Scan(&salt, &digest)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	actual, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(actual, digest) == 1, nil
}
