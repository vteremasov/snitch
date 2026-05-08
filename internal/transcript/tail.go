package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// LineFunc receives one decoded JSON object per transcript line.
type LineFunc func(map[string]any)

// Tail blocks until ctx is done. It waits for the file to appear, seeks to
// the current end (so historical content from a resumed session does not
// replay through onLine), then watches for appends. Partial trailing lines
// are buffered until terminated with '\n'.
func Tail(ctx context.Context, path string, onLine LineFunc) error {
	if err := waitForFile(ctx, path); err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	if err := w.Add(path); err != nil {
		// File may have been rotated/replaced; watch the parent dir as fallback.
		if err := w.Add(filepath.Dir(path)); err != nil {
			return err
		}
	}

	t := &tailer{f: f}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op&fsnotify.Write != 0 || ev.Op&fsnotify.Create != 0 {
				if err := t.drain(onLine); err != nil && !errors.Is(err, io.EOF) {
					return err
				}
			}
		case _, ok := <-w.Errors:
			if !ok {
				return nil
			}
		}
	}
}

func waitForFile(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type tailer struct {
	f   *os.File
	buf []byte
}

func (t *tailer) drain(onLine LineFunc) error {
	chunk := make([]byte, 8192)
	for {
		n, err := t.f.Read(chunk)
		if n > 0 {
			t.buf = append(t.buf, chunk[:n]...)
			for {
				idx := bytes.IndexByte(t.buf, '\n')
				if idx < 0 {
					break
				}
				line := t.buf[:idx]
				t.buf = t.buf[idx+1:]
				if len(line) == 0 {
					continue
				}
				var m map[string]any
				if json.Unmarshal(line, &m) == nil {
					onLine(m)
				}
			}
		}
		if err != nil {
			return err
		}
	}
}
