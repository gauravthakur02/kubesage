package kube

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// FileEntry is one entry of a directory inside a container.
type FileEntry struct {
	Name, Mode, Target string
	Size               int64
	Dir, Link          bool
}

func (c *Client) execCapture(ctx context.Context, ns, pod, container string, cmd []string, stdin io.Reader, limit int64) ([]byte, string, error) {
	var out bytes.Buffer
	var errb bytes.Buffer
	w := io.Writer(&out)
	if limit > 0 {
		w = &limitWriter{w: &out, n: limit}
	}
	err := c.Exec(ctx, ExecOptions{Namespace: ns, Pod: pod, Container: container, Command: cmd, Stdin: stdin, Stdout: w, Stderr: &errb})
	return out.Bytes(), strings.TrimSpace(errb.String()), err
}

type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil // drop the rest, keep the stream alive
	}
	k := int64(len(p))
	if k > l.n {
		k = l.n
	}
	_, err := l.w.Write(p[:k])
	l.n -= k
	return len(p), err
}

// ParseLs parses `ls -la` output (GNU and busybox).
func ParseLs(out string) []FileEntry {
	var es []FileEntry
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 9 || len(f[0]) < 10 || strings.HasPrefix(l, "total") {
			continue
		}
		name := strings.Join(f[8:], " ")
		if name == "." || name == ".." {
			continue
		}
		e := FileEntry{Mode: f[0], Dir: f[0][0] == 'd', Link: f[0][0] == 'l'}
		e.Size, _ = strconv.ParseInt(f[4], 10, 64)
		if e.Link {
			if n, t, ok := strings.Cut(name, " -> "); ok {
				name, e.Target = n, t
			}
		}
		e.Name = name
		es = append(es, e)
	}
	return es
}

// ListDir lists a directory in a container (needs ls in the image and pods/exec).
func (c *Client) ListDir(ctx context.Context, ns, pod, container, path string) ([]FileEntry, error) {
	out, stderr, err := c.execCapture(ctx, ns, pod, container, []string{"ls", "-la", path}, nil, 4<<20)
	if err != nil {
		if stderr != "" {
			return nil, fmt.Errorf("%s", firstLine(stderr))
		}
		return nil, err
	}
	return ParseLs(string(out)), nil
}

// ReadFile returns up to limit bytes of a file inside a container.
func (c *Client) ReadFile(ctx context.Context, ns, pod, container, path string, limit int64) ([]byte, error) {
	out, stderr, err := c.execCapture(ctx, ns, pod, container, []string{"cat", path}, nil, limit)
	if err != nil {
		if stderr != "" {
			return nil, fmt.Errorf("%s", firstLine(stderr))
		}
		return nil, err
	}
	return out, nil
}

// DownloadFile copies a file from a container to a local path (up to 1 GiB).
func (c *Client) DownloadFile(ctx context.Context, ns, pod, container, remote, local string) (int64, error) {
	f, err := os.Create(local)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var errb bytes.Buffer
	lw := &limitWriter{w: f, n: 1 << 30}
	err = c.Exec(ctx, ExecOptions{Namespace: ns, Pod: pod, Container: container, Command: []string{"cat", remote}, Stdout: lw, Stderr: &errb})
	if err != nil {
		os.Remove(local)
		if errb.Len() > 0 {
			return 0, fmt.Errorf("%s", firstLine(errb.String()))
		}
		return 0, err
	}
	st, _ := f.Stat()
	return st.Size(), nil
}

// UploadFile copies a local file into a container (needs sh and cat in the image).
func (c *Client) UploadFile(ctx context.Context, ns, pod, container, local, remote string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	_, stderr, err := c.execCapture(ctx, ns, pod, container, []string{"sh", "-c", `cat > "$1"`, "sh", remote}, f, 1<<16)
	if err != nil && stderr != "" {
		return fmt.Errorf("%s", firstLine(stderr))
	}
	return err
}
