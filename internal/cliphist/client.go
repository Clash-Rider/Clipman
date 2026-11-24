package cliphist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const (
	// DefaultMaxPayloadBytes caps decoded clipboard payloads at 16 MiB so a
	// single huge clipboard entry cannot exhaust application memory.
	DefaultMaxPayloadBytes = 16 * 1024 * 1024

	maxListBytes = 16 * 1024 * 1024
)

// ErrNotFound indicates the cliphist executable is not available in PATH.
var ErrNotFound = errors.New("cliphist executable not found in PATH")

// ErrPayloadTooLarge indicates a decoded clipboard entry exceeded the size limit.
var ErrPayloadTooLarge = errors.New("clipboard entry exceeds maximum allowed size")

// Entry represents a single line from `cliphist list`.
type Entry struct {
	ID       string // Leading numeric ID from cliphist TSV output.
	Preview  string // Display preview text (second TSV column).
	RawLine  string // Exact original line passed back to `cliphist decode` / `cliphist delete`.
	IsBinary bool   // True when cliphist reports binary/image metadata in the preview.
	MIMEType string // Best-effort MIME hint parsed from cliphist binary preview, if present.
}

// Client wraps the cliphist CLI without ever invoking a shell or cliphist wipe.
type Client struct {
	Binary          string // Executable name or path; defaults to "cliphist".
	MaxPayloadBytes int    // Maximum decoded payload size; defaults to DefaultMaxPayloadBytes.
}

// NewClient creates a Client using the default "cliphist" binary.
func NewClient() *Client {
	return &Client{
		Binary:          "cliphist",
		MaxPayloadBytes: DefaultMaxPayloadBytes,
	}
}

func (c *Client) bin() string {
	if c != nil && c.Binary != "" {
		return c.Binary
	}
	return "cliphist"
}

func (c *Client) maxPayload() int {
	if c != nil && c.MaxPayloadBytes > 0 {
		return c.MaxPayloadBytes
	}
	return DefaultMaxPayloadBytes
}

// CheckAvailable verifies that the cliphist executable exists.
func (c *Client) CheckAvailable() error {
	if _, err := exec.LookPath(c.bin()); err != nil {
		return ErrNotFound
	}
	return nil
}

// List runs `cliphist list` and parses its tab-separated output with a bounded read.
func (c *Client) List(ctx context.Context) ([]Entry, error) {
	if err := c.CheckAvailable(); err != nil {
		return nil, err
	}

	data, err := runBoundedStdout(ctx, c.bin(), []string{"list"}, nil, maxListBytes)
	if err != nil {
		return nil, err
	}
	return ParseListOutput(string(data)), nil
}

// Decode retrieves the exact raw bytes for an entry using `cliphist decode`.
// Output is streamed through a bounded reader so oversized entries are rejected
// without allocating an unbounded buffer. Clipboard content is never logged.
func (c *Client) Decode(ctx context.Context, entry Entry) ([]byte, error) {
	if err := c.CheckAvailable(); err != nil {
		return nil, err
	}
	line := entry.RawLine
	if line == "" {
		if entry.ID == "" {
			return nil, errors.New("cliphist decode: empty entry ID")
		}
		line = entry.ID + "\t" + entry.Preview
	}

	return runBoundedStdout(ctx, c.bin(), []string{"decode"}, strings.NewReader(line), c.maxPayload())
}

// Delete removes a single history entry via `cliphist delete`.
// It never calls `cliphist wipe`.
func (c *Client) Delete(ctx context.Context, entry Entry) error {
	if err := c.CheckAvailable(); err != nil {
		return err
	}
	line := entry.RawLine
	if line == "" {
		if entry.ID == "" {
			return errors.New("cliphist delete: empty entry ID")
		}
		line = entry.ID + "\t" + entry.Preview
	}

	cmd := exec.CommandContext(ctx, c.bin(), "delete")
	cmd.Stdin = strings.NewReader(line)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Run(); err != nil {
		return formatExecError("cliphist delete", err)
	}
	return nil
}

// runBoundedStdout streams stdout up to maxBytes+1, canceling the subprocess
// immediately if the output exceeds maxBytes so memory stays strictly bounded.
// Subprocess stderr is discarded and never surfaced in UI errors or logs.
func runBoundedStdout(ctx context.Context, binary string, args []string, stdin io.Reader, maxBytes int) ([]byte, error) {
	opName := "cliphist " + strings.Join(args, " ")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, binary, args...)
	cmd.Stdin = stdin
	cmd.Stderr = io.Discard

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s failed to open output pipe", opName)
	}
	if err := cmd.Start(); err != nil {
		return nil, formatExecError(opName, err)
	}

	limited := io.LimitReader(stdoutPipe, int64(maxBytes)+1)
	data, readErr := io.ReadAll(limited)

	if len(data) > maxBytes {
		// Stop the child cleanly and reap it before returning ErrPayloadTooLarge.
		cancel()
		_ = stdoutPipe.Close()
		_ = cmd.Wait()
		return nil, fmt.Errorf("%w (limit %d bytes)", ErrPayloadTooLarge, maxBytes)
	}

	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, fmt.Errorf("%s failed to read output", opName)
	}
	if waitErr != nil {
		return nil, formatExecError(opName, waitErr)
	}
	return data, nil
}

// ParseListOutput parses the TSV lines emitted by `cliphist list`.
// Malformed lines (missing tab or empty ID) are skipped safely.
func ParseListOutput(out string) []Entry {
	if out == "" {
		return nil
	}
	rawLines := strings.Split(out, "\n")
	entries := make([]Entry, 0, len(rawLines))
	for _, line := range rawLines {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		id, preview, ok := strings.Cut(line, "\t")
		id = strings.TrimSpace(id)
		if !ok || id == "" {
			continue
		}
		isBin, mime := detectBinaryPreview(preview)
		entries = append(entries, Entry{
			ID:       id,
			Preview:  preview,
			RawLine:  line,
			IsBinary: isBin,
			MIMEType: mime,
		})
	}
	return entries
}

// detectBinaryPreview recognizes cliphist's `[[ binary data ... ]]` preview format.
func detectBinaryPreview(preview string) (bool, string) {
	trimmed := strings.TrimSpace(preview)
	if !strings.HasPrefix(trimmed, "[[ binary data") || !strings.HasSuffix(trimmed, "]]") {
		return false, ""
	}
	lower := strings.ToLower(trimmed)
	switch {
	case strings.Contains(lower, "png"):
		return true, "image/png"
	case strings.Contains(lower, "jpeg") || strings.Contains(lower, "jpg"):
		return true, "image/jpeg"
	case strings.Contains(lower, "webp"):
		return true, "image/webp"
	case strings.Contains(lower, "gif"):
		return true, "image/gif"
	case strings.Contains(lower, "bmp"):
		return true, "image/bmp"
	case strings.Contains(lower, "svg"):
		return true, "image/svg+xml"
	default:
		return true, "application/octet-stream"
	}
}

// formatExecError returns safe generic error descriptions with exit codes only.
// Raw subprocess stderr is never exposed to avoid leaking clipboard or system data.
func formatExecError(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out", op)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s canceled", op)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("%s failed (exit %d): verify cliphist and Wayland clipboard service are available", op, exitErr.ExitCode())
	}
	return fmt.Errorf("%s failed to execute", op)
}
