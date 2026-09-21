package recording

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// TestStartRoundPrefersH264SuffixedPath pins resolveRecordingController's
// core behavior directly (the other split.go tests only exercise it
// incidentally through fakePathFinder entries keyed by the bare path):
// when a dual-codec publish has registered both "<path>/h264" and
// "<path>/hevc" (or, as here, just "<path>/h264" alongside the bare path a
// single-codec stream would use), round-start must record the H264 variant
// and record it under that resolved path - not the bare one - so
// round-end's lookup and the audit Store row agree with what was actually
// recorded.
func TestStartRoundPrefersH264SuffixedPath(t *testing.T) {
	const appID = "app1"
	const table = "table1"
	basePath := "app1/table1-fwh"
	h264Path := basePath + "/h264"

	mgr := newTestManager(t)
	bareCtrl := &multiviewTestController{}
	h264Ctrl := &multiviewTestController{}

	h := NewSplitRecHandler(mgr, fakePathFinder{basePath: bareCtrl, h264Path: h264Ctrl}, test.NilLogger)

	startReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))

	require.Equal(t, 1, h264Ctrl.startCount, "the h264-suffixed path must be the one actually recorded")
	require.Equal(t, 0, bareCtrl.startCount, "the bare path must not be recorded when an h264-suffixed variant is live")

	recordID := h.activeGames[tableLockKey(appID, table)].recordIDs[h264Path]
	require.NotEmpty(t, recordID, "the audit record must be keyed by the resolved h264 path, not the bare one")

	stopReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1", GameRound: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq))
	require.Equal(t, []string{"table1-fwh-round1"}, h264Ctrl.renamedTo, "round-end must finalize the same h264 path round-start recorded")
}

// TestStartRoundFallsBackToBarePathWithoutH264Variant covers a
// single-codec publish (no dual-track capability requested): it never gets
// a codec suffix at all, so round-start must still find and record it at
// the bare path when no "<path>/h264" variant exists.
func TestStartRoundFallsBackToBarePathWithoutH264Variant(t *testing.T) {
	const appID = "app1"
	const table = "table1"
	basePath := "app1/table1-fwh"

	mgr := newTestManager(t)
	bareCtrl := &multiviewTestController{}

	h := NewSplitRecHandler(mgr, fakePathFinder{basePath: bareCtrl}, test.NilLogger)

	startReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))

	require.Equal(t, 1, bareCtrl.startCount, "the bare path must be recorded when no h264-suffixed variant exists")
}
