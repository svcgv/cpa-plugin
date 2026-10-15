// encoding.go implements Qoder's custom base64 variant used for request bodies.
//
// Algorithm (mirrors /tmp/cpa-plugin/qoderwork/reference_impl.py):
//  1. std = standard base64(plain)
//  2. rearranged = std[n-a:] + std[a:n-a] + std[:a] where a = n/3
//  3. map each char from std alphabet to custom alphabet; '=' → '$'
package main

import (
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	qoderCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	qoderStdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	qoderCustomPad      = '$'
)

var (
	qoderS2C [128]byte // std char → custom char (0 = invalid)
	qoderC2S [128]byte // custom char → std char (0 = invalid)
)

func init() {
	for i := 0; i < 64; i++ {
		qoderS2C[qoderStdAlphabet[i]] = qoderCustomAlphabet[i]
		qoderC2S[qoderCustomAlphabet[i]] = qoderStdAlphabet[i]
	}
	qoderS2C['='] = qoderCustomPad
	qoderC2S[qoderCustomPad] = '='
}

// qoderEncode encodes plaintext to Qoder's custom base64 variant.
func qoderEncode(plain []byte) string {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	var sb strings.Builder
	sb.Grow(n)
	for i := 0; i < n; i++ {
		c := rearranged[i]
		if c >= 128 || qoderS2C[c] == 0 {
			// Should never happen for valid base64 input.
			sb.WriteByte(c)
			continue
		}
		sb.WriteByte(qoderS2C[c])
	}
	return sb.String()
}

// qoderDecode reverses qoderEncode. It is used when a gateway build answers
// with Encode=1 and returns the custom base64 alphabet instead of plain JSON.
func qoderDecode(encoded []byte) ([]byte, error) {
	n := len(encoded)
	if n == 0 {
		return nil, fmt.Errorf("empty qoder payload")
	}
	std := make([]byte, n)
	for i, c := range encoded {
		if c >= 128 || qoderC2S[c] == 0 {
			return nil, fmt.Errorf("invalid qoder payload byte 0x%02x at %d", c, i)
		}
		std[i] = qoderC2S[c]
	}
	a := n / 3
	out := make([]byte, n)
	// Encode rearranges std as std[n-a:] + std[a:n-a] + std[:a]; reverse it:
	//   segment 1 (last a bytes of std) is out[n-a:]
	//   segment 2 (middle)              is out[a:n-a]
	//   segment 3 (first a bytes)       is out[:a]
	copy(out[:a], std[n-a:])
	copy(out[a:n-a], std[a:n-a])
	copy(out[n-a:], std[:a])
	return base64.StdEncoding.DecodeString(string(out))
}
