package conf

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// nilLogger (defined in conf.go) discards everything. internal/test.NilLogger
// can't be used here: internal/test imports internal/auth, which imports
// internal/conf, which would make this test file's import an import cycle.

func TestJasyptDecryptRoundTrip(t *testing.T) {
	salt := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	cipherB64, err := jasyptEncrypt("my-secret-password", "hello nacos", salt)
	require.NoError(t, err)

	plain, err := jasyptDecrypt("my-secret-password", cipherB64)
	require.NoError(t, err)
	require.Equal(t, "hello nacos", plain)
}

func TestJasyptDecryptWrongPasswordFails(t *testing.T) {
	salt := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	cipherB64, err := jasyptEncrypt("correct-password", "top secret", salt)
	require.NoError(t, err)

	// A wrong password derives a different key/IV; PKCS5 unpadding then
	// fails almost certainly (there's no MAC, so this is the failure mode
	// Jasypt itself relies on to reject bad passwords).
	_, err = jasyptDecrypt("wrong-password", cipherB64)
	require.Error(t, err)
}

func TestJasyptDecryptInvalidInputs(t *testing.T) {
	_, err := jasyptDecrypt("pw", "not-base64!!!")
	require.Error(t, err)

	// valid base64 but too short to contain an 8-byte salt + any ciphertext
	_, err = jasyptDecrypt("pw", "AQIDBAU=")
	require.Error(t, err)
}

func TestParseNacosKeyValueConfig(t *testing.T) {
	content := "" +
		"# comment line\n" +
		"\n" +
		"MINIO_EP = minio.example.com\n" +
		"MINIO_AK=AKIDEXAMPLE\n" +
		"  MINIO_SK = topsecret  \n" +
		"NOT_A_KV_LINE_NO_EQUALS\n"

	values := parseNacosKeyValueConfig(content)
	require.Equal(t, "minio.example.com", values["MINIO_EP"])
	require.Equal(t, "AKIDEXAMPLE", values["MINIO_AK"])
	require.Equal(t, "topsecret", values["MINIO_SK"])
	require.NotContains(t, values, "NOT_A_KV_LINE_NO_EQUALS")
	require.NotContains(t, values, "# comment line")
}

func TestApplyNacosMinioConfigNoopWithoutJasyptPassword(t *testing.T) {
	t.Setenv("BOOTSTRAP_JASYPT_ENCRYPTOR_PASSWORD", "")
	conf := &Conf{}
	conf.setDefaults()
	before := *conf

	applyNacosMinioConfig(conf, nilLogger{})
	require.Equal(t, before.NetStorageMinioEndpoint, conf.NetStorageMinioEndpoint)
	require.Equal(t, before.NetStorageMinioAccessKey, conf.NetStorageMinioAccessKey)
}

func TestApplyNacosMinioConfigFallsBackOnFetchError(t *testing.T) {
	// A Jasypt password is set but Nacos connection info isn't, so the
	// fetch fails fast (no network attempt) - applyNacosMinioConfig must
	// swallow the error and leave conf untouched rather than panicking or
	// blocking startup.
	t.Setenv("BOOTSTRAP_JASYPT_ENCRYPTOR_PASSWORD", "some-password")
	t.Setenv("BOOTSTRAP_NACOS_CONFIG_SERVER", "")
	conf := &Conf{}
	conf.setDefaults()
	before := *conf

	applyNacosMinioConfig(conf, nilLogger{})
	require.Equal(t, before.NetStorageMinioEndpoint, conf.NetStorageMinioEndpoint)
}

func TestNacosFetchMinioConfigRequiresServer(t *testing.T) {
	t.Setenv("BOOTSTRAP_NACOS_CONFIG_SERVER", "")
	_, err := nacosFetchMinioConfig("any-key")
	require.ErrorContains(t, err, "BOOTSTRAP_NACOS_CONFIG_SERVER")
}

func TestNacosFetchMinioConfigInvalidPort(t *testing.T) {
	t.Setenv("BOOTSTRAP_NACOS_CONFIG_SERVER", "127.0.0.1")
	t.Setenv("BOOTSTRAP_NACOS_CONFIG_SERVER_PORT", "not-a-port")
	_, err := nacosFetchMinioConfig("any-key")
	require.ErrorContains(t, err, "BOOTSTRAP_NACOS_CONFIG_SERVER_PORT")
}

func TestNacosFetchMinioConfigBadJasyptPassword(t *testing.T) {
	salt := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	cipherB64, err := jasyptEncrypt("real-password", "nacos-login-password", salt)
	require.NoError(t, err)

	t.Setenv("BOOTSTRAP_NACOS_CONFIG_SERVER", "127.0.0.1")
	t.Setenv("BOOTSTRAP_NACOS_CONFIG_PASSWORD", cipherB64)

	_, err = nacosFetchMinioConfig("wrong-password")
	require.ErrorContains(t, err, "jasypt decrypt")
}
