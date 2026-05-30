package llmrouter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// s3GetObjectFromEnv fetches an S3 object's bytes using the same AWS env
// the rest of talk-go uses (ACCESS_KEY_ID / SECRET_KEY_ID /
// AWS_MAIN_REGION|AWS_REGION / AWS_BUCKET_NAME). Mirrors Python's
// _load_s3_json_from_env(Bucket=AWS_BUCKET_NAME, Key=...). Kept
// self-contained so llmrouter needs no dependency on the disha package.
func s3GetObjectFromEnv(ctx context.Context, objectKey string) ([]byte, error) {
	accessKey := strings.TrimSpace(os.Getenv("ACCESS_KEY_ID"))
	secretKey := strings.TrimSpace(os.Getenv("SECRET_KEY_ID"))
	region := firstNonEmptyEnv("AWS_MAIN_REGION", "AWS_REGION")
	bucket := strings.TrimSpace(os.Getenv("AWS_BUCKET_NAME"))
	if accessKey == "" || secretKey == "" || region == "" || bucket == "" {
		return nil, fmt.Errorf("llmrouter: incomplete AWS env for S3 fetch (need ACCESS_KEY_ID/SECRET_KEY_ID/AWS_MAIN_REGION/AWS_BUCKET_NAME)")
	}
	return s3GetObject(ctx, &http.Client{Timeout: 15 * time.Second}, accessKey, secretKey, region, bucket, objectKey)
}

// s3GetObject performs a SigV4-signed GET and returns the object body.
func s3GetObject(ctx context.Context, httpClient *http.Client, accessKey, secretKey, region, bucket, objectKey string) ([]byte, error) {
	objectKey = strings.TrimLeft(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return nil, fmt.Errorf("llmrouter: S3 object key is required")
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	encodedKey := s3CanonicalURI(objectKey)
	host := fmt.Sprintf("%s.s3.%s.amazonaws.com", bucket, region)
	endpoint := fmt.Sprintf("https://%s%s", host, encodedKey)
	payloadHash := s3SHA256Hex(nil)

	headers := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	signedNames := s3SortedHeaderNames(headers)
	canonicalHeaders := s3CanonicalHeaders(headers, signedNames)
	signedHeaders := strings.Join(signedNames, ";")
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		encodedKey,
		"",
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		s3SHA256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(s3HMACSHA256(s3SigningKey(secretKey, dateStamp, region), []byte(stringToSign)))
	authorization := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("llmrouter: build S3 GET: %w", err)
	}
	req.Header.Set("Authorization", authorization)
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llmrouter: S3 GET failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("llmrouter: S3 GET returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func s3CanonicalURI(objectKey string) string {
	parts := strings.Split(objectKey, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return "/" + strings.Join(parts, "/")
}

func s3SortedHeaderNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)
	return names
}

func s3CanonicalHeaders(headers map[string]string, sortedNames []string) string {
	lower := make(map[string]string, len(headers))
	for name, value := range headers {
		lower[strings.ToLower(name)] = strings.Join(strings.Fields(value), " ")
	}
	var b strings.Builder
	for _, name := range sortedNames {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(lower[name])
		b.WriteByte('\n')
	}
	return b.String()
}

func s3SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func s3SigningKey(secret, dateStamp, region string) []byte {
	kDate := s3HMACSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := s3HMACSHA256(kDate, []byte(region))
	kService := s3HMACSHA256(kRegion, []byte("s3"))
	return s3HMACSHA256(kService, []byte("aws4_request"))
}

func s3HMACSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}
