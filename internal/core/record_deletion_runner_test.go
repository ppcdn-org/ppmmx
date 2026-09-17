package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
	"github.com/bluenviron/mediamtx/internal/recording"
)

// fakeRecordDeletionClient lets tests drive RecordDeletionRunner.executeOnce
// without a real HTTP round trip to ppcenter.
type fakeRecordDeletionClient struct {
	mu sync.Mutex

	claims    []mmxcontrol.RecordDeletionClaim
	claimErr  error
	completed []mmxcontrol.RecordDeletionResult
}

func (f *fakeRecordDeletionClient) Claim(_ context.Context, _ int) ([]mmxcontrol.RecordDeletionClaim, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	claims := f.claims
	f.claims = nil // one-shot: the second poll in a test sees nothing new
	return claims, nil
}

func (f *fakeRecordDeletionClient) Complete(_ context.Context, id uint64, success bool, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, mmxcontrol.RecordDeletionResult{ID: id, Success: success, Error: errMsg})
	return nil
}

func newTestRunner(client recordDeletionClient, cfg recording.UploadConfig) *RecordDeletionRunner {
	r := NewRecordDeletionRunner(client, cfg, nopLogger{})
	r.ctx = context.Background()
	return r
}

func TestRecordDeletionRunnerSkipsWhenNothingClaimed(t *testing.T) {
	client := &fakeRecordDeletionClient{}
	r := newTestRunner(client, recording.UploadConfig{})
	r.executeOnce()
	require.Empty(t, client.completed)
}

func TestRecordDeletionRunnerReportsClaimErrorsWithoutPanicking(t *testing.T) {
	client := &fakeRecordDeletionClient{claimErr: context.DeadlineExceeded}
	r := newTestRunner(client, recording.UploadConfig{})
	require.NotPanics(t, r.executeOnce)
	require.Empty(t, client.completed)
}

// Neither S3 nor MinIO is configured: deleteOne must still report an
// outcome (not panic, not hang) so the claim doesn't sit forever unresolved.
func TestRecordDeletionRunnerReportsFailureWhenStorageUnconfigured(t *testing.T) {
	client := &fakeRecordDeletionClient{
		claims: []mmxcontrol.RecordDeletionClaim{
			{ID: 1, ObjectKey: "app1/round-1.mp4", AppEnv: "prod", SizeBytes: 1024},
		},
	}
	r := newTestRunner(client, recording.UploadConfig{})
	r.executeOnce()

	require.Len(t, client.completed, 1)
	require.Equal(t, uint64(1), client.completed[0].ID)
	require.False(t, client.completed[0].Success)
	require.NotEmpty(t, client.completed[0].Error)
}

func TestRecordDeletionRunnerProcessesEveryClaimInABatch(t *testing.T) {
	client := &fakeRecordDeletionClient{
		claims: []mmxcontrol.RecordDeletionClaim{
			{ID: 1, ObjectKey: "app1/a.mp4"},
			{ID: 2, ObjectKey: "app1/b.mp4"},
			{ID: 3, ObjectKey: "app1/c.mp4"},
		},
	}
	r := newTestRunner(client, recording.UploadConfig{})
	r.executeOnce()

	require.Len(t, client.completed, 3)
	seen := map[uint64]bool{}
	for _, c := range client.completed {
		seen[c.ID] = true
	}
	require.True(t, seen[1])
	require.True(t, seen[2])
	require.True(t, seen[3])
}

func TestRecordDeletionRunnerS3Configured(t *testing.T) {
	cases := []struct {
		name string
		cfg  recording.UploadConfig
		want bool
	}{
		{"all set", recording.UploadConfig{S3Bucket: "b", S3AccessKey: "k", S3SecretKey: "s"}, true},
		{"missing bucket", recording.UploadConfig{S3AccessKey: "k", S3SecretKey: "s"}, false},
		{"missing key", recording.UploadConfig{S3Bucket: "b", S3SecretKey: "s"}, false},
		{"missing secret", recording.UploadConfig{S3Bucket: "b", S3AccessKey: "k"}, false},
		{"nothing set", recording.UploadConfig{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &RecordDeletionRunner{cfg: tc.cfg}
			require.Equal(t, tc.want, r.s3Configured())
		})
	}
}

func TestRecordDeletionRunnerS3BucketName(t *testing.T) {
	cases := []struct {
		bucket string
		want   string
	}{
		{"my-bucket", "my-bucket"},
		{"s3://my-bucket", "my-bucket"},
		{"/my-bucket/", "my-bucket"},
		{"  my-bucket  ", "my-bucket"},
	}
	for _, tc := range cases {
		r := &RecordDeletionRunner{cfg: recording.UploadConfig{S3Bucket: tc.bucket}}
		require.Equal(t, tc.want, r.s3BucketName())
	}
}

func TestRecordDeletionRunnerMinioSecureDefaultsTrue(t *testing.T) {
	cases := []struct {
		useSSL string
		want   bool
	}{
		{"", true},
		{"true", true},
		{"garbage", true},
		{"false", false},
		{"False", false},
		{"no", false},
		{"0", false},
	}
	for _, tc := range cases {
		r := &RecordDeletionRunner{cfg: recording.UploadConfig{MinioUseSSL: tc.useSSL}}
		require.Equal(t, tc.want, r.minioSecure(), "useSSL=%q", tc.useSSL)
	}
}

func TestRecordDeletionRunnerMinioBucketFallsBackToEnv(t *testing.T) {
	r := &RecordDeletionRunner{cfg: recording.UploadConfig{Env: "Prod"}}
	require.Equal(t, "prod", r.minioBucket(""))
	require.Equal(t, "test", r.minioBucket("test"))
	// appEnv overrides the process-wide Env, mirroring
	// recording.UploadConfig.resolveEnv.
	require.Equal(t, "test", r.minioBucket("Test"))
}
