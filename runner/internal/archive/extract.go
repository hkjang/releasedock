package archive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hkjang/releasedock/runner/internal/model"
)

type Result struct {
	Files          []string
	ExtractedBytes int64
}

func Extract(ctx context.Context, archivePath, destination string, policy model.ExtractionPolicy) (Result, error) {
	if err := validatePolicy(policy); err != nil {
		return Result{}, err
	}
	info, err := os.Lstat(archivePath)
	if err != nil {
		return Result{}, fmt.Errorf("inspect archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Result{}, errors.New("archive must be a regular file (symlinks are rejected)")
	}
	if info.Size() > policy.MaxArchiveBytes {
		return Result{}, fmt.Errorf("archive size %d exceeds limit %d", info.Size(), policy.MaxArchiveBytes)
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return Result{}, fmt.Errorf("create extraction directory: %w", err)
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return Result{}, fmt.Errorf("resolve extraction directory: %w", err)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return Result{}, fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	buffered := bufio.NewReader(f)
	reader := io.Reader(buffered)
	magic, _ := buffered.Peek(2)
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return Result{}, fmt.Errorf("open gzip stream: %w", err)
		}
		defer gz.Close()
		reader = gz
	}

	tr := tar.NewReader(&contextReader{ctx: ctx, r: reader})
	result := Result{Files: make([]string, 0)}
	entryCount := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, fmt.Errorf("read tar entry: %w", err)
		}
		entryCount++
		if entryCount > policy.MaxFiles {
			return result, fmt.Errorf("archive entry count exceeds limit %d", policy.MaxFiles)
		}
		name, err := safeArchiveName(header.Name)
		if err != nil {
			return result, fmt.Errorf("unsafe tar entry %q: %w", header.Name, err)
		}
		if name == "." {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := withinRoot(root, target); err != nil {
			return result, fmt.Errorf("unsafe tar entry %q: %w", header.Name, err)
		}
		if err := ensureParents(root, filepath.Dir(target)); err != nil {
			return result, fmt.Errorf("unsafe parent for %q: %w", header.Name, err)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := makeDirectory(target); err != nil {
				return result, fmt.Errorf("create directory %q: %w", header.Name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > policy.MaxExtractedBytes-result.ExtractedBytes {
				return result, fmt.Errorf("extracted bytes exceed limit %d", policy.MaxExtractedBytes)
			}
			if err := writeRegularFile(ctx, tr, target, header.Size); err != nil {
				return result, fmt.Errorf("extract %q: %w", header.Name, err)
			}
			result.ExtractedBytes += header.Size
			result.Files = append(result.Files, name)
		case tar.TypeSymlink:
			if !policy.AllowSymlinks {
				return result, fmt.Errorf("symlink %q is not allowed", header.Name)
			}
			linkTarget, err := safeSymlinkTarget(name, header.Linkname)
			if err != nil {
				return result, fmt.Errorf("unsafe symlink %q: %w", header.Name, err)
			}
			if err := removeNonDirectory(target); err != nil {
				return result, err
			}
			if err := os.Symlink(filepath.FromSlash(linkTarget), target); err != nil {
				return result, fmt.Errorf("create symlink %q: %w", header.Name, err)
			}
		case tar.TypeXGlobalHeader:
			// archive/tar normally consumes PAX headers internally; tolerate an
			// empty global header if it is surfaced by a future Go version.
			continue
		default:
			return result, fmt.Errorf("tar entry %q has unsupported type %d", header.Name, header.Typeflag)
		}
	}
	return result, nil
}

func validatePolicy(p model.ExtractionPolicy) error {
	if p.MaxArchiveBytes <= 0 || p.MaxExtractedBytes <= 0 || p.MaxFiles <= 0 {
		return errors.New("archive byte, extracted byte, and file count limits must be positive")
	}
	return nil
}

func safeArchiveName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') {
		return "", errors.New("empty name or NUL byte")
	}
	if strings.Contains(name, "\\") {
		return "", errors.New("backslash paths are rejected")
	}
	if path.IsAbs(name) {
		return "", errors.New("absolute path")
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("path traversal")
	}
	return cleaned, nil
}

func withinRoot(root, candidate string) error {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("path escapes extraction root")
	}
	return nil
}

func ensureParents(root, parent string) error {
	if err := withinRoot(root, parent); err != nil {
		return err
	}
	rel, err := filepath.Rel(root, parent)
	if err != nil {
		return err
	}
	current := root
	if rel == "." {
		return nil
	}
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("parent is not a real directory")
		}
	}
	return nil
}

func makeDirectory(target string) error {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(target, 0o750)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("directory entry conflicts with a non-directory")
	}
	return nil
}

func removeNonDirectory(target string) error {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("entry conflicts with an existing directory")
	}
	return os.Remove(target)
}

func writeRegularFile(ctx context.Context, tr io.Reader, target string, size int64) error {
	if err := removeNonDirectory(target); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	written, copyErr := io.CopyN(f, &contextReader{ctx: ctx, r: tr}, size)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return closeErr
	}
	if written != size {
		_ = os.Remove(target)
		return io.ErrUnexpectedEOF
	}
	return nil
}

func safeSymlinkTarget(entryName, linkName string) (string, error) {
	if linkName == "" || path.IsAbs(linkName) || strings.Contains(linkName, "\\") || strings.ContainsRune(linkName, '\x00') {
		return "", errors.New("invalid symlink target")
	}
	resolved := path.Clean(path.Join(path.Dir(entryName), linkName))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", errors.New("symlink target escapes extraction root")
	}
	// Store a relative link from the link's own directory, never an absolute
	// workspace path that would leak across workspace moves.
	relative, err := filepath.Rel(filepath.FromSlash(path.Dir(entryName)), filepath.FromSlash(resolved))
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}
