package recording

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

func TestUploadConfigIsProd(t *testing.T) {
	require.True(t, UploadConfig{Env: "prod"}.isProd())
	require.True(t, UploadConfig{Env: "PROD"}.isProd())
	require.True(t, UploadConfig{Env: " prod "}.isProd())
	require.False(t, UploadConfig{Env: "test"}.isProd())
	require.False(t, UploadConfig{Env: "uat"}.isProd())
	require.False(t, UploadConfig{Env: ""}.isProd())
}

func TestUploadConfigConfigured(t *testing.T) {
	require.False(t, UploadConfig{}.configured(), "nothing set")

	require.False(t, UploadConfig{
		Env: "prod", S3Bucket: "b", S3AccessKey: "a",
	}.configured(), "prod missing S3 secret key")

	require.True(t, UploadConfig{
		Env: "prod", S3Bucket: "b", S3AccessKey: "a", S3SecretKey: "s",
	}.configured(), "prod with full S3 creds")

	require.False(t, UploadConfig{
		Env: "prod", MinioEndpoint: "e", MinioAccessKey: "a", MinioSecretKey: "s",
	}.configured(), "prod ignores MinIO-only config")

	require.False(t, UploadConfig{
		Env: "test", MinioEndpoint: "e", MinioAccessKey: "a",
	}.configured(), "non-prod missing MinIO secret key")

	require.True(t, UploadConfig{
		Env: "test", MinioEndpoint: "e", MinioAccessKey: "a", MinioSecretKey: "s",
	}.configured(), "non-prod with full MinIO creds")

	require.False(t, UploadConfig{
		Env: "", S3Bucket: "b", S3AccessKey: "a", S3SecretKey: "s",
	}.configured(), "empty env routes to MinIO, ignoring S3-only config")

	require.False(t, UploadConfig{
		Env: "", MinioEndpoint: "e", MinioAccessKey: "a", MinioSecretKey: "s",
	}.configured(), "MinIO with no env set has no derivable bucket name")
}

func TestUploadConfigMinioBucket(t *testing.T) {
	require.Equal(t, "test", UploadConfig{Env: "test"}.minioBucket())
	require.Equal(t, "uat", UploadConfig{Env: "UAT"}.minioBucket())
	require.Equal(t, "stag", UploadConfig{Env: " stag "}.minioBucket())
	require.Equal(t, "", UploadConfig{Env: ""}.minioBucket())
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
	path := filepath.Join("recordings", "live", "3drush-fwv", "3drush-fwv-loto20250904109-p2w001.mp4")
	require.Equal(t, "3drush-fwv-loto20250904109-p2w001.mp4", objectKeyFor(path))
}

func TestUploadAsyncNoopWhenNotConfigured(t *testing.T) {
	// Should return immediately without attempting any network I/O and
	// without panicking, for both an unconfigured uploader and a nil one.
	u := newUploader(UploadConfig{}, test.NilLogger)
	u.uploadAsync("/does/not/exist.mp4", "does-not-exist.mp4")

	var nilUploader *uploader
	nilUploader.uploadAsync("/does/not/exist.mp4", "does-not-exist.mp4")
}

func TestUploadConfigS3BucketName(t *testing.T) {
	require.Equal(t, "ge-lotto-live", UploadConfig{S3Bucket: "s3://ge-lotto-live"}.s3BucketName(),
		"S3_BUCKET is commonly configured with a s3:// prefix (AWS CLI style); the AWS SDK needs the bare name")
	require.Equal(t, "ge-lotto-live", UploadConfig{S3Bucket: " s3://ge-lotto-live/ "}.s3BucketName())
	require.Equal(t, "plain-bucket", UploadConfig{S3Bucket: "plain-bucket"}.s3BucketName())
	require.Equal(t, "", UploadConfig{}.s3BucketName())
}

func TestPlaybackURLSuffix(t *testing.T) {
	// The playback URL is path-style (domain/bucket/key), so it always
	// needs a resolved bucket name - both S3's (as configured, minus any
	// s3:// prefix) and MinIO's (the env name) - or it's omitted entirely.
	prod := newUploader(UploadConfig{Env: "prod", S3Bucket: "s3://my-bucket", S3Domain: "cdn.example.com/"}, test.NilLogger)
	require.Equal(t, ", url=https://cdn.example.com/my-bucket/key.mp4", prod.playbackURLSuffix("key.mp4"))

	prodNoDomain := newUploader(UploadConfig{Env: "prod", S3Bucket: "my-bucket"}, test.NilLogger)
	require.Equal(t, "", prodNoDomain.playbackURLSuffix("key.mp4"))

	prodNoBucket := newUploader(UploadConfig{Env: "prod", S3Domain: "cdn.example.com"}, test.NilLogger)
	require.Equal(t, "", prodNoBucket.playbackURLSuffix("key.mp4"), "no bucket resolvable - omit rather than build a broken URL")

	nonProd := newUploader(UploadConfig{Env: "test", S3Domain: "cdn.example.com"}, test.NilLogger)
	require.Equal(t, "", nonProd.playbackURLSuffix("key.mp4"), "non-prod ignores S3Domain/S3Bucket")

	nonProdWithMinioDomain := newUploader(UploadConfig{Env: "test", MinioDomain: "minio.example.com/"}, test.NilLogger)
	require.Equal(t, ", url=https://minio.example.com/test/key.mp4", nonProdWithMinioDomain.playbackURLSuffix("key.mp4"))

	// Both S3_HTTPS_DOMAIN and MINIO_URL are commonly configured
	// as a full URL (scheme included) rather than a bare domain; the
	// scheme must not be duplicated.
	prodWithSchemeInDomain := newUploader(UploadConfig{Env: "prod", S3Bucket: "my-bucket", S3Domain: "https://cdn.example.com/"}, test.NilLogger)
	require.Equal(t, ", url=https://cdn.example.com/my-bucket/key.mp4", prodWithSchemeInDomain.playbackURLSuffix("key.mp4"))

	nonProdWithSchemeInMinioDomain := newUploader(UploadConfig{Env: "test", MinioDomain: "https://minio.example.com"}, test.NilLogger)
	require.Equal(t, ", url=https://minio.example.com/test/key.mp4", nonProdWithSchemeInMinioDomain.playbackURLSuffix("key.mp4"))
}

func TestFaststartRemuxSkipsGracefullyWhenFfmpegNotFound(t *testing.T) {
	t.Setenv("PATH", "")
	out, cleanup, err := faststartRemux("/does/not/matter.mp4")
	require.NoError(t, err)
	require.Empty(t, out)
	cleanup() // must be safe to call even though nothing was created
}

// TestFaststartRemuxProducesNonFragmentedMP4 exercises the real ffmpeg
// binary end to end: generates a small fragmented MP4 (mirroring the
// recorder's own moof/mdat-per-part output), remuxes it, and checks the
// result has a single moov before a single mdat with no moof boxes left -
// i.e. a conventional file a plain <video src> can play without
// buffering the whole thing first. Skips if ffmpeg (or this specific
// build's video encoder) isn't available.
func TestFaststartRemuxProducesNonFragmentedMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	dir := t.TempDir()
	input := filepath.Join(dir, "input.mp4")

	gen := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=64x64:rate=5",
		"-pix_fmt", "yuv420p",
		"-movflags", "frag_keyframe+empty_moov",
		input,
	)
	if err := gen.Run(); err != nil {
		t.Skipf("could not generate a fragmented-MP4 test input with this ffmpeg build: %v", err)
	}

	out, cleanup, err := faststartRemux(input)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	require.FileExists(t, out)

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	moovPos := bytes.Index(data, []byte("moov"))
	mdatPos := bytes.Index(data, []byte("mdat"))
	require.Greater(t, moovPos, 0, "moov box not found")
	require.Greater(t, mdatPos, moovPos, "mdat must come after moov (faststart layout)")
	require.Equal(t, -1, bytes.Index(data, []byte("moof")), "remuxed output should not be fragmented")

	cleanup()
	_, statErr := os.Stat(out)
	require.True(t, os.IsNotExist(statErr), "cleanup should remove the remuxed file")
}

// TestResolveFfmpegPathPrefersColocatedBinary guards the actual bug found
// while testing on the real deployment: Go 1.19+ deliberately stopped
// searching the current/working directory in exec.LookPath (security
// fix against binary planting), so a bare exec.LookPath("ffmpeg") never
// finds an ffmpeg placed next to the running binary for a self-contained
// deployment - it silently falls through to (or misses) PATH instead.
func TestResolveFfmpegPathPrefersColocatedBinary(t *testing.T) {
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}

	dir := t.TempDir()
	colocated := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(colocated, []byte("stand-in, never executed by this test"), 0o755))

	resolved, ok := resolveFfmpegPath(dir)
	require.True(t, ok)
	require.Equal(t, colocated, resolved, "must prefer the binary next to the executable over PATH")
}

func TestResolveFfmpegPathFallsBackToPath(t *testing.T) {
	dir := t.TempDir() // no colocated ffmpeg here
	fromPath, pathErr := exec.LookPath("ffmpeg")

	resolved, ok := resolveFfmpegPath(dir)
	require.Equal(t, pathErr == nil, ok)
	if pathErr == nil {
		require.Equal(t, fromPath, resolved)
	}
}

func TestResolveFfmpegPathReturnsFalseWhenNowhereFound(t *testing.T) {
	t.Setenv("PATH", "")
	resolved, ok := resolveFfmpegPath(t.TempDir())
	require.False(t, ok)
	require.Empty(t, resolved)
}
