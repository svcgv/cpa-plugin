package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type cachedQoderImage struct {
	url     string
	expires time.Time
}

var qoderImageCache = struct {
	sync.Mutex
	entries map[[32]byte]cachedQoderImage
}{entries: map[[32]byte]cachedQoderImage{}}
var qoderImageSlots = make(chan struct{}, 4)
var qoderImageFlights [64]sync.Mutex

func qoderImageCenter(sa *storedAuth) string {
	if sa.region() == regionIntl {
		return "https://center.qoder.sh"
	}
	return gatewayBaseCN
}

// Publish inline image bytes using the reference connector's multipart format.
// On an unsupported or failed upload retain the original inline data URL.
func publishQoderImage(sa *storedAuth, original string) string {
	if sa == nil || !strings.HasPrefix(original, "data:image/") {
		return original
	}
	comma := strings.IndexByte(original, ',')
	if comma < 0 {
		return original
	}
	header := original[len("data:"):comma]
	if !strings.HasSuffix(strings.ToLower(header), ";base64") {
		return original
	}
	media := strings.TrimSuffix(strings.ToLower(header), ";base64")
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}[media]
	if ext == "" {
		return original
	}
	data, err := base64.StdEncoding.DecodeString(original[comma+1:])
	if err != nil || len(data) == 0 {
		return original
	}
	identity := string(sa.region()) + "\x00" + sa.Account.UID + "\x00" + media + "\x00"
	hash := sha256.New()
	_, _ = hash.Write([]byte(identity))
	_, _ = hash.Write(data)
	var key [32]byte
	copy(key[:], hash.Sum(nil))
	flight := &qoderImageFlights[int(key[0])%len(qoderImageFlights)]
	flight.Lock()
	defer flight.Unlock()
	qoderImageCache.Lock()
	cached, ok := qoderImageCache.entries[key]
	if ok && time.Now().Before(cached.expires) {
		qoderImageCache.Unlock()
		return cached.url
	}
	if ok {
		delete(qoderImageCache.entries, key)
	}
	qoderImageCache.Unlock()
	qoderImageSlots <- struct{}{}
	defer func() { <-qoderImageSlots }()
	for attempt := 0; attempt < 2; attempt++ {
		published, status := uploadQoderImage(sa, data, media, ext)
		if published != "" {
			qoderImageCache.Lock()
			if len(qoderImageCache.entries) >= 512 {
				for k := range qoderImageCache.entries {
					delete(qoderImageCache.entries, k)
					break
				}
			}
			qoderImageCache.entries[key] = cachedQoderImage{published, time.Now().Add(30 * time.Minute)}
			qoderImageCache.Unlock()
			return published
		}
		if status != http.StatusUnauthorized || attempt != 0 || forcePATCredential(sa) != nil {
			break
		}
	}
	return original
}

func uploadQoderImage(sa *storedAuth, data []byte, media, extension string) (string, int) {
	boundary := "----qodercli-" + uuid.NewString()
	var body bytes.Buffer
	fmt.Fprintf(&body, "--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"image.%s\"\r\nContent-Type: %s\r\n\r\n", boundary, extension, media)
	body.Write(data)
	fmt.Fprintf(&body, "\r\n--%s--\r\n", boundary)
	endpoint := qoderImageCenter(sa) + "/algo/api/v2/image/upload?request_id=" + url.QueryEscape(uuid.NewString())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", 0
	}
	sess, err := cosySessionFor(sa)
	if err != nil {
		return "", 0
	}
	signed := strconv.Itoa(body.Len())
	headers, err := sess.headers(sa.Account.UID, signed, endpoint, "application/json", false)
	if err != nil {
		return "", 0
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	sum := md5.Sum([]byte(signed))
	sigPath, err := cosySigPath(endpoint)
	if err != nil {
		return "", 0
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Cosy-Bodyhash", fmt.Sprintf("%x", sum))
	req.Header.Set("Cosy-Bodylength", strconv.Itoa(len(signed)))
	req.Header.Set("Cosy-Sigpath", sigPath)
	req.Header.Set("Cosy-Machinetype", "5")
	req.Header.Set("Cosy-Machinetoken", sess.MachineID)
	req.Header.Set("Cosy-Machineos", buildMachineOS())
	req.Header.Set("Cosy-Clientip", "127.0.0.1")
	req.Header.Set("Cosy-Data-Policy", "disagree")
	req.Header.Set("AI-CLIENT-TIMESTAMP", strconv.FormatInt(time.Now().Unix(), 10))
	resp, err := hostHTTPDo(req)
	if err != nil {
		return "", 0
	}
	if resp.StatusCode != 200 {
		logPlugin("warn", fmt.Sprintf("Qoder 图片上传 HTTP %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 160)))
		return "", resp.StatusCode
	}
	var payload map[string]any
	if json.Unmarshal(resp.Body, &payload) != nil {
		return "", resp.StatusCode
	}
	candidates := []any{payload["url"]}
	for _, name := range []string{"result", "data"} {
		if obj, ok := payload[name].(map[string]any); ok {
			candidates = append(candidates, obj["url"], obj["oss_url"])
		}
	}
	for _, candidate := range candidates {
		if value, ok := candidate.(string); ok {
			parsed, err := url.Parse(strings.TrimSpace(value))
			if err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" {
				return parsed.String(), resp.StatusCode
			}
		}
	}
	return "", resp.StatusCode
}
