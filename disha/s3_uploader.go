package disha

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const defaultS3UploadTimeout = 10 * time.Second

type JSONUploader interface {
	UploadJSON(ctx context.Context, objectKey string, value any) error
}

type DebugLogUploader func(ctx context.Context, entries []voicepipelinecore.RTVIDebugLogEntry) (string, error)

type S3Uploader struct {
	accessKey  string
	secretKey  string
	region     string
	bucket     string
	httpClient *http.Client
	logger     *log.Logger
}

func NewS3UploaderFromEnv(logger *log.Logger) JSONUploader {
	uploader := &S3Uploader{
		accessKey:  strings.TrimSpace(os.Getenv("ACCESS_KEY_ID")),
		secretKey:  strings.TrimSpace(os.Getenv("SECRET_KEY_ID")),
		region:     firstNonEmptyString(os.Getenv("AWS_MAIN_REGION"), os.Getenv("AWS_REGION")),
		bucket:     strings.TrimSpace(os.Getenv("AWS_BUCKET_NAME")),
		httpClient: &http.Client{Timeout: defaultS3UploadTimeout},
		logger:     logger,
	}
	if uploader.accessKey == "" || uploader.secretKey == "" || uploader.region == "" || uploader.bucket == "" {
		if logger != nil {
			logger.Println("disha: S3 debug log upload disabled; AWS env is incomplete")
		}
		return nil
	}
	return uploader
}

func NewDebugLogUploaderFromEnv(logger *log.Logger, conversationID string) DebugLogUploader {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil
	}
	store := NewS3UploaderFromEnv(logger)
	if store == nil {
		return nil
	}
	objectKey := fmt.Sprintf("debug_log_data/%s/log_data.json", conversationID)
	return func(ctx context.Context, entries []voicepipelinecore.RTVIDebugLogEntry) (string, error) {
		if len(entries) == 0 {
			return "", nil
		}
		if err := store.UploadJSON(ctx, objectKey, entries); err != nil {
			return "", err
		}
		return objectKey, nil
	}
}

func uploadDebugLogs(logger interface{ Println(v ...any) }, uploader DebugLogUploader, logs []voicepipelinecore.RTVIDebugLogEntry) string {
	if uploader == nil || len(logs) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), postCallRequestTimeout)
	defer cancel()
	key, err := uploader(ctx, logs)
	if err != nil {
		if logger != nil {
			logger.Println("disha: debug log upload failed:", err)
		}
		return ""
	}
	return key
}

func (u *S3Uploader) UploadJSON(ctx context.Context, objectKey string, value any) error {
	if u == nil {
		return errors.New("disha: S3 uploader is nil")
	}
	objectKey = strings.TrimLeft(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return errors.New("disha: S3 object key is required")
	}
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("disha: marshal S3 JSON: %w", err)
	}
	return u.putObject(ctx, objectKey, payload, "application/json")
}

func (u *S3Uploader) putObject(ctx context.Context, objectKey string, payload []byte, contentType string) error {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	encodedKey := s3CanonicalURI(objectKey)
	host := fmt.Sprintf("%s.s3.%s.amazonaws.com", u.bucket, u.region)
	endpoint := fmt.Sprintf("https://%s%s", host, encodedKey)
	payloadHash := sha256Hex(payload)

	headers := map[string]string{
		"content-type":         contentType,
		"host":                 host,
		"x-amz-acl":            "public-read",
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	signedHeaderNames := sortedHeaderNames(headers)
	canonicalHeaders := canonicalS3Headers(headers, signedHeaderNames)
	signedHeaders := strings.Join(signedHeaderNames, ";")
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, u.region)
	canonicalRequest := strings.Join([]string{
		http.MethodPut,
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
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(signingKey(u.secretKey, dateStamp, u.region, "s3"), []byte(stringToSign)))
	authorization := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		u.accessKey,
		scope,
		signedHeaders,
		signature,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("disha: build S3 PUT: %w", err)
	}
	req.Header.Set("Authorization", authorization)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.ContentLength = int64(len(payload))

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("disha: S3 PUT failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("disha: S3 PUT returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func sortedHeaderNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)
	return names
}

func canonicalS3Headers(headers map[string]string, sortedNames []string) string {
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

func s3CanonicalURI(objectKey string) string {
	parts := strings.Split(objectKey, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return "/" + strings.Join(parts, "/")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
