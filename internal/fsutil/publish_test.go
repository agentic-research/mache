package fsutil

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// onlyEntries lists dir, so a test can assert Publish left nothing behind.
func onlyEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestPublish_FinalKeepsTheOldFileUntilTheRename(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.db")
	require.NoError(t, os.WriteFile(final, []byte("old"), 0o644))

	require.NoError(t, Publish(final, "", func(tmp string) error {
		// Mid-produce, a reader of final must still see the previous file.
		got, err := os.ReadFile(final)
		require.NoError(t, err)
		assert.Equal(t, "old", string(got), "final changed before the producer finished")
		return os.WriteFile(tmp, []byte("new"), 0o644)
	}))

	got, err := os.ReadFile(final)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
	assert.Equal(t, []string{"out.db"}, onlyEntries(t, dir), "the work directory was left behind")
}

func TestPublish_AFailedProducerLeavesFinalUntouched(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.db")
	require.NoError(t, os.WriteFile(final, []byte("old"), 0o644))
	boom := errors.New("boom")

	err := Publish(final, "", func(tmp string) error {
		require.NoError(t, os.WriteFile(tmp, []byte("half"), 0o644))
		return boom
	})
	require.ErrorIs(t, err, boom)

	got, err := os.ReadFile(final)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
	assert.Equal(t, []string{"out.db"}, onlyEntries(t, dir))
}

// A database whose WAL still holds committed pages is not the main file alone.
// Renaming just the main file would publish it without those writes.
func TestPublish_RefusesToLeaveDependentStateBehind(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.db")
	require.NoError(t, os.WriteFile(final, []byte("old"), 0o644))

	err := Publish(final, "", func(tmp string) error {
		require.NoError(t, os.WriteFile(tmp, []byte("new"), 0o644))
		return os.WriteFile(tmp+"-wal", []byte("pages"), 0o644)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out.db-wal")

	got, err := os.ReadFile(final)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
	assert.Equal(t, []string{"out.db"}, onlyEntries(t, dir))
}

func TestPublish_ProducerThatWritesNothingIsAnError(t *testing.T) {
	final := filepath.Join(t.TempDir(), "out.db")
	err := Publish(final, "", func(string) error { return nil })
	require.Error(t, err)
	assert.NoFileExists(t, final)
}

func TestPublish_SeedStartsTheProducerFromACopy(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.db")
	require.NoError(t, os.WriteFile(final, []byte("v1"), 0o644))

	require.NoError(t, Publish(final, final, func(tmp string) error {
		got, err := os.ReadFile(tmp)
		require.NoError(t, err)
		assert.Equal(t, "v1", string(got), "the producer did not start from the seed")
		return os.WriteFile(tmp, append(got, "+v2"...), 0o644)
	}))

	got, err := os.ReadFile(final)
	require.NoError(t, err)
	assert.Equal(t, "v1+v2", string(got))
}

// An open reader keeps the file it opened: that is what lets a long-lived
// server keep reading a db that a rebuild replaces underneath it.
func TestPublish_AnOpenReaderKeepsTheFileItOpened(t *testing.T) {
	final := filepath.Join(t.TempDir(), "out.db")
	require.NoError(t, os.WriteFile(final, []byte("old"), 0o644))
	reader, err := os.Open(final)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	require.NoError(t, Publish(final, "", func(tmp string) error {
		return os.WriteFile(tmp, []byte("new"), 0o644)
	}))

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}

func TestPublish_ThroughASymlinkReplacesTheTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.db")
	link := filepath.Join(dir, "link.db")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o644))
	require.NoError(t, os.Symlink(target, link))

	require.NoError(t, Publish(link, "", func(tmp string) error {
		return os.WriteFile(tmp, []byte("new"), 0o644)
	}))

	dest, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, target, dest, "the symlink itself was replaced")
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}

func TestWriteFileAtomic_KeepsOwnerOnlyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	require.NoError(t, WriteFileAtomic(path, []byte("{}")))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
