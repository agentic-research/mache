// Package fsutil holds mache's file-publishing primitives: how a file gets
// from "being written" to "visible at its path" without a reader ever seeing
// the part in between.
package fsutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Publish produces a file at a private path and moves it to final in one
// rename, so a reader of final sees the previous file or the finished one —
// never a truncated, half-written, or missing one.
//
// produce is handed a path that does not exist yet unless seed is set, in
// which case it starts as a copy of seed (an incremental update copies the
// current final and edits the copy). It may open, write and close the path
// however it likes; SQLite writers included.
//
// The work happens in a private directory beside final, for two reasons. A
// rename is only atomic within one filesystem, and TMPDIR is often another.
// And a directory of its own makes the publish check structural: when produce
// returns, that directory must hold exactly the one file. Anything else there
// — an uncheckpointed `-wal`, a hot `-journal` — is state the file depends on,
// and renaming the file without it would publish a database missing committed
// writes. That is refused, not guessed at.
//
// A reader that already has final open keeps reading the file it opened: the
// rename replaces the name, not the inode. Seeing the new file means reopening.
//
// When final is a symlink, its target is replaced and the link is left alone,
// as an in-place write through the link would have done.
func Publish(final, seed string, produce func(path string) error) error {
	final, err := resolveTarget(final)
	if err != nil {
		return err
	}
	dir, base := filepath.Split(final)
	if dir == "" {
		dir = "."
	}
	work, err := os.MkdirTemp(dir, "."+base+".publish-*")
	if err != nil {
		return fmt.Errorf("publish %s: %w", final, err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	tmp := filepath.Join(work, base)
	if seed != "" {
		if err := CopyFile(seed, tmp); err != nil {
			return fmt.Errorf("publish %s: seed from %s: %w", final, seed, err)
		}
	}
	if err := produce(tmp); err != nil {
		return err
	}
	if err := requireAlone(work, base); err != nil {
		return fmt.Errorf("publish %s: %w", final, err)
	}
	if err := syncPath(tmp); err != nil {
		return fmt.Errorf("publish %s: sync: %w", final, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("publish %s: %w", final, err)
	}
	// The rename is durable only once the directory entry is.
	if err := syncPath(dir); err != nil {
		return fmt.Errorf("publish %s: sync dir: %w", final, err)
	}
	return nil
}

// WriteFileAtomic writes data to path through Publish. The file is created
// 0o600, as the os.CreateTemp-based version this replaced did.
func WriteFileAtomic(path string, data []byte) error {
	return Publish(path, "", func(tmp string) error {
		return os.WriteFile(tmp, data, 0o600)
	})
}

// CopyFile copies src to dst, creating or truncating dst. It is NOT atomic;
// wrap it in Publish when a reader may be looking at dst.
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// resolveTarget follows final to the file a write through it would land on.
// A final that does not exist yet is returned as-is.
func resolveTarget(final string) (string, error) {
	resolved, err := filepath.EvalSymlinks(final)
	if err == nil {
		return resolved, nil
	}
	if os.IsNotExist(err) {
		return final, nil
	}
	return "", fmt.Errorf("publish %s: %w", final, err)
}

// requireAlone fails unless dir holds exactly the file named base.
func requireAlone(dir, base string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var extra []string
	found := false
	for _, e := range entries {
		if e.Name() == base {
			found = true
			continue
		}
		extra = append(extra, e.Name())
	}
	if !found {
		return fmt.Errorf("producer did not create the file")
	}
	if len(extra) > 0 {
		return fmt.Errorf("producer left %s beside the file; publishing the file alone would drop state it depends on",
			strings.Join(extra, ", "))
	}
	return nil
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
