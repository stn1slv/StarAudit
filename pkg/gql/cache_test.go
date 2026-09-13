package gql

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stn1slv/staraudit/v2/pkg/context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testContext(t *testing.T) *context.Context {
	t.Helper()

	return &context.Context{
		RepoOwner:          "ullaakut",
		RepoName:           "staraudit",
		GithubToken:        "fakeToken",
		CacheDirectoryPath: t.TempDir(),
	}
}

func TestCacheEntryFilename(t *testing.T) {
	ctx := testContext(t)

	filename := cacheEntryFilename(ctx, "-list-firstpage")

	assert.Equal(t, filepath.Join(ctx.CacheDirectoryPath, "ullaakut", "staraudit"), filepath.Dir(filename),
		"cache entries must be scoped to the scanned repository")
	assert.Equal(t, filename, cacheEntryFilename(ctx, "-list-firstpage"),
		"the same key must always map to the same file")
}

// TestCacheEntryFilenameAvoidsCollisions covers keys that a sanitizing scheme
// would map onto the same file. GitHub cursors are base64, so they differ by
// exactly these characters, and a collision silently serves one page of
// stargazers in place of another.
func TestCacheEntryFilenameAvoidsCollisions(t *testing.T) {
	ctx := testContext(t)

	keys := []string{
		"-list-Y3Vyc29yOnYyOpIAzg0D+sk=",
		"-list-Y3Vyc29yOnYyOpIAzg0D/sk=",
		"-list-Y3Vyc29yOnYyOpIAzg0D-sk=",
		"-list-Y3Vyc29yOnYyOpIAzg0Dsk",
		// Differs only by case, which collides on a case-insensitive filesystem.
		"-list-Y3Vyc29yOnYyOpIAZG0D+sk=",
		"-list-Y3Vyc29yOnYyOpIAzg0D+sk=-2019",
	}

	seen := make(map[string]string, len(keys))
	for _, key := range keys {
		filename := cacheEntryFilename(ctx, key)

		if previous, collides := seen[filename]; collides {
			t.Errorf("keys %q and %q map to the same cache file %q", previous, key, filename)
		}

		seen[filename] = key
	}
}

func TestPutCacheThenGetCache(t *testing.T) {
	ctx := testContext(t)

	require.NoError(t, putCache(ctx, "-list-firstpage", []byte(`{"data":{}}`)))

	resp, _, err := getCache(ctx, "-list-firstpage")
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"data":{}}`, string(body))

	// A different key must not resolve to the entry that was just written.
	other, _, err := getCache(ctx, "-list-secondpage") // nolint:bodyclose // there is no body to close on a miss.
	require.NoError(t, err)
	assert.Nil(t, other)
}

func TestGetCacheReturnsNilOnMiss(t *testing.T) {
	ctx := testContext(t)

	resp, filename, err := getCache(ctx, "-list-firstpage") // nolint:bodyclose // there is no body to close on a miss.

	require.NoError(t, err, "a cache miss must not be an error")
	assert.Nil(t, resp)
	assert.NotEmpty(t, filename, "the filename is needed to discard a bad entry")
}

// TestPutCacheIsAtomic verifies that no partially written entry is left in the
// cache directory, which is what makes a corrupt entry possible in the first place.
func TestPutCacheIsAtomic(t *testing.T) {
	ctx := testContext(t)

	require.NoError(t, putCache(ctx, "-list-firstpage", []byte(`{"data":{}}`)))

	entries, err := os.ReadDir(filepath.Join(ctx.CacheDirectoryPath, "ullaakut", "staraudit"))
	require.NoError(t, err)

	require.Len(t, entries, 1, "only the final cache entry should remain")
	assert.Equal(t, filepath.Base(cacheEntryFilename(ctx, "-list-firstpage")), entries[0].Name())
}
