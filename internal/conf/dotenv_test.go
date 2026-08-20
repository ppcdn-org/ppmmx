package conf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDotenvValue(t *testing.T) {
	t.Setenv("TX_SecretKey", "from-environment")
	require.Equal(t, "from-environment", dotenvValue("TX_SecretKey"))

	t.Setenv("TX_SecretKey", "")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("TX_SecretKey = \"from-file\"\n"), 0o600))
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(old) })
	require.Equal(t, "from-file", dotenvValue("TX_SecretKey"))
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
	t.Setenv("TX_SecretKey_Back", "test-back-secret")
	require.Equal(t, "test-back-secret", dotenvValue("TX_SecretKey_Back"))
}
