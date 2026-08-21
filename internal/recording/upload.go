package recording

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// UploadConfig carries net-storage settings for uploading finished round
// recordings. It is sourced from environment variables / bin/.env (see
// internal/conf), never from the YAML.
type UploadConfig struct {
	// Env selects the backend: "prod" uploads to S3, anything else
	// (dev/test/uat/stag/empty) uploads to MinIO.
	Env string

	S3Bucket    string
	S3Region    string
	S3AccessKey string
	S3SecretKey string
	// S3Domain, if set, is used only to build a friendly playback URL for
	// logging (e.g. a CDN domain in front of the bucket).
	S3Domain string

	MinioEndpoint  string
	MinioAccessKey string
	MinioSecretKey string
	// MinioUseSSL is the raw bin/.env value (e.g. "true"/"false"); see
	// minioSecure for how it's interpreted. Empty/unparseable defaults to true.
	MinioUseSSL string
	// MinioDomain, if set, is used only to build a friendly playback URL for
	// logging, same role as S3Domain.
	MinioDomain string
}

// resolveEnv returns the environment to use for this upload: appEnv (from
// the split-rec request's optional "app_env" field) takes priority when
// non-empty, so a single process serving multiple game environments routes
// each round's upload independently; an empty appEnv (older callers that
// don't send it) falls back to c.Env, the process-wide APP_ENV - the
// original, single-environment-per-process behavior is unchanged.
func (c UploadConfig) resolveEnv(appEnv string) string {
	if appEnv != "" {
		return appEnv
	}
	return c.Env
}

func (c UploadConfig) isProd(appEnv string) bool {
	return strings.EqualFold(strings.TrimSpace(c.resolveEnv(appEnv)), "prod")
}

// minioBucket is not a separate setting: the MinIO bucket is always the
// (lowercased) environment name, e.g. env "test" -> bucket "test".
func (c UploadConfig) minioBucket(appEnv string) string {
	return strings.ToLower(strings.TrimSpace(c.resolveEnv(appEnv)))
}

// s3BucketName strips a "s3://" prefix some deployments include in
// S3_BUCKET (e.g. "s3://example-bucket"): the AWS SDK's Bucket field, and a
// path-style playback URL, both need the bare bucket name.
func (c UploadConfig) s3BucketName() string {
	b := strings.TrimSpace(c.S3Bucket)
	b = strings.TrimPrefix(b, "s3://")
	return strings.Trim(b, "/")
}

// minioSecure defaults to true (matches MinIO's own client default) unless
// MinioUseSSL explicitly parses as false.
func (c UploadConfig) minioSecure() bool {
	secure, err := strconv.ParseBool(strings.TrimSpace(c.MinioUseSSL))
	if err != nil {
		return true
	}
	return secure
}

func (c UploadConfig) configured(appEnv string) bool {
	if c.isProd(appEnv) {
		return c.S3Bucket != "" && c.S3AccessKey != "" && c.S3SecretKey != ""
	}
	return c.MinioEndpoint != "" && c.MinioAccessKey != "" && c.MinioSecretKey != "" && c.minioBucket(appEnv) != ""
}

// uploader pushes finished round recordings to S3 (prod) or MinIO (other
// environments) in the background, with retries. Uploading is best-effort:
// a failure never affects the recording/split-rec response.
type uploader struct {
	cfg    UploadConfig
	parent logger.Writer
}

func newUploader(cfg UploadConfig, parent logger.Writer) *uploader {
	return &uploader{cfg: cfg, parent: parent}
}

// uploadAsync uploads filePath under objectKey in the background. appEnv is
// the split-rec request's optional "app_env" field: when non-empty it picks
// which environment's bucket/backend this round's file goes to, taking
// priority over the process-wide APP_ENV (see UploadConfig.resolveEnv) -
// this is what lets one process serve multiple game environments without
// their recordings landing in the same bucket. It is a no-op if net storage
// isn't configured for the resolved environment.
func (u *uploader) uploadAsync(filePath, objectKey, appEnv string) {
	if u == nil || !u.cfg.configured(appEnv) {
		return
	}
	go u.uploadWithRetry(filePath, objectKey, appEnv)
}

func (u *uploader) uploadWithRetry(filePath, objectKey, appEnv string) {
	const maxAttempts = 10

	// The recorder writes fragmented MP4 (moov first, but data split across
	// repeated moof/mdat pairs). That's fine for MSE-based players, but a
	// plain <video src="..."> / "download and double-click" workflow needs
	// a conventional single-moov file to play without buffering the whole
	// thing first. Remux (not re-encode) to faststart MP4 before uploading;
	// if ffmpeg isn't available, upload the original file unchanged.
	uploadPath := filePath
	remuxedPath, cleanup, remuxErr := faststartRemux(filePath)
	if remuxErr != nil {
		u.parent.Log(logger.Warn, "[upload] %s: faststart remux failed, uploading original file: %v", filePath, remuxErr)
	} else if remuxedPath != "" {
		uploadPath = remuxedPath
		defer cleanup()
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > time.Minute {
				backoff = time.Minute
			}
			time.Sleep(backoff)
		}

		var err error
		if u.cfg.isProd(appEnv) {
			err = u.uploadS3(uploadPath, objectKey)
		} else {
			err = u.uploadMinio(uploadPath, objectKey, appEnv)
		}
		if err == nil {
			u.parent.Log(logger.Info, "[upload] %s -> %s succeeded (attempt %d/%d)%s",
				filePath, objectKey, attempt, maxAttempts, u.playbackURLSuffix(objectKey, appEnv))
			return
		}
		lastErr = err
		u.parent.Log(logger.Warn, "[upload] %s -> %s failed (attempt %d/%d): %v",
			filePath, objectKey, attempt, maxAttempts, err)
	}
	u.parent.Log(logger.Warn, "[upload] %s -> %s gave up after %d attempts: %v",
		filePath, objectKey, maxAttempts, lastErr)
}

// faststartRemux stream-copies (no re-encoding) filePath into a sibling
// "<name>.faststart.mp4" with a conventional single moov+mdat layout via
// `ffmpeg -movflags +faststart`, so the uploaded file plays in a plain
// <video src="..."> tag or any simple downloader/player without needing
// the whole file first. The recorder's own output is fragmented MP4
// (moov first, but sample data split across repeated moof/mdat pairs),
// which not every player streams as gracefully.
//
// Returns ("", noop, nil) if ffmpeg can't be found - the caller falls back
// to uploading the original file unremuxed rather than failing the upload
// outright.
func faststartRemux(filePath string) (string, func(), error) {
	noop := func() {}

	ffmpeg, ok := ffmpegPath()
	if !ok {
		return "", noop, nil
	}

	out := strings.TrimSuffix(filePath, filepath.Ext(filePath)) + ".faststart.mp4"
	cleanup := func() { os.Remove(out) }

	cmd := exec.Command(ffmpeg,
		"-y", "-loglevel", "error",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "+faststart",
		out,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, cleanup, nil
}

// ffmpegPath prefers an ffmpeg binary placed next to the running
// executable - a self-contained deployment shouldn't depend on a
// system-wide install (the VPS this was tested on didn't have one) - and
// falls back to PATH. Go 1.19+ deliberately no longer searches the
// current/working directory for security reasons (binary planting), so a
// bare exec.LookPath("ffmpeg") would silently miss a colocated ffmpeg.exe
// even when it's sitting right next to the binary.
func ffmpegPath() (string, bool) {
	exeDir := ""
	if exePath, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exePath)
	}
	return resolveFfmpegPath(exeDir)
}

// resolveFfmpegPath is ffmpegPath's testable core: given the directory the
// running executable lives in (possibly ""), it looks there first, then
// falls back to PATH.
func resolveFfmpegPath(exeDir string) (string, bool) {
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}

	if exeDir != "" {
		candidate := filepath.Join(exeDir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}

	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, true
	}
	return "", false
}

func (u *uploader) uploadS3(filePath, objectKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout(filePath))
	defer cancel()

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(u.cfg.S3Region),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider(u.cfg.S3AccessKey, u.cfg.S3SecretKey, "")),
	)
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	client := s3.NewFromConfig(cfg)
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(u.cfg.s3BucketName()),
		Key:         aws.String(objectKey),
		Body:        f,
		ContentType: aws.String("video/mp4"),
	})
	return err
}

func (u *uploader) uploadMinio(filePath, objectKey, appEnv string) error {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout(filePath))
	defer cancel()

	client, err := minio.New(u.cfg.MinioEndpoint, &minio.Options{
		Creds:  miniocreds.NewStaticV4(u.cfg.MinioAccessKey, u.cfg.MinioSecretKey, ""),
		Secure: u.cfg.minioSecure(),
	})
	if err != nil {
		return fmt.Errorf("create minio client: %w", err)
	}

	_, err = client.FPutObject(ctx, u.cfg.minioBucket(appEnv), objectKey, filePath, minio.PutObjectOptions{
		ContentType: "video/mp4",
	})
	return err
}

// objectKeyFor returns the storage object key for a finished round file:
// just its base name, e.g. "table1-fwv-rec20250904109-p2w001.mp4".
func objectKeyFor(filePath string) string {
	return filepath.Base(filePath)
}

// playbackURLSuffix returns ", url=<...>" when a playback domain is
// configured for the active backend, for a friendlier success log line.
// The URL is path-style (domain/bucket/key): both the S3 and MinIO
// endpoints in use here serve objects that way, not virtual-hosted-style.
func (u *uploader) playbackURLSuffix(objectKey, appEnv string) string {
	domain := u.cfg.S3Domain
	bucket := u.cfg.s3BucketName()
	if !u.cfg.isProd(appEnv) {
		domain = u.cfg.MinioDomain
		bucket = u.cfg.minioBucket(appEnv)
	}
	domain = strings.TrimSpace(domain)
	if domain == "" || bucket == "" {
		return ""
	}
	// The configured domain may already include a scheme (both
	// S3_HTTPS_DOMAIN and MINIO_URL are commonly set to a full
	// "https://..." URL) - don't prepend a second one.
	domain = strings.TrimRight(domain, "/")
	if !strings.Contains(domain, "://") {
		domain = "https://" + domain
	}
	return fmt.Sprintf(", url=%s/%s/%s", domain, bucket, objectKey)
}

// uploadTimeout scales with file size: 60s base + 15s per 10MB, capped at 15m.
func uploadTimeout(filePath string) time.Duration {
	d := 60 * time.Second
	if info, err := os.Stat(filePath); err == nil {
		d += time.Duration(info.Size()/(10*1024*1024)) * 15 * time.Second
	}
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}
