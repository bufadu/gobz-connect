package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// md5Sig computes the Qobuz API request signature.
// Signature = MD5( object + method + sorted(key+value for each param) + ts + secret )
func md5Sig(object, method string, params [][2]string, tsText, appSecret string) string {
	// Sort params by key
	sorted := make([][2]string, len(params))
	copy(sorted, params)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0] < sorted[j][0] })

	var sb strings.Builder
	sb.WriteString(object)
	sb.WriteString(method)
	for _, kv := range sorted {
		sb.WriteString(kv[0])
		sb.WriteString(kv[1])
	}
	sb.WriteString(tsText)
	sb.WriteString(appSecret)

	sum := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// nowSecText returns the current Unix time as a string with the given number of decimals.
func nowSecText(decimals int) string {
	t := time.Now().Unix()
	if decimals == 0 {
		return fmt.Sprintf("%d", t)
	}
	s := fmt.Sprintf("%d.000000", t)
	return s[:len(s)-6+decimals]
}

// buildQuery encodes params as a URL query string (key=value&...).
func buildQuery(params [][2]string) string {
	parts := make([]string, len(params))
	for i, kv := range params {
		parts[i] = url.QueryEscape(kv[0]) + "=" + url.QueryEscape(kv[1])
	}
	return strings.Join(parts, "&")
}
