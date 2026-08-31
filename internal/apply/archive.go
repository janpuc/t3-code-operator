package apply

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type extensionArchiveLimits struct {
	maxBytes   int64
	maxEntries int
}

type extensionArchiveBudget struct {
	remainingBytes   int64
	remainingEntries int
	context          context.Context
}

func defaultExtensionArchiveLimits() extensionArchiveLimits {
	return extensionArchiveLimits{maxBytes: 512 << 20, maxEntries: 100_000}
}

func extractExtensionArchive(archivePath, destination string, limits extensionArchiveLimits) error {
	return extractExtensionArchiveWithBudget(archivePath, destination, newExtensionArchiveBudget(limits))
}

func extractExtensionArchiveWithBudget(
	archivePath string,
	destination string,
	budget *extensionArchiveBudget,
) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	magic := make([]byte, 4)
	count, err := io.ReadFull(archive, magic)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	switch {
	case count >= 4 && string(magic[:4]) == "PK\x03\x04":
		if err := archive.Close(); err != nil {
			return err
		}
		return extractExtensionZipWithBudget(archivePath, destination, budget)
	case count >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		compressed, err := gzip.NewReader(archive)
		if err != nil {
			return err
		}
		defer compressed.Close()
		return extractExtensionTarWithBudget(compressed, destination, budget)
	default:
		return extractExtensionTarWithBudget(archive, destination, budget)
	}
}

func extractExtensionTar(reader io.Reader, destination string, limits extensionArchiveLimits) error {
	return extractExtensionTarWithBudget(reader, destination, newExtensionArchiveBudget(limits))
}

func extractExtensionTarWithBudget(reader io.Reader, destination string, budget *extensionArchiveBudget) error {
	archive := tar.NewReader(budget.wrapReader(reader))
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Extension tar archive: %w", err)
		}
		if err := budget.consumeEntry(); err != nil {
			return err
		}
		target, skip, err := extensionArchiveTarget(destination, header.Name)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := createExtensionArchiveDirectory(destination, target); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := budget.consumeBytes(header.Size); err != nil {
				return err
			}
			if err := writeExtensionArchiveFile(destination, target, budget.wrapReader(archive), header.Size, fs.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := budget.consumeBytes(int64(len(header.Linkname))); err != nil {
				return err
			}
			if err := writeExtensionArchiveLink(destination, target, header.Linkname); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeLink:
			return fmt.Errorf("Extension archive contains unsupported hard link %s", header.Name)
		default:
			return fmt.Errorf("Extension archive contains unsupported entry %s", header.Name)
		}
	}
}

func extractExtensionZip(archivePath, destination string, limits extensionArchiveLimits) error {
	return extractExtensionZipWithBudget(archivePath, destination, newExtensionArchiveBudget(limits))
}

func extractExtensionZipWithBudget(archivePath, destination string, budget *extensionArchiveBudget) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if err := budget.consumeEntry(); err != nil {
			return err
		}
		target, skip, err := extensionArchiveTarget(destination, entry.Name)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		mode := entry.Mode()
		switch {
		case entry.FileInfo().IsDir():
			if err := createExtensionArchiveDirectory(destination, target); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			if entry.UncompressedSize64 > uint64(^uint64(0)>>1) {
				return errors.New("Extension archive link target is too large")
			}
			if err := budget.consumeBytes(int64(entry.UncompressedSize64)); err != nil {
				return err
			}
			reader, err := entry.Open()
			if err != nil {
				return err
			}
			linkTarget, readErr := io.ReadAll(io.LimitReader(budget.wrapReader(reader), int64(entry.UncompressedSize64)+1))
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				return errors.Join(readErr, closeErr)
			}
			if uint64(len(linkTarget)) != entry.UncompressedSize64 {
				return errors.New("Extension archive link target is too large")
			}
			if err := writeExtensionArchiveLink(destination, target, string(linkTarget)); err != nil {
				return err
			}
		case mode.IsRegular():
			if entry.UncompressedSize64 > uint64(^uint64(0)>>1) {
				return errors.New("Extension archive exceeds its extracted size limit")
			}
			if err := budget.consumeBytes(int64(entry.UncompressedSize64)); err != nil {
				return err
			}
			reader, err := entry.Open()
			if err != nil {
				return err
			}
			writeErr := writeExtensionArchiveFile(
				destination,
				target,
				budget.wrapReader(reader),
				int64(entry.UncompressedSize64),
				mode,
			)
			closeErr := reader.Close()
			if writeErr != nil || closeErr != nil {
				return errors.Join(writeErr, closeErr)
			}
		default:
			return fmt.Errorf("Extension archive contains unsupported entry %s", entry.Name)
		}
	}
	return nil
}

func newExtensionArchiveBudget(limits extensionArchiveLimits) *extensionArchiveBudget {
	return &extensionArchiveBudget{
		remainingBytes:   limits.maxBytes,
		remainingEntries: limits.maxEntries,
	}
}

func newExtensionArchiveBudgetWithContext(limits extensionArchiveLimits, ctx context.Context) *extensionArchiveBudget {
	budget := newExtensionArchiveBudget(limits)
	budget.context = ctx
	return budget
}

func (budget *extensionArchiveBudget) consumeEntry() error {
	if err := budget.contextError(); err != nil {
		return err
	}
	if budget == nil || budget.remainingEntries <= 0 {
		return errors.New("Extension archive contains too many entries")
	}
	budget.remainingEntries--
	return nil
}

func (budget *extensionArchiveBudget) consumeBytes(size int64) error {
	if err := budget.contextError(); err != nil {
		return err
	}
	if budget == nil || size < 0 || size > budget.remainingBytes {
		return errors.New("Extension archive exceeds its extracted size limit")
	}
	budget.remainingBytes -= size
	return nil
}

func (budget *extensionArchiveBudget) contextError() error {
	if budget == nil || budget.context == nil {
		return nil
	}
	return budget.context.Err()
}

func (budget *extensionArchiveBudget) wrapReader(reader io.Reader) io.Reader {
	if budget == nil || budget.context == nil {
		return reader
	}
	return &extensionContextReader{context: budget.context, reader: reader}
}

type extensionContextReader struct {
	context context.Context
	reader  io.Reader
}

func (reader *extensionContextReader) Read(buffer []byte) (int, error) {
	if err := reader.context.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func extensionArchiveTarget(root, name string) (string, bool, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, `\`) || path.IsAbs(name) {
		return "", false, fmt.Errorf("Extension archive contains unsafe path %q", name)
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".." {
			return "", false, fmt.Errorf("Extension archive path %q contains a parent traversal", name)
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		return root, true, nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("Extension archive path %q escapes its destination", name)
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	if !isPathWithin(root, target) {
		return "", false, fmt.Errorf("Extension archive path %q escapes its destination", name)
	}
	return target, false, nil
}

func createExtensionArchiveDirectory(root, target string) error {
	if err := rejectSymlinkComponents(root, target); err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Extension archive path %s collides with an existing entry", target)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	return rejectSymlinkComponents(root, target)
}

func writeExtensionArchiveFile(root, target string, reader io.Reader, size int64, mode fs.FileMode) (result error) {
	if err := createExtensionArchiveDirectory(root, filepath.Dir(target)); err != nil {
		return err
	}
	if err := rejectSymlinkComponents(root, target); err != nil {
		return err
	}
	permission := mode.Perm() & 0o777
	permission |= 0o400
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, permission)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			result = errors.Join(result, closeErr)
		}
	}()
	written, err := io.CopyN(file, reader, size)
	if err != nil {
		return err
	}
	if written != size {
		return io.ErrUnexpectedEOF
	}
	return file.Sync()
}

func writeExtensionArchiveLink(root, target, linkTarget string) error {
	if linkTarget == "" || len(linkTarget) > 4096 || filepath.IsAbs(linkTarget) || strings.ContainsRune(linkTarget, '\x00') {
		return fmt.Errorf("Extension archive link %s has an unsafe target", target)
	}
	for _, component := range strings.Split(filepath.ToSlash(linkTarget), "/") {
		if component == ".." {
			return fmt.Errorf("Extension archive link %s contains a parent traversal", target)
		}
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(target), filepath.FromSlash(linkTarget)))
	if !isPathWithin(root, resolved) {
		return fmt.Errorf("Extension archive link %s escapes its destination", target)
	}
	if err := createExtensionArchiveDirectory(root, filepath.Dir(target)); err != nil {
		return err
	}
	if err := rejectSymlinkComponents(root, target); err != nil {
		return err
	}
	return os.Symlink(filepath.FromSlash(linkTarget), target)
}
