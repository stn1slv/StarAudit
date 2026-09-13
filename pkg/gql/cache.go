// Package gql provides functions to fetch data from the GitHub GraphQL API.
package gql

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/stn1slv/staraudit/v2/pkg/context"
)

// getCache returns the cached response for the given key, or a nil response
// if there is no entry for it yet. The name of the cache file is always
// returned so that callers can discard an entry whose contents turn out to
// be unusable.
func getCache(ctx *context.Context, key string) (*http.Response, string, error) {
	filename := cacheEntryFilename(ctx, key)

	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		return nil, filename, fmt.Errorf("unable to create cache directory: %w", err)
	}

	body, err := os.ReadFile(filename) // #nosec G304
	if err != nil {
		if os.IsNotExist(err) {
			return nil, filename, nil
		}
		return nil, filename, fmt.Errorf("unable to read cache file: %w", err)
	}

	return &http.Response{Body: io.NopCloser(bytes.NewReader(body))}, filename, nil
}

// putCache stores the supplied response body in the cache. The body is
// written to a temporary file and then renamed into place, so that an
// interrupted run cannot leave a truncated entry behind.
func putCache(ctx *context.Context, key string, body []byte) error {
	filename := cacheEntryFilename(ctx, key)

	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		return fmt.Errorf("unable to create cache directory: %w", err)
	}

	f, err := os.CreateTemp(filepath.Dir(filename), ".tmp-*")
	if err != nil {
		return fmt.Errorf("unable to create cache file: %w", err)
	}

	// Removing the temporary file is a no-op once it has been renamed.
	defer func() { _ = os.Remove(f.Name()) }()

	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("unable to write response in cache file: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("unable to flush cache file: %w", err)
	}

	if err := os.Rename(f.Name(), filename); err != nil {
		return fmt.Errorf("unable to move cache file into place: %w", err)
	}

	return nil
}

// cacheEntryFilename builds the path of the cache entry for the given key,
// in a subdirectory dedicated to the scanned repository.
//
// The key is hashed rather than sanitized because GitHub cursors are base64
// values: stripping the characters that are awkward in a filename, or
// relying on a case-insensitive filesystem, would let two distinct cursors
// collide and serve one page's data in place of another.
func cacheEntryFilename(ctx *context.Context, key string) string {
	sum := sha256.Sum256([]byte(key))

	return filepath.Join(ctx.CacheDirectoryPath, ctx.RepoOwner, ctx.RepoName, hex.EncodeToString(sum[:]))
}

// listFilePagination generates the cache key suffix for stargazer lists.
func listFilePagination(cursor string) string {
	if cursor == "" {
		return "-list-firstpage"
	}

	return fmt.Sprintf("-list-%s", cursor)
}

// contribFilePagination generates the cache key suffix for user contribution data.
func contribFilePagination(cursor string, year int) string {
	if cursor == "" {
		return fmt.Sprintf("-firstpage-%d", year)
	}

	return fmt.Sprintf("-%s-%d", cursor, year)
}
