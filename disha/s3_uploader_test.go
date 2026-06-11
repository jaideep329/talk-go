package disha

import "testing"

func TestNewS3GetClientFromEnvUsesBucketRegion(t *testing.T) {
	t.Setenv("ACCESS_KEY_ID", "test-access")
	t.Setenv("SECRET_KEY_ID", "test-secret")
	t.Setenv("AWS_MAIN_REGION", "ap-south-1")
	t.Setenv("AWS_US_BUCKET_NAME", "us-bucket")
	t.Setenv("AWS_US_REGION", "us-east-1")

	client := NewS3GetClientFromEnv(nil, "AWS_US_BUCKET_NAME", "AWS_US_REGION")
	uploader, ok := client.(*S3Uploader)
	if !ok {
		t.Fatalf("client = %#v, want *S3Uploader", client)
	}
	if uploader.bucket != "us-bucket" || uploader.region != "us-east-1" {
		t.Fatalf("bucket=%q region=%q, want us-bucket/us-east-1", uploader.bucket, uploader.region)
	}
}

func TestNewS3GetClientFromEnvDisabledWhenRegionMissing(t *testing.T) {
	t.Setenv("ACCESS_KEY_ID", "test-access")
	t.Setenv("SECRET_KEY_ID", "test-secret")
	t.Setenv("AWS_US_BUCKET_NAME", "us-bucket")
	t.Setenv("AWS_US_REGION", "")

	if client := NewS3GetClientFromEnv(nil, "AWS_US_BUCKET_NAME", "AWS_US_REGION"); client != nil {
		t.Fatalf("client = %#v, want nil when region env is empty", client)
	}
}
