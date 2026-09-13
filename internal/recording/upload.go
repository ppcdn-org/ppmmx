package recording

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// SplitRecFileReporter is the narrow interface uploader uses to tell
// ppcenter about one finished, uploaded split-rec round file, so it lands
// in ppcenter's MySQL (see ppcenter's
// POST /internal/mmx/v1/records/split-rec-files) alongside the S3/MinIO
// object itself. Satisfied by *internal/mmxcontrol.RecordingSyncClient;
// kept as a narrow interface here so this package doesn't need to import
// mmxcontrol. A nil reporter (SetSplitRecFileReporter never called, or
// mmxControl disabled) just means round files are never reported - the
// upload itself is unaffected either way.
type SplitRecFileReporter interface {
	ReportSplitRecFile(ctx context.Context, file SplitRecFileInfo) error
}

// SplitRecFileInfo carries one finished round file's identity/metadata for
// SplitRecFileReporter - a package-local mirror of
// mmxcontrol.SplitRecFileMetadata so this package doesn't need to import
// mmxcontrol just for a struct literal.
type SplitRecFileInfo struct {
	TableID         string
	GameID          string
	GameRound       string
	AppEnv          string
	StreamPath      string
	FileName        string
	ObjectKey       string
	PlaybackURL     string
	DurationSeconds int64
	SizeBytes       int64
}

// UploadConfig carries net-storage settings for uploading finished round
// recordings. It is sourced from environment variables / bin/.env (see
// internal/conf), never from the YAML.
type UploadConfig struct {
	// Env is the process-wide default environment, used for the MinIO
	// fallback bucket when a split-rec request doesn't send its own
	// "appEnv" (MinIO always needs some bucket to target). It plays no part
	// in the normal S3 (OVH net-storage) path: S3, once configured, uploads
	// every environment to the same S3Bucket, and the "<appEnv>/" key prefix
	// that tells them apart (see remoteObjectKey) is added only when the
	// request itself sends a non-empty appEnv - Env is never used as a
	// fallback for it.
	Env string

	S3Bucket    string
	S3Region    string
	S3AccessKey string
	S3SecretKey string
	// S3Domain, if set, is used only to build a friendly playback URL for
	// logging (e.g. a CDN domain in front of the bucket).
	S3Domain string
	// S3Endpoint, if set, overrides the S3 API endpoint so uploads can target
	// an S3-compatible provider (e.g. OVH: "https://s3.sgp.io.cloud.ovh.net")
	// instead of AWS. Empty means the AWS SDK's default AWS endpoint for
	// S3Region. A trailing slash is tolerated.
	S3Endpoint string
	// S3ACL, if set, is sent as the canned ACL (e.g. "public-read",
	// "private") on every object PUT to S3. Empty omits the ACL header
	// entirely, so the object falls back to whatever default ACL/policy the
	// bucket itself has - the original, implicit behavior from before this
	// setting existed.
	S3ACL string

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

// resolveEnv returns the environment to use for the MinIO fallback bucket
// (see minioBucket): appEnv (from the split-rec request's optional
// "appEnv" field) takes priority when non-empty, so a single process
// serving multiple game environments routes each round independently; an
// empty appEnv falls back to c.Env, the process-wide APP_ENV, since MinIO
// always needs some bucket to target. Not used on the S3 path - see
// remoteObjectKey, which leaves an empty appEnv unprefixed instead.
func (c UploadConfig) resolveEnv(appEnv string) string {
	if appEnv != "" {
		return appEnv
	}
	return c.Env
}

// minioBucket is not a separate setting: the MinIO bucket is always the
// resolved environment's (lowercased) name, e.g. env "test" -> bucket
// "test". Only relevant on the MinIO fallback path - see s3Configured. MinIO
// always needs *some* bucket to write to, so this still falls back to the
// process-wide Env when appEnv is empty - unlike remoteObjectKey's S3 key
// prefix, which has a well-defined "no prefix" empty case instead.
func (c UploadConfig) minioBucket(appEnv string) string {
	return strings.ToLower(strings.TrimSpace(c.resolveEnv(appEnv)))
}

// remoteObjectKey returns the object key actually used at the storage
// backend for objectKey. OVH net-storage gives ppcdn a single bucket for
// every environment, so on the S3 path (the normal case) different
// environments are kept apart by an "<appEnv>/" key prefix instead of by
// bucket - e.g. a partner's integration-test upload with appEnv=test lands
// in the same S3Bucket as everything else, as "test/table1-...mp4". The
// prefix is opt-in: it only applies when the split-rec request itself sends
// a non-empty appEnv. A request that omits it gets the plain, unprefixed
// key - same as before this feature existed - with no fallback to the
// process-wide APP_ENV and no error either way; this keeps the common case
// (callers that never pass appEnv) byte-for-byte unchanged. The MinIO
// fallback already separates environments by bucket (see minioBucket), so
// the key there is left untouched regardless.
func (c UploadConfig) remoteObjectKey(objectKey, appEnv string) string {
	appEnv = strings.TrimSpace(appEnv)
	if !c.s3Configured() || appEnv == "" {
		return objectKey
	}
	return strings.ToLower(appEnv) + "/" + objectKey
}

// s3BucketName strips a "s3://" prefix some deployments include in
// S3_BUCKET (e.g. "s3://example-bucket"): the AWS SDK's Bucket field, and a
// path-style playback URL, both need the bare bucket name.
func (c UploadConfig) s3BucketName() string {
	b := strings.TrimSpace(c.S3Bucket)
	b = strings.TrimPrefix(b, "s3://")
	return strings.Trim(b, "/")
}

// s3ACL returns the canned ACL to send with each S3 PutObject call, or ""
// to omit the ACL header entirely - see UploadConfig.S3ACL.
func (c UploadConfig) s3ACL() s3types.ObjectCannedACL {
	return s3types.ObjectCannedACL(strings.TrimSpace(c.S3ACL))
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

// s3Configured reports whether S3 (OVH net-storage) credentials are set.
// Unlike the old prod-only check, this no longer depends on appEnv: S3, once
// configured, is the backend for every environment (see remoteObjectKey).
func (c UploadConfig) s3Configured() bool {
	return c.S3Bucket != "" && c.S3AccessKey != "" && c.S3SecretKey != ""
}

// configured reports whether uploads for appEnv have anywhere to go: S3 if
// configured (takes priority, and then covers every environment), otherwise
// MinIO if that's configured for the resolved environment's bucket.
func (c UploadConfig) configured(appEnv string) bool {
	if c.s3Configured() {
		return true
	}
	return c.MinioEndpoint != "" && c.MinioAccessKey != "" && c.MinioSecretKey != "" && c.minioBucket(appEnv) != ""
}

// uploader pushes finished round recordings to S3 (every environment, once
// configured) or MinIO (fallback when S3 isn't configured) in the
// background, with retries. Uploading is best-effort: a failure never
// affects the recording/split-rec response.
type uploader struct {
	cfg      UploadConfig
	parent   logger.Writer
	reporter SplitRecFileReporter
}

func newUploader(cfg UploadConfig, parent logger.Writer) *uploader {
	return &uploader{cfg: cfg, parent: parent}
}

// uploadAsync uploads filePath under objectKey in the background. appEnv is
// the split-rec request's optional "appEnv" field. On the S3 path, a
// non-empty appEnv only adds an "<appEnv>/" key prefix within the one
// shared bucket (see remoteObjectKey) - it never changes the bucket itself,
// and an empty appEnv gets no prefix at all (no fallback to the
// process-wide APP_ENV). It is a no-op if net storage isn't configured for
// the resolved environment (see UploadConfig.configured, which - unlike the
// key prefix - does still fall back to APP_ENV for the MinIO path, since
// MinIO needs some bucket to target). info carries the
// tableId/gameId/gameRound identity (and appEnv again, for the reported
// row) used to tell ppcenter about the file once the upload succeeds - see
// SplitRecFileReporter; a zero-value info is fine when no reporter is
// wired in.
func (u *uploader) uploadAsync(filePath, objectKey, appEnv string, info SplitRecFileInfo) {
	if u == nil || !u.cfg.configured(appEnv) {
		return
	}
	go u.uploadWithRetry(filePath, objectKey, appEnv, info)
}

func (u *uploader) uploadWithRetry(filePath, objectKey, appEnv string, info SplitRecFileInfo) {
	const maxAttempts = 10

	// The recorder writes fragmented MP4 (moov first, but data split across
	// repeated moof/mdat pairs). That's fine for MSE-based players, but a
	// plain <video src="..."> / "download and double-click" workflow needs
	// a conventional single-moov file to play without buffering the whole
	// thing first. Remux (not re-encode) to faststart MP4 before uploading;
	// if that fails for any reason, upload the original file unchanged
	// rather than losing the recording.
	uploadPath := filePath
	remuxedPath, cleanup, remuxErr := faststartRemux(filePath)
	if remuxErr != nil {
		u.parent.Log(logger.Warn, "[upload] %s: faststart remux failed, uploading original file: %v", filePath, remuxErr)
	} else {
		uploadPath = remuxedPath
		defer cleanup()
	}

	useS3 := u.cfg.s3Configured()
	remoteKey := u.cfg.remoteObjectKey(objectKey, appEnv)

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
		if useS3 {
			err = u.uploadS3(uploadPath, remoteKey)
		} else {
			err = u.uploadMinio(uploadPath, remoteKey, appEnv)
		}
		if err == nil {
			u.parent.Log(logger.Info, "[upload] %s -> %s succeeded (attempt %d/%d)%s",
				filePath, remoteKey, attempt, maxAttempts, u.playbackURLSuffix(remoteKey, appEnv))
			u.reportSplitRecFile(remoteKey, appEnv, info)
			return
		}
		lastErr = err
		u.parent.Log(logger.Warn, "[upload] %s -> %s failed (attempt %d/%d): %v",
			filePath, remoteKey, attempt, maxAttempts, err)
	}
	u.parent.Log(logger.Warn, "[upload] %s -> %s gave up after %d attempts: %v",
		filePath, remoteKey, maxAttempts, lastErr)
}

// reportSplitRecFile tells ppcenter about a round file once its upload has
// succeeded - see SplitRecFileReporter. Best-effort and synchronous in the
// caller's own background goroutine (uploadWithRetry's), same as the
// upload itself: a failure here only logs a warning, since the file is
// already safely uploaded by this point and the split-rec HTTP response
// returned long ago.
func (u *uploader) reportSplitRecFile(remoteKey, appEnv string, info SplitRecFileInfo) {
	if u.reporter == nil {
		return
	}
	info.ObjectKey = remoteKey
	info.AppEnv = appEnv
	info.PlaybackURL = u.playbackURL(remoteKey, appEnv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := u.reporter.ReportSplitRecFile(ctx, info); err != nil {
		u.parent.Log(logger.Warn, "[upload] %s -> %s: report to ppcenter failed: %v", remoteKey, appEnv, err)
	}
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

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		// Custom endpoint for S3-compatible providers (e.g. OVH). Overrides
		// any AWS_ENDPOINT_URL[_S3] the SDK may have picked up from the env,
		// so the endpoint can live in .env alongside the other S3_* settings.
		if ep := strings.TrimRight(strings.TrimSpace(u.cfg.S3Endpoint), "/"); ep != "" {
			o.BaseEndpoint = aws.String(ep)
		}
	})
	input := &s3.PutObjectInput{
		Bucket:      aws.String(u.cfg.s3BucketName()),
		Key:         aws.String(objectKey),
		Body:        f,
		ContentType: aws.String("video/mp4"),
	}
	if acl := u.cfg.s3ACL(); acl != "" {
		input.ACL = acl
	}
	_, err = client.PutObject(ctx, input)
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

// playbackURL returns the public playback URL for remoteKey - the object
// key as actually stored (see remoteObjectKey; already "<env>/..."-prefixed
// on the S3 path, by the time this is called) - on the active backend (S3
// if configured, MinIO otherwise), or "" if no playback domain is
// configured for it. The URL is path-style (domain/bucket/key): both the
// S3 and MinIO endpoints in use here serve objects that way, not
// virtual-hosted-style.
func (u *uploader) playbackURL(remoteKey, appEnv string) string {
	domain := u.cfg.S3Domain
	bucket := u.cfg.s3BucketName()
	if !u.cfg.s3Configured() {
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
	return fmt.Sprintf("%s/%s/%s", domain, bucket, remoteKey)
}

// playbackURLSuffix returns ", url=<...>" for a friendlier success log
// line - see playbackURL.
func (u *uploader) playbackURLSuffix(remoteKey, appEnv string) string {
	url := u.playbackURL(remoteKey, appEnv)
	if url == "" {
		return ""
	}
	return ", url=" + url
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
