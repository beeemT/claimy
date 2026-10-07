package login

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/zalando/go-keyring"
)

const keychainService = "claimy"

var (
	errCredentialNotFound       = errors.New("login credential not found")
	currentStore          store = keyringStore{}
	userConfigDir               = os.UserConfigDir
)

type storedCredential struct {
	RefreshToken string `json:"refreshToken"`
	Server       string `json:"server"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientId"`
	Subject      string `json:"subject"`
	Email        string `json:"email"`
}

type store interface {
	Get(string) (string, error)
	Set(string, string) error
	Delete(string) error
}

type keyringStore struct{}

func (keyringStore) Get(key string) (string, error) {
	value, err := keyring.Get(keychainService, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", errCredentialNotFound
	}

	return value, err
}

func (keyringStore) Set(key, value string) error {
	if runtime.GOOS == "darwin" && darwinCommandLength(key, value) > 4096 {
		return errors.New("login credential exceeds the macOS keychain command limit")
	}

	return keyring.Set(keychainService, key, value)
}

func (keyringStore) Delete(key string) error {
	err := keyring.Delete(keychainService, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return errCredentialNotFound
	}

	return err
}

func darwinCommandLength(key, value string) int {
	encodedLength := len("go-keyring-base64:") + base64.StdEncoding.EncodedLen(len(value))

	return len("add-generic-password -U -s ") + shellQuoteLength(keychainService) +
		len(" -a ") + shellQuoteLength(key) + len(" -w ") + encodedLength + len("\n")
}

func shellQuoteLength(value string) int {
	if value == "" {
		return 2
	}
	unsafe := false
	quotes := 0
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		case strings.ContainsRune("_@%+=:,./-", char):
		default:
			unsafe = true
			if char == '\'' {
				quotes++
			}
		}
	}
	if !unsafe {
		return len(value)
	}

	return len(value) + 2 + quotes*4
}

func (credential storedCredential) validFor(server string) bool {
	if credential.RefreshToken == "" || credential.Server != server || credential.ClientID == "" || strings.TrimSpace(credential.Subject) == "" || credential.Email == "" || credential.Email != canonicalEmail(credential.Email) {
		return false
	}
	issuer, err := normalizeIssuer(credential.Issuer)

	return err == nil && issuer == credential.Issuer
}

// DefaultURL returns the saved non-secret Claimy server URL, if one exists.
func DefaultURL() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("read login configuration failed")
	}

	return serverFromConfig(data)
}

func serverFromConfig(data []byte) (string, error) {
	var config struct {
		Server string `json:"server"`
	}
	if json.Unmarshal(data, &config) != nil || strings.TrimSpace(config.Server) == "" {
		return "", errors.New("login configuration is invalid")
	}

	return normalizeServer(config.Server)
}

func configPath() (string, error) {
	directory, err := userConfigDir()
	if err != nil {
		return "", errors.New("user configuration directory unavailable")
	}

	return filepath.Join(directory, "claimy", "config.json"), nil
}

func lockForServer(server string) (*flock.Flock, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}

	return flock.New(filepath.Join(directory, credentialKey(server)+".lock")), nil
}

func lockForConfig() (*flock.Flock, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}

	return flock.New(filepath.Join(directory, "config.lock")), nil
}

func acquireLock(ctx context.Context, lock *flock.Flock) error {
	for {
		if err := ctx.Err(); err != nil {
			return errors.New("login operation was canceled or timed out")
		}
		locked, err := lock.TryLock()
		if err != nil {
			return errors.New("acquire login lock failed")
		}
		if locked {
			if err := ctx.Err(); err != nil {
				return errors.Join(errors.New("login operation was canceled or timed out"), releaseLoginLock(lock))
			}

			return nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return errors.New("login operation was canceled or timed out")
		case <-timer.C:
		}
	}
}

func releaseLoginLock(lock *flock.Flock) error {
	if err := lock.Unlock(); err != nil {
		return errors.New("release login lock failed")
	}

	return nil
}

func credentialKey(server string) string {
	hash := sha256.Sum256([]byte(server))

	return "credential-" + base64.RawURLEncoding.EncodeToString(hash[:])
}

func normalizeServer(raw string) (string, error) {
	parsed, err := parseServerURL(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if err := normalizeServerHost(parsed); err != nil {
		return "", err
	}
	if err := normalizeServerPath(parsed); err != nil {
		return "", err
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""

	return parsed.String(), nil
}

func parseServerURL(value string) (*url.URL, error) {
	if value == "" || strings.Contains(value, "#") {
		return nil, errors.New("invalid Claimy server URL")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, errors.New("invalid Claimy server URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case httpsScheme, httpScheme:
	default:
		return nil, errors.New("claimy server URL must use HTTPS")
	}

	return parsed, nil
}

func normalizeServerHost(parsed *url.URL) error {
	host := strings.ToLower(parsed.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return errors.New("invalid Claimy server URL")
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return errors.New("invalid Claimy server URL")
	}
	if parsed.Scheme == httpScheme && !isLoopbackServerHost(host) {
		return errors.New("claimy server URL must use HTTPS")
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return errors.New("invalid Claimy server URL")
		}
		port = strconv.Itoa(number)
	}
	if parsed.Scheme == httpsScheme && port == "443" || parsed.Scheme == httpScheme && port == "80" {
		port = ""
	}
	switch {
	case strings.Contains(host, ":") && port == "":
		parsed.Host = "[" + host + "]"
	case strings.Contains(host, ":"):
		parsed.Host = net.JoinHostPort(host, port)
	case port == "":
		parsed.Host = host
	default:
		parsed.Host = net.JoinHostPort(host, port)
	}

	return nil
}

func isLoopbackServerHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func normalizeServerPath(parsed *url.URL) error {
	escapedPath := strings.TrimRight(parsed.EscapedPath(), "/")
	if escapedPath == "" {
		parsed.Path = ""
		parsed.RawPath = ""

		return nil
	}
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return errors.New("invalid Claimy server URL")
	}
	parsed.Path = decodedPath
	parsed.RawPath = ""
	if parsed.EscapedPath() != escapedPath {
		parsed.RawPath = escapedPath
	}

	return nil
}

type configSnapshot struct {
	path   string
	data   []byte
	exists bool
}

func persistLogin(ctx context.Context, server string, credential storedCredential) (persistErr error) {
	serverLock, err := lockForServer(server)
	if err != nil {
		return errors.New("create login lock failed")
	}
	if err := acquireLock(ctx, serverLock); err != nil {
		return err
	}
	defer func() {
		persistErr = errors.Join(persistErr, releaseLoginLock(serverLock))
	}()

	configLock, err := lockForConfig()
	if err != nil {
		return errors.New("create login configuration lock failed")
	}
	if err := acquireLock(ctx, configLock); err != nil {
		return err
	}
	defer func() {
		persistErr = errors.Join(persistErr, releaseLoginLock(configLock))
	}()

	snapshot, err := readConfigSnapshot()
	if err != nil {
		return err
	}
	if err := writeDefaultConfig(server); err != nil {
		return err
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return errors.Join(errors.New("save login credential failed"), restoreConfigSnapshot(snapshot))
	}
	if err := currentStore.Set(credentialKey(server), string(encoded)); err != nil {
		return errors.Join(errors.New("secure login storage unavailable"), restoreConfigSnapshot(snapshot))
	}

	return nil
}

func readConfigSnapshot() (configSnapshot, error) {
	path, err := configPath()
	if err != nil {
		return configSnapshot{}, err
	}
	snapshot := configSnapshot{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return configSnapshot{}, errors.New("read login configuration failed")
	}
	if _, err := serverFromConfig(data); err != nil {
		return configSnapshot{}, err
	}
	snapshot.data = data
	snapshot.exists = true

	return snapshot, nil
}

func writeDefaultConfig(server string) error {
	data, err := json.Marshal(struct {
		Server string `json:"server"`
	}{Server: server})
	if err != nil {
		return errors.New("save login configuration failed")
	}
	path, err := configPath()
	if err != nil {
		return err
	}

	return atomicWrite(path, data)
}

func restoreConfigSnapshot(snapshot configSnapshot) error {
	if !snapshot.exists {
		if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("restore login configuration failed")
		}

		return nil
	}
	if err := atomicWrite(snapshot.path, snapshot.data); err != nil {
		return errors.New("restore login configuration failed")
	}

	return nil
}

func atomicWrite(path string, data []byte) (writeErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("create login configuration failed")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return errors.New("secure login configuration directory failed")
	}
	file, err := os.CreateTemp(directory, ".claimy-config-*")
	if err != nil {
		return errors.New("create login configuration failed")
	}
	temporary := file.Name()
	committed := false
	defer func() {
		if !committed {
			if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
				writeErr = errors.Join(writeErr, errors.New("remove temporary login configuration failed"))
			}
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return errors.Join(errors.New("secure login configuration failed"), closeLoginConfigFile(file))
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(errors.New("write login configuration failed"), closeLoginConfigFile(file))
	}
	if err := file.Sync(); err != nil {
		return errors.Join(errors.New("write login configuration failed"), closeLoginConfigFile(file))
	}
	if err := file.Close(); err != nil {
		return errors.New("write login configuration failed")
	}
	if err := os.Rename(temporary, path); err != nil {
		return errors.New("save login configuration failed")
	}
	committed = true

	return nil
}

func closeLoginConfigFile(file *os.File) error {
	if err := file.Close(); err != nil {
		return errors.New("close login configuration failed")
	}

	return nil
}

func clearDefault(ctx context.Context, server string) (clearErr error) {
	lock, err := lockForConfig()
	if err != nil {
		return errors.New("create login configuration lock failed")
	}
	if err := acquireLock(ctx, lock); err != nil {
		return err
	}
	defer func() {
		clearErr = errors.Join(clearErr, releaseLoginLock(lock))
	}()

	defaultServer, err := DefaultURL()
	if err != nil {
		return err
	}
	if defaultServer == "" || defaultServer != server {
		return nil
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("clear login configuration failed")
	}

	return nil
}
