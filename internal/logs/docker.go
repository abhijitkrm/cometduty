package logs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DockerSource streams a container's stdout+stderr through the Docker Engine
// API — over a unix socket or a TCP endpoint (a read-only socket proxy is
// the recommended deployment; the raw socket is effectively root).
type DockerSource struct {
	host      string // http://... base URL (dummy host ok for unix sockets)
	container string
	client    *http.Client
	tail      int       // lines to read back at first connect
	last      time.Time // the newest line read — a reconnect skips up to it
}

// resumeTail is how far back a reconnect reads. Docker's since= would be
// exact, but on the json-file driver it reads the log from its start: GBs
// on a node without log rotation, on every reconnect. tail= seeks from the
// end; lines up to the last one seen are dropped.
const resumeTail = 500

// streamClient bounds connecting and the response headers, never the body:
// a followed log stream lasts as long as the container.
func streamClient(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	tr := &http.Transport{ResponseHeaderTimeout: 30 * time.Second}
	if dial != nil {
		tr.DialContext = dial
	} else {
		tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	return &http.Client{Transport: tr}
}

// NewDockerSource builds a docker log source. dockerHost accepts
// unix:///path/to/docker.sock (default) or tcp://host:port — anything with a
// reachable Docker Engine API (e.g. a tecnativa/docker-socket-proxy).
func NewDockerSource(dockerHost, container string, tail int) (*DockerSource, error) {
	if container == "" {
		return nil, fmt.Errorf("logs: docker source needs a container name")
	}
	s := &DockerSource{container: container, tail: tail}
	switch {
	case strings.HasPrefix(dockerHost, "tcp://"):
		s.host = "http://" + strings.TrimPrefix(dockerHost, "tcp://")
		s.client = streamClient(nil)
	case strings.HasPrefix(dockerHost, "http://") || strings.HasPrefix(dockerHost, "https://"):
		s.host = dockerHost
		s.client = streamClient(nil)
	default:
		sock := strings.TrimPrefix(dockerHost, "unix://")
		if sock == "" || sock == dockerHost {
			sock = "/var/run/docker.sock"
		}
		s.host = "http://docker"
		s.client = streamClient(func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", sock)
		})
	}
	return s, nil
}

func (s *DockerSource) String() string { return "docker:" + s.container }

// Stream blocks, emitting each log line into out until ctx ends or the
// stream breaks. Reconnection is the caller's job — s.since keeps position
// so reconnects don't replay history.
func (s *DockerSource) Stream(ctx context.Context, out chan<- string) error {
	q := url.Values{
		"follow": {"1"}, "stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"},
	}
	if !s.last.IsZero() {
		q.Set("tail", fmt.Sprint(resumeTail))
	} else {
		q.Set("tail", fmt.Sprint(s.tail))
	}
	u := s.host + "/v1.43/containers/" + url.PathEscape(s.container) + "/logs?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker logs %s: %s", s.container, resp.Status)
	}
	// Container started with a TTY → raw stream; otherwise each frame carries
	// an 8-byte header: [stream,0,0,0, size(32 BE)]. Peek and pick.
	if strings.Contains(resp.Header.Get("Content-Type"), "raw-stream") {
		return s.scan(ctx, resp.Body, out)
	}
	return s.demux(ctx, resp.Body, out)
}

// demux splits the multiplexed frame stream into log lines.
func (s *DockerSource) demux(ctx context.Context, r io.Reader, out chan<- string) error {
	br := bufio.NewReader(r)
	var partial []byte // line continuation across frames
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return err
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		if size == 0 || size > 1<<22 {
			return fmt.Errorf("bad docker frame size %d", size)
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(br, buf); err != nil {
			return err
		}
		buf = append(partial, buf...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			partial = buf[i+1:]
			if text, ok := s.note(string(buf[:i])); ok && !emit(ctx, out, text) {
				return nil
			}
			buf = partial
		}
		partial = buf
	}
}

// scan handles a plain (TTY) stream.
func (s *DockerSource) scan(ctx context.Context, r io.Reader, out chan<- string) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if text, ok := s.note(sc.Text()); ok && !emit(ctx, out, text) {
			return nil
		}
	}
	return sc.Err()
}

// note strips the docker timestamp prefix and returns the text; false for a
// line already read before a reconnect.
func (s *DockerSource) note(line string) (string, bool) {
	if i := strings.IndexByte(line, ' '); i > 0 {
		if t, err := time.Parse(time.RFC3339Nano, line[:i]); err == nil {
			if !t.After(s.last) {
				return "", false
			}
			s.last = t
			return line[i+1:], true
		}
	}
	return line, true
}

// emit blocks on out unless ctx ends.
func emit(ctx context.Context, out chan<- string, line string) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- line:
		return true
	}
}
