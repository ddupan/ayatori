package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
)

func TestFetchImage(t *testing.T) {
	const isoPath, diskPath = "/iso", "/disk"
	iso := make([]byte, isoMagicOffset+64)
	copy(iso[isoMagicOffset:], "CD001")
	disk := []byte("QFI\xfb not an iso")
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case isoPath:
			_, _ = w.Write(iso)
		case diskPath:
			_, _ = w.Write(disk)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name, path, sha, wantErr string
		kind                     computev1alpha1.ImageType
	}{
		{name: "iso", path: isoPath, sha: sum(iso), kind: computev1alpha1.ImageISO},
		{name: "disk", path: diskPath, sha: sum(disk), kind: computev1alpha1.ImageDisk},
		{name: "wrong checksum", path: diskPath, sha: sum(iso), kind: computev1alpha1.ImageDisk, wantErr: "checksum mismatch"},
		{name: "iso declared as disk", path: isoPath, sha: sum(iso), kind: computev1alpha1.ImageDisk, wantErr: "does not match"},
		{name: "disk declared as iso", path: diskPath, sha: sum(disk), kind: computev1alpha1.ImageISO, wantErr: "does not match"},
		{name: "http error", path: "/gone", sha: sum(disk), kind: computev1alpha1.ImageDisk, wantErr: "404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := filepath.Join(t.TempDir(), "image")
			err := fetchImage(t.Context(), computev1alpha1.ImageHTTPSource{URL: srv.URL + tc.path, SHA256: tc.sha}, tc.kind, tmp)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(tmp)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[string][]byte{isoPath: iso, diskPath: disk}[tc.path]; !bytes.Equal(got, want) {
				t.Fatalf("file content differs: %d bytes", len(got))
			}
		})
	}
}
