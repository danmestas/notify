// Package awssigv4 is a stdlib-only implementation of AWS Signature Version 4
// request signing (https://docs.aws.amazon.com/general/latest/gr/signature-version-4.html).
// It exists so the SES and SNS providers can authenticate against AWS without
// pulling in aws-sdk-go. Sign mutates the request in place: it computes the
// SHA256 payload hash, sets the X-Amz-Date, Host, and X-Amz-Content-Sha256
// headers, builds the canonical request and string-to-sign, derives the signing
// key via the AWS4 HMAC chain, and writes the Authorization header.
//
// The signer is verified against the published "get-vanilla" SigV4 test vector
// (see awssigv4_test.go); the signed-header set is the minimal host;x-amz-date,
// with the payload hash carried in the canonical request and surfaced as the
// X-Amz-Content-Sha256 header for the receiver.
package awssigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	algorithm  = "AWS4-HMAC-SHA256"
	terminator = "aws4_request"
	// signedHeaders is the canonical, sorted, semicolon-joined list of headers
	// this signer includes in the signature. host;x-amz-date is the minimal
	// valid set and matches the AWS get-vanilla reference vector.
	signedHeaders = "host;x-amz-date"
)

// Sign signs req in place using AWS Signature Version 4. body is the exact
// request payload (nil/empty hashes to the SHA256 of the empty string). service
// and region select the credential scope (e.g. "ses"/"us-east-1"). now is
// injected so the X-Amz-Date and credential scope are deterministic in tests.
//
// On return req carries the X-Amz-Date, Host, X-Amz-Content-Sha256, and
// Authorization headers required by AWS.
func Sign(req *http.Request, body []byte, service, region, accessKeyID, secretAccessKey string, now time.Time) error {
	if req == nil {
		return fmt.Errorf("awssigv4: nil request")
	}
	if req.URL == nil {
		return fmt.Errorf("awssigv4: request has no URL")
	}

	host := req.URL.Host
	if host == "" {
		return fmt.Errorf("awssigv4: request URL has no host")
	}

	t := now.UTC()
	amzDate := t.Format("20060102T150405Z")
	dateStamp := t.Format("20060102")

	payloadHash := hexSHA256(body)

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	// net/http transmits the Host from req.Host (or req.URL.Host); setting both
	// the field and a header keeps the canonical "host" value unambiguous.
	req.Host = host

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := req.URL.Query().Encode()

	canonicalHeaders := "host:" + host + "\n" + "x-amz-date:" + amzDate + "\n"
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := strings.Join([]string{dateStamp, region, service, terminator}, "/")
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		credentialScope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKey(secretAccessKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	authorization := fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm,
		accessKeyID,
		credentialScope,
		signedHeaders,
		signature,
	)
	req.Header.Set("Authorization", authorization)
	return nil
}

// deriveSigningKey runs the AWS4 HMAC chain:
// AWS4+secret → date → region → service → "aws4_request".
func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, terminator)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
