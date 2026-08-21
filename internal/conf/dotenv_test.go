package conf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDotenvValue(t *testing.T) {
	t.Setenv("TX_SECRET_KEY", "from-environment")
	require.Equal(t, "from-environment", DotenvValue("TX_SECRET_KEY"))

	t.Setenv("TX_SECRET_KEY", "")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("TX_SECRET_KEY = \"from-file\"\n"), 0o600))
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(old) })
	require.Equal(t, "from-file", DotenvValue("TX_SECRET_KEY"))
}

func TestSetDotenvValueUpdatesExistingKey(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(
		"OTHER=keep\nRECORD_MODE=pull\n"), 0o600))
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(old) })

	require.NoError(t, SetDotenvValue("RECORD_MODE", "push"))

	got, err := os.ReadFile(filepath.Join(dir, ".env"))
	require.NoError(t, err)
	require.Equal(t, "OTHER=keep\nRECORD_MODE=push\n", string(got))
}

func TestSetDotenvValueAppendsNewKey(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("OTHER=keep\n"), 0o600))
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(old) })

	require.NoError(t, SetDotenvValue("RECORD_MODE", "push"))

	got, err := os.ReadFile(filepath.Join(dir, ".env"))
	require.NoError(t, err)
	require.Equal(t, "OTHER=keep\nRECORD_MODE=push\n", string(got))
}

func TestSetDotenvValuePreservesCRLFLineEndings(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(
		"# comment\r\nOTHER=keep\r\nRECORD_MODE=pull\r\n"), 0o600))
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(old) })

	require.NoError(t, SetDotenvValue("RECORD_MODE", "push"))

	got, err := os.ReadFile(filepath.Join(dir, ".env"))
	require.NoError(t, err)
	require.Equal(t, "# comment\r\nOTHER=keep\r\nRECORD_MODE=push\r\n", string(got))
}

func TestSetDotenvValueCreatesFileWhenMissing(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(old) })

	require.NoError(t, SetDotenvValue("RECORD_MODE", "pull"))

	got, err := os.ReadFile(filepath.Join(dir, ".env"))
	require.NoError(t, err)
	require.Equal(t, "RECORD_MODE=pull\n", string(got))
}

func TestSplitRecAuthValidation(t *testing.T) {
	conf := &Conf{}
	conf.setDefaults()
	conf.SplitRecAuthMode = "invalid"
	require.EqualError(t, conf.Validate(nil), "'splitRecAuthMode' must be either 'simple' or 'advance'")

	conf.setDefaults()
	conf.SplitRecAuthMode = "advance"
	require.EqualError(t, conf.Validate(nil), "SPLIT_REC_SECRET must be set when splitRecAuthMode is advance")
}

func TestBackSecretFromEnvironment(t *testing.T) {
	t.Setenv("TX_SECRET_KEY_BACK", "test-back-secret")
	require.Equal(t, "test-back-secret", DotenvValue("TX_SECRET_KEY_BACK"))
}
