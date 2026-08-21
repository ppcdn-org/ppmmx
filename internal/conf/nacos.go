package conf

import (
	"crypto/cipher"
	"crypto/des"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// dataId/group this service publishes its Nacos config under. Defaults to
// "mmx"; override with BOOTSTRAP_NACOS_CONFIG_DATA_ID to match an existing
// deployment's Nacos config entry.
const (
	nacosDataIDDefault = "mmx"
	nacosGroup         = "DEFAULT_GROUP"
)

func nacosDataID() string {
	if v := strings.TrimSpace(DotenvValue("BOOTSTRAP_NACOS_CONFIG_DATA_ID")); v != "" {
		return v
	}
	return nacosDataIDDefault
}

const (
	jasyptIterations = 1000
	jasyptSaltSize   = 8
	jasyptBlockSize  = 8
)

// applyNacosMinioConfig fetches MinIO settings from Nacos and applies them
// to conf, if BOOTSTRAP_JASYPT_ENCRYPTOR_PASSWORD is set. It is a no-op
// (not an error) when that env var is absent - Nacos integration is opt-in.
// Fetch failures are logged and otherwise ignored so a Nacos outage never
// blocks startup; the MINIO_* env vars remain available as a fallback.
func applyNacosMinioConfig(conf *Conf, l logger.Writer) {
	aesKey := strings.TrimSpace(DotenvValue("BOOTSTRAP_JASYPT_ENCRYPTOR_PASSWORD"))
	if aesKey == "" {
		return
	}

	values, err := nacosFetchMinioConfig(aesKey)
	if err != nil {
		if l != nil {
			l.Log(logger.Warn, "nacos config fetch failed, falling back to MINIO_* env vars: %v", err)
		}
		return
	}

	if v := values["MINIO_EP"]; v != "" {
		conf.NetStorageMinioEndpoint = v
	}
	if v := values["MINIO_AK"]; v != "" {
		conf.NetStorageMinioAccessKey = v
	}
	if v := values["MINIO_SK"]; v != "" {
		conf.NetStorageMinioSecretKey = v
	}
	if v := values["MINIO_USESSL"]; v != "" {
		conf.NetStorageMinioUseSSL = v
	}
	if v := values["MINIO_URL"]; v != "" {
		conf.NetStorageMinioDomain = v
	}
}

// nacosFetchMinioConfig connects to Nacos - connection info from the
// BOOTSTRAP_NACOS_CONFIG_* env vars / bin/.env - and returns the key=value
// pairs published under dataId nacosDataID() (default "mmx", override via
// BOOTSTRAP_NACOS_CONFIG_DATA_ID). The Nacos client's
// own password (BOOTSTRAP_NACOS_CONFIG_PASSWORD) is Jasypt-encrypted
// (PBEWithMD5AndDES, matching Java's org.jasypt.util.text.BasicTextEncryptor)
// and is decrypted with aesKey (BOOTSTRAP_JASYPT_ENCRYPTOR_PASSWORD) first.
func nacosFetchMinioConfig(aesKey string) (map[string]string, error) {
	server := strings.TrimSpace(DotenvValue("BOOTSTRAP_NACOS_CONFIG_SERVER"))
	if server == "" {
		return nil, fmt.Errorf("BOOTSTRAP_NACOS_CONFIG_SERVER is not set")
	}
	portStr := strings.TrimSpace(DotenvValue("BOOTSTRAP_NACOS_CONFIG_SERVER_PORT"))
	if portStr == "" {
		portStr = "8848"
	}
	port, err := strconv.ParseUint(portStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid BOOTSTRAP_NACOS_CONFIG_SERVER_PORT: %w", err)
	}
	username := strings.TrimSpace(DotenvValue("BOOTSTRAP_NACOS_CONFIG_USERNAME"))
	namespace := strings.TrimSpace(DotenvValue("BOOTSTRAP_NACOS_CONFIG_NAMESPACE"))
	encPassword := strings.TrimSpace(DotenvValue("BOOTSTRAP_NACOS_CONFIG_PASSWORD"))

	password := ""
	if encPassword != "" {
		password, err = jasyptDecrypt(aesKey, encPassword)
		if err != nil {
			return nil, fmt.Errorf("jasypt decrypt BOOTSTRAP_NACOS_CONFIG_PASSWORD: %w", err)
		}
	}

	clientConfig := constant.ClientConfig{
		NamespaceId:         namespace,
		TimeoutMs:           5000,
		NotLoadCacheAtStart: true,
		LogDir:              "./logs",
		CacheDir:            "./cache",
		LogLevel:            "warn",
	}
	if username != "" {
		clientConfig.Username = username
	}
	if password != "" {
		clientConfig.Password = password
	}

	client, err := clients.NewConfigClient(vo.NacosClientParam{
		ClientConfig:  &clientConfig,
		ServerConfigs: []constant.ServerConfig{{IpAddr: server, Port: port}},
	})
	if err != nil {
		return nil, fmt.Errorf("create nacos client: %w", err)
	}

	content, err := client.GetConfig(vo.ConfigParam{DataId: nacosDataID(), Group: nacosGroup})
	if err != nil {
		return nil, fmt.Errorf("get nacos config: %w", err)
	}
	return parseNacosKeyValueConfig(content), nil
}

// parseNacosKeyValueConfig parses a flat "KEY=value" config body, one
// setting per line, "#"-prefixed lines and blank lines ignored.
func parseNacosKeyValueConfig(content string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

// jasyptDecrypt decrypts a Base64 ciphertext produced by Java's
// org.jasypt.util.text.BasicTextEncryptor (algorithm PBEWithMD5AndDES, 1000
// iterations): salt (8 bytes) || DES-CBC ciphertext, key/IV derived from
// repeated MD5(password||salt).
func jasyptDecrypt(password, cipherB64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(cipherB64)
	if err != nil {
		return "", fmt.Errorf("jasypt base64 decode: %w", err)
	}
	if len(raw) <= jasyptSaltSize || (len(raw)-jasyptSaltSize)%jasyptBlockSize != 0 {
		return "", errors.New("jasypt: invalid ciphertext length")
	}

	salt := raw[:jasyptSaltSize]
	encrypted := raw[jasyptSaltSize:]

	key, iv := jasyptDeriveKeyAndIV([]byte(password), salt, jasyptIterations)
	block, err := des.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("jasypt des cipher: %w", err)
	}

	decrypted := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(decrypted, encrypted)

	plain, err := jasyptPKCS5Unpad(decrypted)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// jasyptEncrypt is the inverse of jasyptDecrypt; only used by tests to
// produce known ciphertexts (Jasypt uses a random salt per call, so this
// isn't deterministic across runs).
func jasyptEncrypt(password, plaintext string, salt []byte) (string, error) {
	if len(salt) != jasyptSaltSize {
		return "", errors.New("jasypt: salt must be 8 bytes")
	}
	key, iv := jasyptDeriveKeyAndIV([]byte(password), salt, jasyptIterations)
	block, err := des.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("jasypt des cipher: %w", err)
	}
	padded := jasyptPKCS5Pad([]byte(plaintext), jasyptBlockSize)
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, padded)
	return base64.StdEncoding.EncodeToString(append(append([]byte(nil), salt...), encrypted...)), nil
}

func jasyptDeriveKeyAndIV(password, salt []byte, iterations int) (key, iv []byte) {
	h := md5.New()
	h.Write(password)
	h.Write(salt)
	dk := h.Sum(nil)
	for i := 1; i < iterations; i++ {
		sum := md5.Sum(dk)
		dk = sum[:]
	}
	return dk[:8], dk[8:16]
}

func jasyptPKCS5Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%jasyptBlockSize != 0 {
		return nil, errors.New("jasypt: invalid padded data length")
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > jasyptBlockSize || pad > len(data) {
		return nil, errors.New("jasypt: invalid PKCS5 padding")
	}
	for i := 0; i < pad; i++ {
		if data[len(data)-1-i] != byte(pad) {
			return nil, errors.New("jasypt: invalid PKCS5 padding")
		}
	}
	return data[:len(data)-pad], nil
}

func jasyptPKCS5Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}
