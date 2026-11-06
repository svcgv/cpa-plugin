package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestImageUploadUsesLengthSignatureAndCaches(t *testing.T) {
	qoderImageCache.Lock()
	qoderImageCache.entries = map[[32]byte]cachedQoderImage{}
	qoderImageCache.Unlock()
	calls := 0
	qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPut || r.URL.Path != "/algo/api/v2/image/upload" || r.Header.Get("Cosy-Sigpath") != "/api/v2/image/upload" {
			t.Errorf("wrong image route or signature path")
		}
		body, _ := io.ReadAll(r.Body)
		signed := strconv.Itoa(len(body))
		sum := md5.Sum([]byte(signed))
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Error(err)
			return
		}
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		part, err := reader.NextPart()
		if err != nil {
			t.Error(err)
			return
		}
		data, _ := io.ReadAll(part)
		if part.FormName() != "file" || part.FileName() != "image.png" || string(data) != "image-bytes" {
			t.Error("incorrect multipart image")
		}
		if r.Header.Get("Cosy-Bodylength") != strconv.Itoa(len(signed)) || r.Header.Get("Cosy-Bodyhash") != fmt.Sprintf("%x", sum) {
			t.Errorf("signed body should be the decimal content length")
		}
		_, _ = w.Write([]byte(`{"data":{"oss_url":"https://images.example.test/object"}}`))
	})
	sa := &storedAuth{Auth: storedTokens{AccessToken: "jt-test", PersonalToken: "pt-test", Region: "cn"}, Account: storedAccount{UID: "image-test"}}
	inline := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	if got := publishQoderImage(sa, inline); got != "https://images.example.test/object" {
		t.Fatalf("unexpected image result %q", got)
	}
	if got := publishQoderImage(sa, inline); got != "https://images.example.test/object" || calls != 1 {
		t.Fatalf("image cache missed: %d", calls)
	}
}

func TestImageUploadFallsBackToInlineOnError(t *testing.T) {
	qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	sa := &storedAuth{Auth: storedTokens{AccessToken: "jt-test", PersonalToken: "pt-test", Region: "cn"}, Account: storedAccount{UID: "image-fallback"}}
	inline := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("fallback-image"))
	if got := publishQoderImage(sa, inline); got != inline {
		t.Fatal("failed upload should preserve inline data")
	}
}

func TestLiveImagePublication(t *testing.T) {
	path := os.Getenv("QODER_TEST_PAT_PATH")
	if path == "" || os.Getenv("QODER_TEST_IMAGE_UPLOAD") != "1" {
		t.Skip("live image publication not requested")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read PAT")
	}
	pat := strings.TrimSpace(string(data))
	tok, err := exchangePATForJobToken(pat)
	if err != nil {
		t.Fatal("PAT exchange failed")
	}
	ui, err := fetchUserInfo(tok.Token)
	if err != nil {
		t.Fatal("userinfo failed")
	}
	sa := buildStoredAuthFromJobToken(pat, tok, ui)
	inline := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/aN8AAAAASUVORK5CYII="
	result := publishQoderImage(sa, inline)
	if result == inline {
		png, _ := base64.StdEncoding.DecodeString(strings.SplitN(inline, ",", 2)[1])
		_, status := uploadQoderImage(sa, png, "image/png", "png")
		t.Fatalf("upstream image publication fell back to inline (HTTP %d)", status)
	}
	t.Log("synthetic image published successfully")
}
