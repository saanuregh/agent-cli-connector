package agent

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	releaseURLVariable = "AGENT_CLI_CONNECTOR_RELEASE_URL"
	noDownloadVariable = "AGENT_CLI_CONNECTOR_NO_DOWNLOAD"
	defaultReleaseURL  = "https://github.com/saanuregh/agent-cli-connector/releases/download/v" + LibraryVersion + "/"
	checksumsFileName  = "SHA256SUMS"
)

// releasePlatform returns the platform name the release uses for this
// operating system and architecture.
func releasePlatform() (string, error) {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	osName := map[string]string{"linux": "linux", "darwin": "macos", "windows": "windows"}[runtime.GOOS]
	if arch == "" || osName == "" || (osName == "windows" && arch != "x86_64") {
		return "", fmt.Errorf("agent: no prebuilt library for %s/%s; build it with `zig build -Doptimize=ReleaseFast` and set %s to the shared library",
			runtime.GOOS, runtime.GOARCH, libraryPathVariable)
	}
	if osName == "linux" {
		if musl, _ := filepath.Glob("/lib/ld-musl-*.so.1"); len(musl) > 0 {
			return "", fmt.Errorf("agent: the prebuilt Linux library targets glibc and the release does not cover musl; build it with `zig build -Doptimize=ReleaseFast` and set %s to the shared library",
				libraryPathVariable)
		}
	}
	return osName + "-" + arch, nil
}

// libraryFileName returns the release file name of the shared library.
func libraryFileName(platform string) string {
	switch runtime.GOOS {
	case "windows":
		return "agent-cli-connector-" + platform + ".dll"
	case "darwin":
		return "libagent-cli-connector-" + platform + ".dylib"
	default:
		return "libagent-cli-connector-" + platform + ".so"
	}
}

// cachedLibrary returns the shared library in the user cache directory,
// downloading it first unless downloading is turned off.
func cachedLibrary() (string, error) {
	platform, err := releasePlatform()
	if err != nil {
		return "", err
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("agent: no user cache directory (%w); set %s to the shared library", err, libraryPathVariable)
	}
	dir := filepath.Join(cacheRoot, "agent-cli-connector", LibraryVersion, platform)
	name := libraryFileName(platform)
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if os.Getenv(noDownloadVariable) == "1" {
		return "", fmt.Errorf("agent: the native library is not at %s and %s=1 turns downloading off; set %s to the shared library",
			path, noDownloadVariable, libraryPathVariable)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("agent: create the cache directory: %w", err)
	}
	if err := downloadVerified(releaseBaseURL(), name, path); err != nil {
		return "", fmt.Errorf("%w; set %s to a local shared library, or %s to another release", err, libraryPathVariable, releaseURLVariable)
	}
	return path, nil
}

func releaseBaseURL() string {
	base := os.Getenv(releaseURLVariable)
	if base == "" {
		return defaultReleaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base
}

var downloadClient = &http.Client{Timeout: 10 * time.Minute}

// downloadVerified downloads name from base to path. It checks the bytes
// against the SHA256SUMS of the same base, and renames a temporary file
// into place, so other processes never see a partial or unverified file.
func downloadVerified(base, name, path string) error {
	sums, err := fetch(base + checksumsFileName)
	if err != nil {
		return err
	}
	want, err := checksumOf(sums, name)
	if err != nil {
		return err
	}
	body, err := fetch(base + name)
	if err != nil {
		return err
	}
	got := sha256.Sum256(body)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("agent: %s%s does not match its SHA-256 in %s", base, name, checksumsFileName)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), name+".tmp-*")
	if err != nil {
		return fmt.Errorf("agent: save the native library: %w", err)
	}
	_, err = tmp.Write(body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("agent: save the native library: %w", err)
	}
	return nil
}

func fetch(url string) ([]byte, error) {
	resp, err := downloadClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("agent: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent: download %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("agent: download %s: %w", url, err)
	}
	return body, nil
}

// checksumOf returns the lowercase hex SHA-256 of name from a sha256sum
// listing ("<hex>  <name>", or "<hex> *<name>" in binary mode).
func checksumOf(sums []byte, name string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for scanner.Scan() {
		hash, file, ok := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
		if !ok {
			continue
		}
		file = strings.TrimPrefix(strings.TrimLeft(file, " "), "*")
		if file == name && len(hash) == sha256.Size*2 {
			return strings.ToLower(hash), nil
		}
	}
	return "", errors.New("agent: " + checksumsFileName + " has no SHA-256 for " + name)
}
