package recording

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type managerTestController struct {
	stops int
}

func (c *managerTestController) StartOnDemandRecording(Options) (string, error) {
	return "recording.mp4", nil
}
func (c *managerTestController) StopOnDemandRecording() (int64, int64, error) {
	c.stops++
	return 0, 0, nil
}
func (c *managerTestController) SplitRecording(string) (string, error) { return "", nil }
func (c *managerTestController) IsOnline() bool              { return true }

func TestStopIfCurrentDoesNotStopReplacement(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "recordings.db"))
	require.NoError(t, err)
	defer store.Close()
	mgr := NewManager(store)
	firstController := &managerTestController{}
	first, err := mgr.Start("live/test", Options{Format: "fmp4"}, firstController)
	require.NoError(t, err)
	_, err = mgr.Stop("live/test")
	require.NoError(t, err)

	replacementController := &managerTestController{}
	replacement, err := mgr.Start("live/test", Options{Format: "fmp4"}, replacementController)
	require.NoError(t, err)
	_, err = mgr.StopIfCurrent("live/test", first.ID)
	require.Error(t, err)
	require.Equal(t, 0, replacementController.stops)
	require.Equal(t, replacement.ID, mgr.GetRunning("live/test").ID)
}

func TestStopIfCurrentStopsExpectedJob(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "recordings.db"))
	require.NoError(t, err)
	defer store.Close()
	mgr := NewManager(store)
	controller := &managerTestController{}
	record, err := mgr.Start("live/test", Options{Format: "fmp4"}, controller)
	require.NoError(t, err)
	_, err = mgr.StopIfCurrent("live/test", record.ID)
	require.NoError(t, err)
	require.Equal(t, 1, controller.stops)
	require.Nil(t, mgr.GetRunning("live/test"))
}
