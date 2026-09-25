package logs

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"time"
)

// FileSource tails a log file — the Kubernetes-friendly option (share an
// emptyDir for node stdout) and also works for json-file docker logs bind
// mounts. Polls rather than fsnotify so it survives rotations and doesn't
// need per-OS plumbing.
type FileSource struct {
	path string
	tail bool // first open starts at EOF, not at the file's start
}

// NewFileSource tails path. Historical lines are skipped — we only alert on
// new output (a crash-loop replay of old panic lines is handled by docker's
// log anyway).
func NewFileSource(path string) (*FileSource, error) {
	if path == "" {
		return nil, fmt.Errorf("logs: file source needs a file path")
	}
	return &FileSource{path: path, tail: true}, nil
}

func (s *FileSource) String() string { return "file:" + s.path }

// Stream blocks emitting new lines until ctx ends. Handles the file being
// created late, truncated, or rotated (size shrink → reopen from 0).
func (s *FileSource) Stream(ctx context.Context, out chan<- string) error {
	var f *os.File
	var off int64
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	poll := time.NewTicker(time.Second)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
		}

		if f == nil {
			var err error
			f, err = os.Open(s.path)
			if err != nil {
				continue // file may not exist yet — keep polling
			}
			st, _ := f.Stat()
			if s.tail {
				off = st.Size() // start at EOF — only alert on new lines
				s.tail = false
			}
		}

		st, err := f.Stat()
		if err != nil {
			f.Close()
			f = nil
			continue
		}
		if st.Size() < off { // truncated/rotated — reopen from the top
			off = 0
		}
		if _, err := f.Seek(off, 0); err != nil {
			f.Close()
			f = nil
			continue
		}
		rd := bufio.NewReaderSize(f, 64<<10)
		for {
			line, err := rd.ReadString('\n')
			if err == nil {
				off += int64(len(line))
				if !emit(ctx, out, line[:len(line)-1]) {
					return nil
				}
				continue
			}
			// io.EOF: `line` is an unterminated tail — don't advance off, so
			// the next Seek(off) re-reads it once the newline lands.
			break
		}
	}
}
