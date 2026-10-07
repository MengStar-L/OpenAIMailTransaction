package app

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAdminCDKExportMatchesBatchAndRequiresAuthentication(t *testing.T) {
	a := newTestApp(t, &testProvider{})
	h := a.Handler()
	cookie := adminCookie(t, a)
	created := apiRequest(h, http.MethodPost, "/api/admin/cdks", map[string]any{"kind": "phone", "quantity": 2, "note": "CSV export", "expires_days": 7, "max_attempts": 2}, "", cookie, "http://example.test")
	if created.Code != http.StatusOK {
		t.Fatalf("issue HTTP %d: %s", created.Code, created.Body.String())
	}
	var batch struct {
		Codes     []string `json:"codes"`
		BatchID   string   `json:"batch_id"`
		ExportURL string   `json:"export_url"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Codes) != 2 || batch.BatchID == "" || !strings.HasPrefix(batch.ExportURL, "/api/admin/exports/") {
		t.Fatalf("invalid batch response: %+v", batch)
	}

	unauthenticated := apiRequest(h, http.MethodGet, batch.ExportURL, nil, "", nil, "")
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated download HTTP %d", unauthenticated.Code)
	}
	for _, code := range batch.Codes {
		if bytes.Contains(unauthenticated.Body.Bytes(), []byte(code)) {
			t.Fatal("unauthenticated download leaked a voucher")
		}
	}
	download := apiRequest(h, http.MethodGet, batch.ExportURL, nil, "", cookie, "")
	if download.Code != http.StatusOK {
		t.Fatalf("download HTTP %d: %s", download.Code, download.Body.String())
	}
	if download.Header().Get("Content-Type") != "text/csv; charset=utf-8" || download.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unsafe CSV headers: %v", download.Header())
	}
	disposition, params, err := mime.ParseMediaType(download.Header().Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || params["filename"] != "CDK-"+batch.BatchID+".csv" {
		t.Fatalf("invalid attachment filename: %q (%v)", download.Header().Get("Content-Disposition"), err)
	}
	bom := []byte{0xef, 0xbb, 0xbf}
	if !bytes.HasPrefix(download.Body.Bytes(), bom) {
		t.Fatal("CSV is missing the UTF-8 BOM required for Chinese Excel labels")
	}
	rows, err := csv.NewReader(bytes.NewReader(download.Body.Bytes()[len(bom):])).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || len(rows[0]) != 2 || rows[0][0] != "CDK" || rows[0][1] != "批次" {
		t.Fatalf("CSV must have a header and exactly two voucher rows: %#v", rows)
	}
	for i, code := range batch.Codes {
		if len(rows[i+1]) != 2 || rows[i+1][0] != code || rows[i+1][1] != batch.BatchID {
			t.Fatalf("CSV row does not match issuance: %#v", rows[i+1])
		}
		stored, err := scanCDK(a.db.QueryRow("SELECT "+cdkColumns+" FROM cdks WHERE hash=?", hash(code)))
		if err != nil {
			t.Fatal(err)
		}
		if stored.Hash != hash(code) || stored.MaskedCode == code {
			t.Fatal("CSV export changed hashed/masked voucher storage")
		}
	}
}

func TestAdminCDKExportExpiredAndUnknownTokens(t *testing.T) {
	a := newTestApp(t, &testProvider{})
	h := a.Handler()
	cookie := adminCookie(t, a)
	const token = "expired-export-token"
	a.exports.Store(token, codeExport{Codes: []string{"PRIVATE-TEST-CODE"}, BatchID: "TEST", ExpiresAt: time.Now().Add(-time.Minute)})
	expired := apiRequest(h, http.MethodGet, "/api/admin/exports/"+token, nil, "", cookie, "")
	if expired.Code != http.StatusGone {
		t.Fatalf("expired export HTTP %d: %s", expired.Code, expired.Body.String())
	}
	if bytes.Contains(expired.Body.Bytes(), []byte("PRIVATE-TEST-CODE")) {
		t.Fatal("expired export leaked plaintext voucher")
	}
	if _, present := a.exports.Load(token); present {
		t.Fatal("expired plaintext export remains cached after retrieval")
	}
	for _, id := range []string{token, "unknown-export-token"} {
		missing := apiRequest(h, http.MethodGet, "/api/admin/exports/"+id, nil, "", cookie, "")
		if missing.Code != http.StatusNotFound {
			t.Fatalf("unknown token %q HTTP %d", id, missing.Code)
		}
	}
}
