package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf8"
)

const maxUploadBytes = maxImportBytes

// ErrTooLarge is returned when an upload exceeds maxUploadBytes.
var ErrTooLarge = errors.New("file too large")

// SaveUploadedCsv stores an uploaded CSV file in the CSV directory under
// name. Existing files are never overwritten (409), and the file appears
// atomically: it is written to a temporary file, fsynced, then hard-linked
// into place, which fails if the name is already taken.
func (a *App) SaveUploadedCsv(name string, r io.Reader) (*CsvFile, error) {
	v := &ValidationError{}
	if !csvFilenameRe.MatchString(name) {
		v.add("file", "file name must end in .csv and use only letters, digits, '.', '_' or '-' (max 128 characters)")
		return nil, v
	}

	data, err := io.ReadAll(io.LimitReader(r, maxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	if len(data) > maxUploadBytes {
		return nil, ErrTooLarge
	}
	if len(data) == 0 {
		v.add("file", "file is empty")
		return nil, v
	}
	body := bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		v.add("file", "file must be UTF-8 text")
		return nil, v
	}

	final := filepath.Join(a.CSVDir, name)
	if _, err := os.Lstat(final); err == nil {
		return nil, &ConflictError{Message: fmt.Sprintf("a file named %q already exists", name)}
	}

	tmp, err := os.CreateTemp(a.CSVDir, ".upload-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create upload file in %s: %w", a.CSVDir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // after a successful link this only removes the temp name

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write upload: %w", err)
	}
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("chmod upload: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("sync upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close upload: %w", err)
	}

	if err := os.Link(tmpName, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, &ConflictError{Message: fmt.Sprintf("a file named %q already exists", name)}
		}
		// Some network filesystems do not support hard links. Fall back to
		// rename, which is atomic but would replace a file created in the
		// tiny window since the Lstat check above.
		if _, statErr := os.Lstat(final); statErr == nil {
			return nil, &ConflictError{Message: fmt.Sprintf("a file named %q already exists", name)}
		}
		if err := os.Rename(tmpName, final); err != nil {
			return nil, fmt.Errorf("finalise upload: %w", err)
		}
	}

	info, err := os.Stat(final)
	if err != nil {
		return nil, fmt.Errorf("stat upload: %w", err)
	}
	return &CsvFile{Filename: name, SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC()}, nil
}
