package core

import (
	"context"

	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
	"github.com/bluenviron/mediamtx/internal/recording"
)

// splitRecFileReporterAdapter satisfies recording.SplitRecFileReporter by
// forwarding to a *mmxcontrol.RecordingSyncClient. The two packages don't
// import each other (see recording.SplitRecFileReporter's own doc comment),
// so core - which already depends on both - is where the field-for-field
// conversion between their otherwise-identical structs happens.
type splitRecFileReporterAdapter struct {
	client *mmxcontrol.RecordingSyncClient
}

func (a splitRecFileReporterAdapter) ReportSplitRecFile(ctx context.Context, file recording.SplitRecFileInfo) error {
	return a.client.ReportSplitRecFile(ctx, mmxcontrol.SplitRecFileMetadata{
		TableID:         file.TableID,
		GameID:          file.GameID,
		GameRound:       file.GameRound,
		AppEnv:          file.AppEnv,
		StreamPath:      file.StreamPath,
		FileName:        file.FileName,
		ObjectKey:       file.ObjectKey,
		PlaybackURL:     file.PlaybackURL,
		DurationSeconds: file.DurationSeconds,
		SizeBytes:       file.SizeBytes,
	})
}
