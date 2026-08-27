package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	zlog "github.com/rs/zerolog/log"
)

const (
	awsAlgorithm      = "AWS4-HMAC-SHA256"
	emptyPayloadHash  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	unsignedPayload   = "UNSIGNED-PAYLOAD"
	streamingPayload  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	requestTerminator = "aws4_request"
	maxClockSkew      = 15 * time.Minute
)

// sigv4Middleware validates the SigV4 signature of every request before it
// reaches the S3 handler, and verifies PUT payload hashes while the handler
// reads the body.
func sigv4Middleware(next http.Handler, accessKey, secretKey string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := verifySigV4(r, accessKey, secretKey); err != nil {
			cred, signed := authFields(r)
			zlog.Error().
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Str("credential", cred).
				Str("signed_headers", signed).
				Err(err).
				Msg("sigv4 verification failed")
			writeSigError(w, http.StatusForbidden, "SignatureDoesNotMatch",
				err.Error(), r.URL.Path)
			return
		}
		if payloadHash := r.Header.Get("x-amz-content-sha256"); payloadHash != "" &&
			payloadHash != unsignedPayload && len(payloadHash) == 64 {
			r.Body = io.NopCloser(&hashVerifyingReader{
				r:        r.Body,
				h:        sha256.New(),
				expected: payloadHash,
			})
		}
		next.ServeHTTP(w, r)
	})
}

// authFields returns the Credential and SignedHeaders values from a SigV4
// Authorization header for logging; the signature itself is never logged.
func authFields(r *http.Request) (cred, signed string) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, awsAlgorithm+" ") {
		return "", ""
	}
	for _, kv := range strings.Split(strings.TrimPrefix(auth, awsAlgorithm+" "), ", ") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "Credential":
			cred = v
		case "SignedHeaders":
			signed = v
		}
	}
	return cred, signed
}

type sigError struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
}

func writeSigError(w http.ResponseWriter, status int, code, message, resource string) {
	data, _ := xml.Marshal(sigError{
		Code:      code,
		Message:   message,
		Resource:  resource,
		RequestID: "overlay-s3",
	})
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(data)
}

// errPayloadHashMismatch is returned by hashVerifyingReader when the streamed
// payload does not match the signed hash.
var errPayloadHashMismatch = errors.New("payload hash mismatch")

// hashVerifyingReader computes the payload hash while streaming and fails
// with io.EOF replaced by errPayloadHashMismatch when it does not match the
// signed value.
type hashVerifyingReader struct {
	r        io.Reader
	h        hash.Hash
	expected string
}

func (v *hashVerifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
	}
	if err == io.EOF && hex.EncodeToString(v.h.Sum(nil)) != v.expected {
		zlog.Error().Msg("payload hash mismatch")
		return n, errPayloadHashMismatch
	}
	return n, err
}

// verifySigV4 checks the request's SigV4 Authorization header against the
// configured key. Chunked (STREAMING-*) payload signing and presigned URLs
// are rejected. The body itself is not read here; the caller validates the
// payload hash for PUTs when the header carries a concrete hash.
func verifySigV4(r *http.Request, accessKey, secretKey string) error {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return fmt.Errorf("missing Authorization header")
	}
	algo, rest, ok := strings.Cut(auth, " ")
	if !ok || algo != awsAlgorithm {
		return fmt.Errorf("unsupported authorization scheme")
	}
	fields := map[string]string{}
	// minio-go's streaming signer emits "Credential=...,SignedHeaders=...,Signature=..."
	// without spaces after the commas, so split on "," and trim instead of ", "
	for _, kv := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			return fmt.Errorf("malformed authorization header")
		}
		fields[k] = v
	}
	credential, ok := fields["Credential"]
	if !ok {
		return fmt.Errorf("missing Credential")
	}
	signedHeadersStr, ok := fields["SignedHeaders"]
	if !ok {
		return fmt.Errorf("missing SignedHeaders")
	}
	signature, ok := fields["Signature"]
	if !ok {
		return fmt.Errorf("missing Signature")
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 || credParts[4] != requestTerminator {
		return fmt.Errorf("malformed Credential")
	}
	keyID, date, region, service := credParts[0], credParts[1], credParts[2], credParts[3]
	if keyID != accessKey {
		return fmt.Errorf("access key mismatch")
	}

	if r.URL.Query().Get("X-Amz-Signature") != "" {
		return fmt.Errorf("presigned URLs not supported")
	}

	amzDate := r.Header.Get("x-amz-date")
	if amzDate == "" {
		return fmt.Errorf("missing x-amz-date")
	}
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return fmt.Errorf("invalid x-amz-date")
	}
	if skew := time.Since(t); skew > maxClockSkew || skew < -maxClockSkew {
		return fmt.Errorf("request time outside acceptable skew")
	}

	payloadHash := r.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		payloadHash = unsignedPayload
	}
	if strings.HasPrefix(payloadHash, "STREAMING-") && payloadHash != streamingPayload {
		return fmt.Errorf("unsupported streaming payload signing %q", payloadHash)
	}

	signedHeaders := strings.Split(signedHeadersStr, ";")
	for i := range signedHeaders {
		signedHeaders[i] = strings.ToLower(strings.TrimSpace(signedHeaders[i]))
	}
	sort.Strings(signedHeaders)
	hasHost := false
	var canonicalHeaders strings.Builder
	for _, h := range signedHeaders {
		var v string
		switch h {
		case "host":
			v = r.Host
			hasHost = true
		case "content-length":
			v = strconv.FormatInt(r.ContentLength, 10)
		default:
			v = r.Header.Get(h)
		}
		if v == "" {
			return fmt.Errorf("missing signed header %s", h)
		}
		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.Join(strings.Fields(v), " "))
		canonicalHeaders.WriteByte('\n')
	}
	if !hasHost {
		return fmt.Errorf("signed headers must include host")
	}

	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI(r.URL),
		canonicalQueryString(r.URL),
		canonicalHeaders.String(),
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")

	scope := date + "/" + region + "/" + service + "/" + requestTerminator
	stringToSign := strings.Join([]string{
		awsAlgorithm,
		amzDate,
		scope,
		hexSHA256(canonicalRequest),
	}, "\n")

	expected := hexHMAC(signingKey(secretKey, date, region, service), stringToSign)
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(signature))) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// canonicalURI percent-encodes each path segment per the SigV4 spec,
// preserving '/' separators.
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	segments := strings.Split(u.Path, "/")
	for i, seg := range segments {
		segments[i] = awsEncode(seg)
	}
	return strings.Join(segments, "/")
}

func canonicalQueryString(u *url.URL) string {
	vals := u.Query()
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(awsEncode(k))
		sb.WriteByte('=')
		vs := vals[k]
		sort.Strings(vs)
		sb.WriteString(awsEncode(vs[0]))
	}
	return sb.String()
}

// awsEncode implements the SigV4 percent-encoding: unreserved characters are
// kept, everything else is upper-case hex-escaped, spaces become %20.
func awsEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' ||
			c == '.' || c == '~' {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('%')
			sb.WriteByte(hexDigits[c>>4])
			sb.WriteByte(hexDigits[c&0xf])
		}
	}
	return sb.String()
}

func hexSHA256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func signingKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(requestTerminator))
}

func hexHMAC(key []byte, data string) string {
	return hex.EncodeToString(hmacSHA256(key, []byte(data)))
}
