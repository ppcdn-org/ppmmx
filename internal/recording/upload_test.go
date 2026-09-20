package recording

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/bluenviron/mediamtx/internal/test"
)

// TestUploadConfigS3Configured verifies S3 (OVH net-storage) readiness is
// purely a function of whether credentials are set - not of appEnv/Env -
// since S3, once configured, is the backend for every environment (see
// remoteObjectKey for how environments are kept apart within it).
func TestUploadConfigS3Configured(t *testing.T) {
	require.False(t, UploadConfig{}.s3Configured())
	require.False(t, UploadConfig{S3Bucket: "b", S3AccessKey: "a"}.s3Configured(), "missing S3 secret key")
	require.True(t, UploadConfig{S3Bucket: "b", S3AccessKey: "a", S3SecretKey: "s"}.s3Configured())
	require.True(t, UploadConfig{Env: "test", S3Bucket: "b", S3AccessKey: "a", S3SecretKey: "s"}.s3Configured(),
		"env is irrelevant to S3 readiness")
}

func TestUploadConfigConfigured(t *testing.T) {
	require.False(t, UploadConfig{}.configured(""), "nothing set")

	require.False(t, UploadConfig{
		S3Bucket: "b", S3AccessKey: "a",
	}.configured(""), "S3 missing secret key, and no MinIO fallback configured")

	require.True(t, UploadConfig{
		S3Bucket: "b", S3AccessKey: "a", S3SecretKey: "s",
	}.configured(""), "S3 fully configured")

	require.True(t, UploadConfig{
		Env: "test", S3Bucket: "b", S3AccessKey: "a", S3SecretKey: "s",
	}.configured(""), "S3 configured - used for every environment, not just prod")

	require.True(t, UploadConfig{
		S3Bucket: "b", S3AccessKey: "a", S3SecretKey: "s",
		MinioEndpoint: "e", MinioAccessKey: "a", MinioSecretKey: "s",
	}.configured("test"), "S3 takes priority over MinIO when both happen to be configured")

	require.False(t, UploadConfig{
		Env: "test", MinioEndpoint: "e", MinioAccessKey: "a",
	}.configured(""), "no S3, MinIO missing secret key")

	require.True(t, UploadConfig{
		Env: "test", MinioEndpoint: "e", MinioAccessKey: "a", MinioSecretKey: "s",
	}.configured(""), "no S3 - falls back to MinIO")

	require.False(t, UploadConfig{
		Env: "", MinioEndpoint: "e", MinioAccessKey: "a", MinioSecretKey: "s",
	}.configured(""), "no S3, and MinIO with no resolvable env has no derivable bucket name")
}

// TestUploadConfigRemoteObjectKey covers the OVH net-storage requirement
// that every environment shares one S3 bucket, kept apart by an "<appEnv>/"
// object-key prefix instead of by bucket selection - including prod, which
// gets "prod/..." like any other environment rather than staying unprefixed
// (when a request actually sends appEnv="prod"). The prefix is opt-in: it
// only appears when the request itself sends a non-empty appEnv, with no
// fallback to the process-wide Env and no error either way - this keeps the
// common case (callers that never pass appEnv, e.g. today's real prod
// traffic) byte-for-byte identical to the plain, unprefixed key.
func TestUploadConfigRemoteObjectKey(t *testing.T) {
	s3 := UploadConfig{S3Bucket: "b", S3AccessKey: "a", S3SecretKey: "s"}
	require.Equal(t, "prod/key.mp4", s3.remoteObjectKey("key.mp4", "prod"), "prod is not special-cased - it gets a prefix too")
	require.Equal(t, "test/key.mp4", s3.remoteObjectKey("key.mp4", "test"))
	require.Equal(t, "key.mp4", s3.remoteObjectKey("key.mp4", ""),
		"no appEnv sent - unprefixed, regardless of S3Bucket etc. being configured")
	require.Equal(t, "key.mp4",
		UploadConfig{Env: "prod", S3Bucket: "b", S3AccessKey: "a", S3SecretKey: "s"}.remoteObjectKey("key.mp4", ""),
		"empty appEnv is NOT prefixed with the process-wide Env either - no fallback here, unlike minioBucket")
	require.Equal(t, "uat/key.mp4", s3.remoteObjectKey("key.mp4", "  UAT  "),
		"appEnv is trimmed and lowercased when present")

	minio := UploadConfig{Env: "test"} // no S3 creds -> MinIO fallback
	require.Equal(t, "key.mp4", minio.remoteObjectKey("key.mp4", "test"),
		"MinIO already separates environments by bucket, so the key is left untouched")
}

func TestUploadConfigMinioBucket(t *testing.T) {
	require.Equal(t, "test", UploadConfig{Env: "test"}.minioBucket(""))
	require.Equal(t, "uat", UploadConfig{Env: "UAT"}.minioBucket(""))
	require.Equal(t, "stag", UploadConfig{Env: " stag "}.minioBucket(""))
	require.Equal(t, "", UploadConfig{Env: ""}.minioBucket(""))
	require.Equal(t, "prod", UploadConfig{Env: "test"}.minioBucket("prod"), "app_env overrides process-wide Env")
	require.Equal(t, "prod", UploadConfig{Env: "prod"}.minioBucket(""), "empty app_env falls back to process-wide Env")
}

func TestUploadConfigMinioSecure(t *testing.T) {
	require.True(t, UploadConfig{}.minioSecure(), "unset defaults to true")
	require.True(t, UploadConfig{MinioUseSSL: "not-a-bool"}.minioSecure(), "unparseable defaults to true")
	require.True(t, UploadConfig{MinioUseSSL: "true"}.minioSecure())
	require.True(t, UploadConfig{MinioUseSSL: " 1 "}.minioSecure())
	require.False(t, UploadConfig{MinioUseSSL: "false"}.minioSecure())
	require.False(t, UploadConfig{MinioUseSSL: "0"}.minioSecure())
}

func TestObjectKeyForUsesBaseName(t *testing.T) {
	path := filepath.Join("recordings", "live", "table1-fwv", "table1-fwv-rec20250904109-p2w001.mp4")
	require.Equal(t, "table1-fwv-rec20250904109-p2w001.mp4", objectKeyFor(path))
}

func TestUploadAsyncNoopWhenNotConfigured(t *testing.T) {
	// Should return immediately without attempting any network I/O and
	// without panicking, for both an unconfigured uploader and a nil one.
	u := newUploader(UploadConfig{}, test.NilLogger)
	u.uploadAsync("/does/not/exist.mp4", "does-not-exist.mp4", "", SplitRecFileInfo{})

	var nilUploader *uploader
	nilUploader.uploadAsync("/does/not/exist.mp4", "does-not-exist.mp4", "", SplitRecFileInfo{})
}

func TestUploadConfigS3BucketName(t *testing.T) {
	require.Equal(t, "example-bucket", UploadConfig{S3Bucket: "s3://example-bucket"}.s3BucketName(),
		"S3_BUCKET is commonly configured with a s3:// prefix (AWS CLI style); the AWS SDK needs the bare name")
	require.Equal(t, "example-bucket", UploadConfig{S3Bucket: " s3://example-bucket/ "}.s3BucketName())
	require.Equal(t, "plain-bucket", UploadConfig{S3Bucket: "plain-bucket"}.s3BucketName())
	require.Equal(t, "", UploadConfig{}.s3BucketName())
}

func TestUploadConfigS3ACL(t *testing.T) {
	require.Equal(t, s3types.ObjectCannedACL(""), UploadConfig{}.s3ACL(),
		"empty S3ACL omits the header, falling back to the bucket's own default ACL/policy")
	require.Equal(t, s3types.ObjectCannedACLPublicRead, UploadConfig{S3ACL: "public-read"}.s3ACL())
	require.Equal(t, s3types.ObjectCannedACLPrivate, UploadConfig{S3ACL: " private "}.s3ACL())
}

func TestPlaybackURLSuffix(t *testing.T) {
	// The playback URL is path-style (domain/bucket/key), so it always
	// needs a resolved bucket name - both S3's (as configured, minus any
	// s3:// prefix) and MinIO's (the env name) - or it's omitted entirely.
	// playbackURL renders whatever key it's handed as-is; in real use that
	// key already carries the "<env>/" prefix from remoteObjectKey (see
	// TestUploadConfigRemoteObjectKey) - here it's passed explicitly to
	// isolate domain/bucket resolution.
	s3 := newUploader(UploadConfig{S3Bucket: "s3://my-bucket", S3AccessKey: "a", S3SecretKey: "s", S3Domain: "cdn.example.com/"}, test.NilLogger)
	require.Equal(t, ", url=https://cdn.example.com/my-bucket/prod/key.mp4", s3.playbackURLSuffix("prod/key.mp4", "prod"),
		"S3 configured: same bucket/domain regardless of env")
	require.Equal(t, ", url=https://cdn.example.com/my-bucket/test/key.mp4", s3.playbackURLSuffix("test/key.mp4", "test"),
		"non-prod appEnv - still S3, same bucket, just a different key prefix")

	s3NoDomain := newUploader(UploadConfig{S3Bucket: "my-bucket", S3AccessKey: "a", S3SecretKey: "s"}, test.NilLogger)
	require.Equal(t, "", s3NoDomain.playbackURLSuffix("prod/key.mp4", "prod"))

	s3NoBucket := newUploader(UploadConfig{S3AccessKey: "a", S3SecretKey: "s", S3Domain: "cdn.example.com"}, test.NilLogger)
	require.Equal(t, "", s3NoBucket.playbackURLSuffix("prod/key.mp4", "prod"), "no bucket resolvable - omit rather than build a broken URL")

	// S3 not fully configured (credentials missing) falls back to MinIO,
	// bucketed by the resolved environment; S3Domain/S3Bucket are ignored.
	s3IncompleteFallsBackToMinio := newUploader(UploadConfig{Env: "test", S3Domain: "cdn.example.com", MinioDomain: "minio.example.com/"}, test.NilLogger)
	require.Equal(t, ", url=https://minio.example.com/test/key.mp4", s3IncompleteFallsBackToMinio.playbackURLSuffix("key.mp4", ""))

	nonProdWithMinioDomain := newUploader(UploadConfig{Env: "test", MinioDomain: "minio.example.com/"}, test.NilLogger)
	require.Equal(t, ", url=https://minio.example.com/test/key.mp4", nonProdWithMinioDomain.playbackURLSuffix("key.mp4", ""))

	// Both S3_HTTPS_DOMAIN and MINIO_URL are commonly configured
	// as a full URL (scheme included) rather than a bare domain; the
	// scheme must not be duplicated.
	s3WithSchemeInDomain := newUploader(UploadConfig{S3Bucket: "my-bucket", S3AccessKey: "a", S3SecretKey: "s", S3Domain: "https://cdn.example.com/"}, test.NilLogger)
	require.Equal(t, ", url=https://cdn.example.com/my-bucket/key.mp4", s3WithSchemeInDomain.playbackURLSuffix("key.mp4", "prod"))

	nonProdWithSchemeInMinioDomain := newUploader(UploadConfig{Env: "test", MinioDomain: "https://minio.example.com"}, test.NilLogger)
	require.Equal(t, ", url=https://minio.example.com/test/key.mp4", nonProdWithSchemeInMinioDomain.playbackURLSuffix("key.mp4", ""))

	// app_env overrides the process-wide Env for MinIO's bucket choice (S3's
	// bucket never varies by env - see TestUploadConfigRemoteObjectKey for
	// how appEnv affects the S3 key instead).
	appEnvOverridesMinioBucket := newUploader(UploadConfig{Env: "test", MinioDomain: "minio.example.com"}, test.NilLogger)
	require.Equal(t, ", url=https://minio.example.com/uat/key.mp4", appEnvOverridesMinioBucket.playbackURLSuffix("key.mp4", "uat"))

	// Virtual-hosted-style domains (bucket already embedded as the leading
	// host label, e.g. OVH's own net-storage default domain) must not get
	// "/<bucket>/" appended on top - that doubles the bucket segment and
	// 403s against the real endpoint (confirmed in production against
	// ppcdn-net-storage.s3.sgp.io.cloud.ovh.net: the doubled-bucket URL
	// 403s, "domain/key" with no bucket segment 200s).
	s3VirtualHostedStyle := newUploader(UploadConfig{
		S3Bucket: "ppcdn-net-storage", S3AccessKey: "a", S3SecretKey: "s",
		S3Domain: "ppcdn-net-storage.s3.sgp.io.cloud.ovh.net",
	}, test.NilLogger)
	require.Equal(t, ", url=https://ppcdn-net-storage.s3.sgp.io.cloud.ovh.net/test/key.mp4",
		s3VirtualHostedStyle.playbackURLSuffix("test/key.mp4", "test"),
		"bucket is already the domain's leading host label - must not be repeated in the path")

	// A path-style domain that merely starts with the bucket name as a
	// substring (not a full host-label match) must still get the bucket
	// segment - e.g. bucket "cdn" against host "cdn2.example.com" is not
	// the same host label and would silently 404 if the bucket segment
	// were dropped.
	s3PathStyleWithSimilarPrefix := newUploader(UploadConfig{
		S3Bucket: "cdn", S3AccessKey: "a", S3SecretKey: "s",
		S3Domain: "cdn2.example.com",
	}, test.NilLogger)
	require.Equal(t, ", url=https://cdn2.example.com/cdn/key.mp4",
		s3PathStyleWithSimilarPrefix.playbackURLSuffix("key.mp4", "prod"),
		"host label must match the bucket exactly (or bucket+'.'), not just share a string prefix")
}

func TestFaststartRemuxFailsOnUnreadableInput(t *testing.T) {
	out, cleanup, err := faststartRemux(filepath.Join(t.TempDir(), "does-not-exist.mp4"))
	require.Error(t, err)
	require.Empty(t, out)
	cleanup() // must be safe to call even though nothing was created
}
