package webrtc

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPathBelongsToApp(t *testing.T) {
	cases := []struct {
		name     string
		pathName string
		appID    string
		want     bool
	}{
		{"bare path", "app1/live", "app1", true},
		{"codec-suffixed path", "app1/live/h264", "app1", true},
		{"exact app segment only", "app1", "app1", true},
		{"sibling app with shared prefix", "app10/live", "app1", false},
		{"other app", "app2/live", "app1", false},
		{"empty app", "app1/live", "", false},
		{"empty path", "", "app1", false},
		{"whitespace tolerated", "  app1/live  ", " app1 ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, pathBelongsToApp(tc.pathName, tc.appID))
		})
	}
}
