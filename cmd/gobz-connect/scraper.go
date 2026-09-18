package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// AppSecrets holds the scraped Qobuz app credentials.
type AppSecrets struct {
	AppID   string
	Secrets map[string]string // timezone -> secret
}

// FetchAppSecrets scrapes the Qobuz web player to find the app_id and app_secret.
func FetchAppSecrets() (*AppSecrets, error) {
	entrypoints := []string{
		"https://play.qobuz.com/login",
		"https://play.qobuz.com/",
	}
	for _, ep := range entrypoints {
		log.Printf("scraper: trying %s", ep)
		s, err := tryQobuzFromBundles(ep)
		if err != nil {
			log.Printf("scraper: %s failed: %v", ep, err)
			continue
		}
		if len(s.Secrets) > 0 {
			return s, nil
		}
	}
	return nil, fmt.Errorf("could not scrape Qobuz app secrets")
}

func tryQobuzFromBundles(entryURL string) (*AppSecrets, error) {
	html, err := fetchURL(entryURL)
	if err != nil {
		return nil, err
	}
	scripts := extractPlayerScriptSrcs(html, entryURL)
	secrets := &AppSecrets{Secrets: make(map[string]string)}

	limit := len(scripts)
	if limit > 12 {
		limit = 12
	}
	for i := 0; i < limit; i++ {
		jsURL := scripts[i]
		if !strings.Contains(jsURL, "play.qobuz.com") {
			continue
		}
		log.Printf("scraper: scanning JS [%d/%d]: %s", i+1, limit, jsURL)
		body, err := fetchURL(jsURL)
		if err != nil {
			log.Printf("scraper: fetch JS failed: %v", err)
			continue
		}
		scanAppID(body, secrets)
		scanSeeds(body, secrets)
		if len(secrets.Secrets) > 0 && secrets.AppID != "" {
			return secrets, nil
		}
	}
	return secrets, nil
}

func fetchURL(rawURL string) (string, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/124 Safari/537.36")
	req.Header.Set("Referer", "https://play.qobuz.com/")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

var reAppID = regexp.MustCompile(`production:\{api:\{appId:"(\d{9})"`)

func scanAppID(hay string, s *AppSecrets) {
	if m := reAppID.FindStringSubmatch(hay); len(m) == 2 {
		s.AppID = m[1]
		log.Printf("scraper: found appId=%s", s.AppID)
	}
}

var reSeed = regexp.MustCompile(`\.initialSeed\("([^"]+)",window\.utimezone\.([a-z]+)\)`)

func scanSeeds(hay string, s *AppSecrets) {
	seeds := make(map[string]string)
	for _, m := range reSeed.FindAllStringSubmatch(hay, -1) {
		seed := m[1]
		tz := strings.Title(m[2]) // e.g. "europe" -> "Europe"
		seeds[tz] = seed
		log.Printf("scraper: found seed for %s", tz)
	}
	for tz, seed := range seeds {
		deriveSecret(hay, tz, seed, s)
	}
}

var reInfo = regexp.MustCompile(`info:"([^"]*)"`)
var reExtras = regexp.MustCompile(`extras:"([^"]*)"`)

func deriveSecret(hay, tz, seed string, s *AppSecrets) {
	anchor := "/" + tz
	idx := strings.Index(hay, anchor)
	if idx < 0 {
		return
	}
	snippet := hay[idx:]
	mInfo := reInfo.FindStringSubmatch(snippet)
	mExtras := reExtras.FindStringSubmatch(snippet)
	if mInfo == nil || mExtras == nil {
		return
	}
	info := mInfo[1]
	extras := mExtras[1]

	combined := seed + info + extras
	if len(combined) <= 44 {
		return
	}
	enc := combined[:len(combined)-44]
	dec, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		// Try standard base64
		dec, err = base64.URLEncoding.DecodeString(enc)
		if err != nil {
			return
		}
	}
	secret := string(dec)
	s.Secrets[tz] = secret
	log.Printf("scraper: derived secret for %s", tz)
}

func extractScriptSrcs(html string) []string {
	var out []string
	// <script src="...">
	reSrc := regexp.MustCompile(`(?i)<script[^>]+src="([^"]+\.js[^"]*)"`)
	for _, m := range reSrc.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	// <link rel="preload" as="script" href="...">
	reLink := regexp.MustCompile(`(?i)<link[^>]+rel="preload"[^>]+as="script"[^>]+href="([^"]+\.js[^"]*)"`)
	for _, m := range reLink.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

func extractPlayerScriptSrcs(html, baseURL string) []string {
	srcs := extractScriptSrcs(html)
	var out []string
	seen := make(map[string]bool)
	for _, s := range srcs {
		abs := absolutize(baseURL, s)
		if !seen[abs] && isPlayerAsset(abs) {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	return out
}

func isPlayerAsset(u string) bool {
	if !strings.Contains(u, "play.qobuz.com") {
		return false
	}
	return strings.Contains(u, "/resources/") ||
		strings.Contains(u, "/_next/") ||
		strings.HasSuffix(strings.Split(u, "?")[0], ".js")
}

func absolutize(base, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	// strip path from base
	cut := strings.LastIndex(base, "/")
	if strings.HasPrefix(ref, "/") {
		// find scheme+host
		after := strings.Index(base, "://")
		if after < 0 {
			return ref
		}
		host := base[after+3:]
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		return base[:after+3] + host + ref
	}
	if cut > 8 {
		return base[:cut+1] + ref
	}
	return base + "/" + ref
}
