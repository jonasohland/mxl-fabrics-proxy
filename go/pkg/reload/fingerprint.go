package reload

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// unreadable is mixed into the fingerprint in place of the contents of a file
// or directory that cannot be read. It is distinct from the contents of an
// empty file.
var unreadable = []byte("\x00unreadable")

// isConfigFile mirrors the file selection the proxy performs in loadFromDisk.
func isConfigFile(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// expand resolves a single config path into the list of files the proxy would
// load for it. A path ending in .yaml/.yml is taken as a file, anything else is
// read as a directory, non-recursively.
func expand(path string) ([]string, error) {
	if isConfigFile(path) {
		return []string{path}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, entry := range entries {
		if isConfigFile(entry.Name()) {
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}

	return files, nil
}

func writeChunk(h hash.Hash, data []byte) {
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(data)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(data)
}

// Fingerprint hashes the contents of every configuration file the proxy would
// load for the given paths.
//
// Paths that cannot be listed or read are not an error: they contribute a
// marker to the fingerprint instead. A config volume appearing, disappearing or
// having a file removed all register as changes, and a read that races with an
// atomic update of the underlying files resolves itself on a later poll.
func Fingerprint(paths []string) string {
	sum := sha256.New()

	var files []string
	for _, path := range paths {
		expanded, err := expand(path)
		if err != nil {
			slog.Debug("cannot list config path", "path", path, "error", err)
			writeChunk(sum, []byte(path))
			writeChunk(sum, unreadable)
			continue
		}

		files = append(files, expanded...)
	}

	slices.Sort(files)
	files = slices.Compact(files)

	for _, file := range files {
		writeChunk(sum, []byte(file))

		contents, err := os.ReadFile(file)
		if err != nil {
			slog.Debug("cannot read config file", "path", file, "error", err)
			writeChunk(sum, unreadable)
			continue
		}

		writeChunk(sum, contents)
	}

	return hex.EncodeToString(sum.Sum(nil))
}

func short(fingerprint string) string {
	if len(fingerprint) > 12 {
		return fingerprint[:12]
	}

	return fingerprint
}
