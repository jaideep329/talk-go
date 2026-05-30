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

	"github.com/jaideep329/talk-go/internal/sentryutil"
	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const (
	defaultS3UploadTimeout   = 10 * time.Second
	defaultS3DownloadTimeout = 15 * time.Second
)

type JSONUploader interface {
	UploadJSON(ctx context.Context, objectKey string, value any) error
}

// S3GetClient is the narrow read surface used by phonetic-dict and
// other reference-data fetchers. Implementations sign requests with
// SigV4 against the configured AWS account.
type S3GetClient interface {
	GetObject(ctx context.Context, bucket, objectKey string) ([]byte, error)
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
	uploader := newS3ClientFromEnv(logger, strings.TrimSpace(os.Getenv("AWS_BUCKET_NAME")), defaultS3UploadTimeout)
	if uploader == nil {
		if logger != nil {
			logger.Println("disha: S3 debug log upload disabled; AWS env is incomplete")
		}
		return nil
	}
	return uploader
}

// NewS3GetClientFromEnv returns a SigV4-signed GET client. The caller
// passes the bucket they intend to read so we can share credentials
// across reference buckets (e.g. AWS_US_BUCKET_NAME for phonetics).
func NewS3GetClientFromEnv(logger *log.Logger, bucketEnvKeys ...string) S3GetClient {
	bucket := strings.TrimSpace(firstNonEmptyString(envValues(bucketEnvKeys)...))
	if bucket == "" {
		if logger != nil {
			logger.Println("disha: S3 GET client disabled; bucket env is empty")
		}
		return nil
	}
	client := newS3ClientFromEnv(logger, bucket, defaultS3DownloadTimeout)
	if client == nil {
		if logger != nil {
			logger.Println("disha: S3 GET client disabled; AWS env is incomplete")
		}
		return nil
	}
	return client
}

func envValues(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, os.Getenv(k))
	}
	return out
}

func newS3ClientFromEnv(logger *log.Logger, bucket string, timeout time.Duration) *S3Uploader {
	client := &S3Uploader{
		accessKey:  strings.TrimSpace(os.Getenv("ACCESS_KEY_ID")),
		secretKey:  strings.TrimSpace(os.Getenv("SECRET_KEY_ID")),
		region:     firstNonEmptyString(os.Getenv("AWS_MAIN_REGION"), os.Getenv("AWS_REGION")),
		bucket:     bucket,
		httpClient: &http.Client{Timeout: timeout},
		logger:     logger,
	}
	if client.accessKey == "" || client.secretKey == "" || client.region == "" || client.bucket == "" {
		return nil
	}
	return client
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
		sentryutil.Capture(sentryutil.Event{
			Err: err,
			Tags: map[string]string{
				"component": "disha_s3",
				"operation": "debug_log_upload",
			},
		})
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

// GetObject performs a SigV4-signed GET against the configured bucket
// (or the bucket argument if non-empty). Returns the raw object body.
func (u *S3Uploader) GetObject(ctx context.Context, bucket, objectKey string) ([]byte, error) {
	if u == nil {
		return nil, errors.New("disha: S3 client is nil")
	}
	objectKey = strings.TrimLeft(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return nil, errors.New("disha: S3 object key is required")
	}
	if strings.TrimSpace(bucket) == "" {
		bucket = u.bucket
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	encodedKey := s3CanonicalURI(objectKey)
	host := fmt.Sprintf("%s.s3.%s.amazonaws.com", bucket, u.region)
	endpoint := fmt.Sprintf("https://%s%s", host, encodedKey)
	payloadHash := sha256Hex(nil)

	headers := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	signedHeaderNames := sortedHeaderNames(headers)
	canonicalHeaders := canonicalS3Headers(headers, signedHeaderNames)
	signedHeaders := strings.Join(signedHeaderNames, ";")
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, u.region)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		wrapped := fmt.Errorf("disha: build S3 GET: %w", err)
		sentryutil.Capture(sentryutil.Event{
			Err:  wrapped,
			Tags: map[string]string{"component": "disha_s3", "operation": "GET"},
			Details: map[string]any{
				"bucket":     bucket,
				"object_key": objectKey,
			},
		})
		return nil, wrapped
	}
	req.Header.Set("Authorization", authorization)
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		wrapped := fmt.Errorf("disha: S3 GET failed: %w", err)
		sentryutil.Capture(sentryutil.Event{
			Err:  wrapped,
			Tags: map[string]string{"component": "disha_s3", "operation": "GET"},
			Details: map[string]any{
				"bucket":     bucket,
				"object_key": objectKey,
			},
		})
		return nil, wrapped
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		wrapped := fmt.Errorf("disha: S3 GET returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		sentryutil.Capture(sentryutil.Event{
			Err:  wrapped,
			Tags: map[string]string{"component": "disha_s3", "operation": "GET"},
			Details: map[string]any{
				"bucket":     bucket,
				"object_key": objectKey,
				"status":     resp.StatusCode,
			},
		})
		return nil, wrapped
	}
	return body, nil
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
		wrapped := fmt.Errorf("disha: build S3 PUT: %w", err)
		sentryutil.Capture(sentryutil.Event{
			Err:  wrapped,
			Tags: map[string]string{"component": "disha_s3", "operation": "PUT"},
			Details: map[string]any{
				"bucket":     u.bucket,
				"object_key": objectKey,
			},
		})
		return wrapped
	}
	req.Header.Set("Authorization", authorization)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.ContentLength = int64(len(payload))

	resp, err := u.httpClient.Do(req)
	if err != nil {
		wrapped := fmt.Errorf("disha: S3 PUT failed: %w", err)
		sentryutil.Capture(sentryutil.Event{
			Err:  wrapped,
			Tags: map[string]string{"component": "disha_s3", "operation": "PUT"},
			Details: map[string]any{
				"bucket":     u.bucket,
				"object_key": objectKey,
			},
		})
		return wrapped
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		wrapped := fmt.Errorf("disha: S3 PUT returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		sentryutil.Capture(sentryutil.Event{
			Err:  wrapped,
			Tags: map[string]string{"component": "disha_s3", "operation": "PUT"},
			Details: map[string]any{
				"bucket":     u.bucket,
				"object_key": objectKey,
				"status":     resp.StatusCode,
			},
		})
		return wrapped
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
