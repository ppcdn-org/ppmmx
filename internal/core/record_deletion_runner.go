package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
	"github.com/bluenviron/mediamtx/internal/recording"
)

// recordDeletionClient is the subset of *mmxcontrol.RecordDeletionClient
// this runner needs, narrowed so tests can substitute a fake.
type recordDeletionClient interface {
	Claim(ctx context.Context, limit int) ([]mmxcontrol.RecordDeletionClaim, error)
	Complete(ctx context.Context, id uint64, success bool, errMsg string) error
}

// RecordDeletionRunner periodically claims pending object deletions from
// ppcenter, executes them against the configured S3/MinIO storage, and
// reports the outcome. Runs only on record nodes that have net-storage
// credentials configured.
//
// A delete that fails is reported back; ppcenter decides how many retries
// to allow before marking it failed-for-good. A delete whose object is
// already absent (e.g. a prior lifecycle rule or manual cleanup beat us to
// it) is reported as success: the desired end state has been reached either
// way.
type RecordDeletionRunner struct {
	client recordDeletionClient
	cfg    recording.UploadConfig
	parent logger.Writer

	claimLimit int
	interval   time.Duration

	ctx       context.Context
	ctxCancel func()
	done      chan struct{}
}

// NewRecordDeletionRunner creates a runner that polls ppcenter's deletion
// queue every interval and executes up to claimLimit objects per pass.
func NewRecordDeletionRunner(
	client recordDeletionClient,
	cfg recording.UploadConfig,
	parent logger.Writer,
) *RecordDeletionRunner {
	return &RecordDeletionRunner{
		client:     client,
		cfg:        cfg,
		parent:     parent,
		claimLimit: 100,
		interval:   30 * time.Second,
	}
}

func (r *RecordDeletionRunner) Initialize() {
	r.ctx, r.ctxCancel = context.WithCancel(context.Background())
	r.done = make(chan struct{})
	go r.run()
}

func (r *RecordDeletionRunner) Close() {
	r.ctxCancel()
	<-r.done
}

func (r *RecordDeletionRunner) run() {
	defer close(r.done)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.executeOnce()
		case <-r.ctx.Done():
			return
		}
	}
}

func (r *RecordDeletionRunner) executeOnce() {
	claims, err := r.client.Claim(r.ctx, r.claimLimit)
	if err != nil {
		r.parent.Log(logger.Debug, "[record-deletion] claim failed: %v", err)
		return
	}
	if len(claims) == 0 {
		return
	}

	for _, claim := range claims {
		success, errMsg := r.deleteOne(claim.ObjectKey, claim.AppEnv)
		if err := r.client.Complete(r.ctx, claim.ID, success, errMsg); err != nil {
			r.parent.Log(logger.Debug, "[record-deletion] complete failed for %s (id=%d): %v",
				claim.ObjectKey, claim.ID, err)
		}
		if success {
			r.parent.Log(logger.Info, "[record-deletion] deleted %s (id=%d, size=%d)",
				claim.ObjectKey, claim.ID, claim.SizeBytes)
		} else {
			r.parent.Log(logger.Warn, "[record-deletion] failed to delete %s (id=%d): %s",
				claim.ObjectKey, claim.ID, errMsg)
		}
	}
}

// s3Configured reports whether S3 credentials are set, mirroring
// recording.UploadConfig.s3Configured but usable from outside the package.
func (r *RecordDeletionRunner) s3Configured() bool {
	return r.cfg.S3Bucket != "" && r.cfg.S3AccessKey != "" && r.cfg.S3SecretKey != ""
}

// s3BucketName strips "s3://" prefix if present, mirroring
// recording.UploadConfig.s3BucketName.
func (r *RecordDeletionRunner) s3BucketName() string {
	b := strings.TrimSpace(r.cfg.S3Bucket)
	b = strings.TrimPrefix(b, "s3://")
	return strings.Trim(b, "/")
}

// minioSecure defaults to true, mirroring recording.UploadConfig.minioSecure.
func (r *RecordDeletionRunner) minioSecure() bool {
	// minio.New's Secure option defaults to true, so anything that doesn't
	// explicitly parse as false stays at the default.
	if strings.EqualFold(strings.TrimSpace(r.cfg.MinioUseSSL), "false") ||
		strings.EqualFold(strings.TrimSpace(r.cfg.MinioUseSSL), "no") ||
		strings.EqualFold(strings.TrimSpace(r.cfg.MinioUseSSL), "0") {
		return false
	}
	return true
}

// minioBucket returns the bucket name for an appEnv, mirroring
// recording.UploadConfig.minioBucket.
func (r *RecordDeletionRunner) minioBucket(appEnv string) string {
	e := appEnv
	if e == "" {
		e = r.cfg.Env
	}
	return strings.ToLower(strings.TrimSpace(e))
}

// deleteOne removes one object from storage. Returns success and an error
// message for the report. An object that is already gone is counted as
// success.
func (r *RecordDeletionRunner) deleteOne(objectKey, appEnv string) (bool, string) {
	if r.s3Configured() {
		return r.deleteS3(objectKey)
	}
	return r.deleteMinio(objectKey, appEnv)
}

func (r *RecordDeletionRunner) deleteS3(objectKey string) (bool, string) {
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(r.cfg.S3Region),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider(r.cfg.S3AccessKey, r.cfg.S3SecretKey, "")),
	)
	if err != nil {
		return false, fmt.Sprintf("load aws config: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if ep := strings.TrimRight(strings.TrimSpace(r.cfg.S3Endpoint), "/"); ep != "" {
			o.BaseEndpoint = aws.String(ep)
		}
	})

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.s3BucketName()),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "notfound") ||
			strings.Contains(strings.ToLower(err.Error()), "nosuchkey") {
			return true, ""
		}
		return false, err.Error()
	}
	return true, ""
}

func (r *RecordDeletionRunner) deleteMinio(objectKey, appEnv string) (bool, string) {
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()

	client, err := minio.New(r.cfg.MinioEndpoint, &minio.Options{
		Creds:  miniocreds.NewStaticV4(r.cfg.MinioAccessKey, r.cfg.MinioSecretKey, ""),
		Secure: r.minioSecure(),
	})
	if err != nil {
		return false, fmt.Sprintf("create minio client: %v", err)
	}

	err = client.RemoveObject(ctx, r.minioBucket(appEnv), objectKey, minio.RemoveObjectOptions{})
	if err != nil {
		return false, err.Error()
	}
	return true, ""
}

// Log implements logger.Writer for minimal logging context.
func (r *RecordDeletionRunner) Log(level logger.Level, format string, args ...any) {
	r.parent.Log(level, "[record-deletion] "+format, args...)
}
