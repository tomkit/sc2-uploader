package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const defaultServer = "https://www.starcraft2.ai"

func serverBase() string {
	if s := strings.TrimRight(os.Getenv("SC2_API_BASE"), "/"); s != "" {
		return s
	}
	return defaultServer
}

func userAgent() string {
	return fmt.Sprintf("sc2-uploader/%s (%s; %s)", version, runtime.GOOS, runtime.GOARCH)
}

var httpClient = &http.Client{Timeout: 2 * time.Minute}

// UploadResult is the site's `?summary=1` answer.
type UploadResult struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Map       string `json:"map"`
	GameType  string `json:"gameType"`
	Duplicate bool   `json:"duplicate"`
	Players   []struct {
		Name   string `json:"name"`
		Race   string `json:"race"`
		Result string `json:"result"`
	} `json:"players"`
}

// Errors the upload loop treats differently.
type RetryError struct {
	After  time.Duration
	Reason string
}

func (e *RetryError) Error() string { return e.Reason }

type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return e.Reason }

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// looksLikeReplay mirrors the server's check: every SC2 replay is an MPQ
// archive. A half-written file fails it and is retried later rather than
// sent and rejected.
func looksLikeReplay(data []byte) bool {
	return len(data) >= 64 && bytes.HasPrefix(data, []byte("MPQ")) && (data[3] == 0x1a || data[3] == 0x1b)
}

// upload sends one replay anonymously. The site needs no account for this;
// linking only attributes the upload afterwards (claim).
func upload(path string, data []byte) (*UploadResult, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("replay", filepath.Base(path))
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", serverBase()+"/api/parse?summary=1", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "application/json")
	req.ContentLength = int64(body.Len())

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, &RetryError{After: time.Minute, Reason: "network error: " + err.Error()}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	isJSON := strings.Contains(res.Header.Get("Content-Type"), "json")

	switch {
	case res.StatusCode == 200 && isJSON:
		var out UploadResult
		if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
			return nil, &RetryError{After: 5 * time.Minute, Reason: "unexpected response from the site"}
		}
		return &out, nil
	case res.StatusCode == 429:
		after := time.Minute
		if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s > 0 {
			after = time.Duration(s) * time.Second
		}
		return nil, &RetryError{After: after, Reason: "rate limited by the site: " + jsonError(raw)}
	case (res.StatusCode == 403 || res.StatusCode == 503) && !isJSON:
		// An HTML page instead of JSON: the site's bot protection stopped
		// the request. Usually a VPN or a datacenter network.
		return nil, &RetryError{
			After:  30 * time.Minute,
			Reason: "blocked by the site's bot protection (an HTML page came back). If you use a VPN, try without it",
		}
	case res.StatusCode == 400 || res.StatusCode == 413 || res.StatusCode == 422:
		return nil, &RejectedError{Reason: fmt.Sprintf("the site rejected it (%d): %s", res.StatusCode, jsonError(raw))}
	default:
		return nil, &RetryError{After: 5 * time.Minute, Reason: fmt.Sprintf("site error %d: %s", res.StatusCode, jsonError(raw))}
	}
}

func jsonError(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

type existing struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// alreadyUploaded asks the site which of these files it already has
// (anyone's upload counts), so backfill never re-uploads them.
func alreadyUploaded(hashes []string) (map[string]existing, error) {
	payload, _ := json.Marshal(map[string][]string{"hashes": hashes})
	req, _ := http.NewRequest("POST", serverBase()+"/api/replays/exists", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "json") {
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, jsonError(raw))
	}
	var out struct {
		Found map[string]existing `json:"found"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Found == nil {
		out.Found = map[string]existing{}
	}
	return out.Found, nil
}

// claim attributes an upload to the linked account (proof: the file hash).
func claim(token, replayID, fileHash string) error {
	payload, _ := json.Marshal(map[string]string{"id": replayID, "fileHash": fileHash})
	req, _ := http.NewRequest("POST", serverBase()+"/api/replays/claim", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent())
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == 401 {
		return errors.New("account link expired or revoked; run `sc2-uploader link` again")
	}
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("claim failed (%d): %s", res.StatusCode, jsonError(b))
	}
	return nil
}

// ---- Account link: OAuth 2.0 device authorization grant (RFC 8628) ----

type deviceStart struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func postForm(path string, form url.Values) (int, []byte, error) {
	req, _ := http.NewRequest("POST", serverBase()+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
	res, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	return res.StatusCode, b, nil
}

// linkAccount runs the device flow: prints where to go and the code, then
// polls until the user approves on any device. Returns the token.
func linkAccount(out io.Writer) (string, time.Time, error) {
	host, _ := os.Hostname()
	if len(host) > 40 {
		host = host[:40]
	}
	status, body, err := postForm("/api/mcp-auth/device", url.Values{"client_name": {"SC2 Uploader on " + host}})
	if err != nil {
		return "", time.Time{}, err
	}
	var start deviceStart
	if status != 200 || json.Unmarshal(body, &start) != nil || start.DeviceCode == "" {
		return "", time.Time{}, fmt.Errorf("couldn't start sign-in (%d): %s", status, jsonError(body))
	}
	base, _ := url.Parse(serverBase())
	if v, err := url.Parse(start.VerificationURI); err != nil || v.Host != base.Host {
		return "", time.Time{}, errors.New("the site returned an unexpected sign-in address")
	}
	fmt.Fprintf(out, "\nOn your phone or computer, go to:\n\n    %s\n\nand enter the code:\n\n    %s\n\nOnly enter it because you asked to link this uploader. Waiting…\n",
		start.VerificationURI, start.UserCode)

	interval := time.Duration(max(start.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(max(start.ExpiresIn, 60)) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		status, body, err := postForm("/api/mcp-auth/token", url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {start.DeviceCode},
		})
		if err != nil {
			continue
		}
		var tok struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
			Error       string `json:"error"`
		}
		_ = json.Unmarshal(body, &tok)
		if status == 200 && tok.AccessToken != "" {
			return tok.AccessToken, time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second), nil
		}
		switch tok.Error {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return "", time.Time{}, errors.New("sign-in was cancelled")
		case "expired_token":
			return "", time.Time{}, errors.New("the code expired; run `sc2-uploader link` again")
		default:
			return "", time.Time{}, fmt.Errorf("sign-in failed: %s", jsonError(body))
		}
	}
	return "", time.Time{}, errors.New("the code expired; run `sc2-uploader link` again")
}

func revoke(token string) {
	_, _, _ = postForm("/api/mcp-auth/revoke", url.Values{"token": {token}})
}
