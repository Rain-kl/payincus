// Port of server/src/storage/path.ts.
package storage

import (
	"errors"
	"strings"
)

// NormalizeStorageBasePath normalizes a base directory path and rejects traversal
// or backslash/NUL characters, matching normalizeStorageBasePath.
func NormalizeStorageBasePath(basePath string) (string, error) {
	raw := strings.TrimSpace(basePath)
	if raw == "" {
		return "", nil
	}
	if strings.ContainsRune(raw, 0) || strings.ContainsRune(raw, '\\') {
		return "", errors.New("Storage base path contains invalid characters")
	}
	absolute := strings.HasPrefix(raw, "/")
	parts := strings.Split(raw, "/")
	segments := make([]string, 0, len(parts))
	for _, seg := range parts {
		if seg == "" {
			continue
		}
		if seg == "." || seg == ".." {
			return "", errors.New("Storage base path cannot contain relative path segments")
		}
		segments = append(segments, seg)
	}
	if len(segments) == 0 {
		if absolute {
			return "/", nil
		}
		return "", nil
	}
	prefix := ""
	if absolute {
		prefix = "/"
	}
	return prefix + strings.Join(segments, "/") + "/", nil
}

// NormalizeStorageFilename validates and returns a single-segment remote filename.
func NormalizeStorageFilename(filename string) (string, error) {
	raw := strings.TrimSpace(filename)
	if raw == "" {
		return "", errors.New("Storage filename cannot be empty")
	}
	if strings.ContainsRune(raw, 0) ||
		strings.ContainsRune(raw, '/') ||
		strings.ContainsRune(raw, '\\') ||
		raw == "." || raw == ".." {
		return "", errors.New("Storage filename contains invalid path segments")
	}
	if len(raw) > 255 {
		return "", errors.New("Storage filename is too long")
	}
	return raw, nil
}

// JoinStorageRemotePath joins basePath and filename with full path safety checks.
func JoinStorageRemotePath(basePath, filename string) (string, error) {
	normBase, err := NormalizeStorageBasePath(basePath)
	if err != nil {
		return "", err
	}
	normFile, err := NormalizeStorageFilename(filename)
	if err != nil {
		return "", err
	}
	return normBase + normFile, nil
}

// JoinStorageRemoteDirectory joins basePath with an optional relative subPath.
func JoinStorageRemoteDirectory(basePath, subPath string) (string, error) {
	normBase, err := NormalizeStorageBasePath(basePath)
	if err != nil {
		return "", err
	}
	rawSub := strings.TrimSpace(subPath)
	if rawSub == "" {
		return normBase, nil
	}
	normSub, err := NormalizeStorageBasePath(rawSub)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(normSub, "/") {
		return "", errors.New("Storage sub path must be relative")
	}
	return normBase + normSub, nil
}
