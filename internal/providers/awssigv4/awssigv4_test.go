package awssigv4

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSign_GetVanillaKnownVector pins the signer against the canonical AWS
// Signature Version 4 "get-vanilla" worked example (the documented test vector
// from the AWS SigV4 test suite):
//
//	method:    GET
//	host:      example.amazonaws.com
//	path:      /
//	X-Amz-Date 20150830T123600Z
//	accessKey  AKIDEXAMPLE
//	secret     wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY
//	region     us-east-1
//	service    service
//
// The expected Authorization header — Signature
// 5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31 — is the
// published value for this vector; reproducing it EXACTLY proves the canonical
// request, string-to-sign, signing-key derivation, and final HMAC are correct.
func TestSign_GetVanillaKnownVector(t *testing.T) {
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if err := Sign(req, nil, "service", "us-east-1",
		"AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", now); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	const wantAuth = "AWS4-HMAC-SHA256 " +
		"Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"

	if got := req.Header.Get("Authorization"); got != wantAuth {
		t.Errorf("Authorization mismatch:\n got = %q\nwant = %q", got, wantAuth)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q, want 20150830T123600Z", got)
	}
	// Host must be set on the request (net/http uses req.Host, not the header).
	if req.Host != "example.amazonaws.com" && req.Header.Get("Host") != "example.amazonaws.com" {
		t.Errorf("Host not set: req.Host=%q header=%q", req.Host, req.Header.Get("Host"))
	}
}

// TestSign_BodyHash verifies the payload SHA256 is computed over the supplied
// body and surfaced in X-Amz-Content-Sha256 (and folded into the signature via
// the canonical request). The hash of the literal bytes "hello" is a known
// SHA256 constant.
func TestSign_BodyHash(t *testing.T) {
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	req, err := http.NewRequest(http.MethodPost, "https://example.amazonaws.com/", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	body := []byte("hello")
	if err := Sign(req, body, "service", "us-east-1",
		"AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", now); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	const helloSHA = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != helloSHA {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got, helloSHA)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request") {
		t.Errorf("Authorization prefix unexpected: %q", auth)
	}
	if !strings.Contains(auth, "Signature=") {
		t.Errorf("Authorization missing Signature: %q", auth)
	}
}

// TestSign_EmptyBodyHash verifies a nil body hashes to the SHA256 of the empty
// string (the documented UNSIGNED-PAYLOAD alternative is NOT used here).
func TestSign_EmptyBodyHash(t *testing.T) {
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err := Sign(req, nil, "service", "us-east-1", "AKIDEXAMPLE", "secret", now); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != emptySHA {
		t.Errorf("X-Amz-Content-Sha256 (empty) = %q, want %q", got, emptySHA)
	}
}
