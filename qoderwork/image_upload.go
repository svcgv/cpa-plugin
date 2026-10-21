// image_upload.go implements the Qoder image-attachment flow.
//
// The upstream agent body accepts images only through
// chat_context.imageUrls, and those URLs must be ones the gateway itself
// issued: a client-supplied URL (for example a public image link from a
// Responses request) is silently ignored, and the model answers
// "I did not receive any image".
//
// So a client image URL is downloaded, then uploaded to the gateway's
// /api/v2/image/upload endpoint with the same COSY-signed request profile the
// CLI uses, and the returned URL is what goes into the body.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// maxUploadImageBytes bounds what we are willing to download and re-upload.
const maxUploadImageBytes = 12 << 20

// qoderImageUploadPath is the gateway image-upload endpoint used by the CLI.
const qoderImageUploadPath = "/algo/api/v2/image/upload"

// uploadImageBytes posts one image to the gateway and returns the URL it
// issued. dataURL is accepted as "data:image/png;base64,..." so inline
// attachments (Codex pastes) work without a round trip.
func uploadImageBytes(sa *storedAuth, data []byte, mimeType string) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("empty image")
	}
	if len(data) > maxUploadImageBytes {
		return "", fmt.Errorf("image too large: %d bytes", len(data))
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "image")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	requestID := strings.ReplaceAll(uuid.NewString(), "-", "")
	url := gatewayBaseCN + qoderImageUploadPath + "?request_id=" + requestID
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("AI-CLIENT-TIMESTAMP", fmt.Sprintf("%d", time.Now().Unix()))
	// The upload is signed with the same bodyless profile as the catalog:
	// a request-body signature would not match what the gateway hashes for a
	// multipart PUT, and it answers 101 Signature invalid.
	sess, err := cosySessionFor(sa)
	if err != nil {
		return "", fmt.Errorf("cosy sign: %w", err)
	}
	hdr, err := sess.modelListHeaders(sa.Account.UID, url)
	if err != nil {
		return "", fmt.Errorf("cosy sign: %w", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image upload status %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	return imageURLFromUploadResponse(resp.Body)
}

// imageURLFromUploadResponse extracts the hosted URL from an upload response.
// The gateway has used both {"url":...} and {"data":{"url":...}} shapes.
func imageURLFromUploadResponse(body []byte) (string, error) {
	var probe map[string]interface{}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", fmt.Errorf("image upload parse: %w", err)
	}
	for _, key := range []string{"data", "result"} {
		nested, ok := probe[key].(map[string]interface{})
		if !ok {
			continue
		}
		if u := firstNonEmptyString(nested, "url", "oss_url", "image_url", "imageUrl"); u != "" {
			return u, nil
		}
	}
	if u := firstNonEmptyString(probe, "url", "oss_url", "image_url", "imageUrl"); u != "" {
		return u, nil
	}
	return "", fmt.Errorf("image upload: no url in response")
}

// firstNonEmptyString returns the first non-empty string value among keys.
func firstNonEmptyString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// fetchImageURL downloads a client-supplied image URL so it can be re-uploaded.
func fetchImageURL(url string) ([]byte, string, error) {
	if strings.HasPrefix(url, "data:") {
		return decodeDataURL(url)
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, "", fmt.Errorf("unsupported image url scheme")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch image status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUploadImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxUploadImageBytes {
		return nil, "", fmt.Errorf("image too large")
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// decodeDataURL parses a data: URL with base64 content.
func decodeDataURL(dataURL string) ([]byte, string, error) {
	header, b64, found := strings.Cut(dataURL, ",")
	if !found {
		return nil, "", fmt.Errorf("malformed data url")
	}
	mimeType := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		// Try raw (unpadded) as well.
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(b64, "="))
		if err != nil {
			return nil, "", fmt.Errorf("data url base64: %w", err)
		}
	}
	return raw, mimeType, nil
}

// resolveClientImages turns client-supplied image references into gateway URLs.
// Failures are reported per image and never abort the whole request: the
// conversation still goes out with whatever images did resolve.
func resolveClientImages(sa *storedAuth, images []string) []string {
	var out []string
	for _, ref := range images {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		data, mimeType, err := fetchImageURL(ref)
		if err != nil {
			continue
		}
		url, err := uploadImageBytes(sa, data, mimeType)
		if err != nil || url == "" {
			continue
		}
		out = append(out, url)
	}
	return out
}
