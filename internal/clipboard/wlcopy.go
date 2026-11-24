package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// DefaultMaxCopyBytes caps copied payloads at 16 MiB to match cliphist decode limits.
const DefaultMaxCopyBytes = 16 * 1024 * 1024

// ErrNotFound indicates the wl-copy executable is not available in PATH.
var ErrNotFound = errors.New("wl-copy executable not found in PATH")

// ErrPayloadTooLarge indicates data passed to wl-copy exceeds the allowed size limit.
var ErrPayloadTooLarge = errors.New("clipboard payload exceeds maximum copy size")

// Copier copies raw bytes to the Wayland clipboard via wl-copy.
type Copier struct {
	Binary       string // Executable name or path; defaults to "wl-copy".
	MaxCopyBytes int    // Maximum bytes accepted; defaults to DefaultMaxCopyBytes.
}

// NewCopier creates a Copier using the default "wl-copy" binary.
func NewCopier() *Copier {
	return &Copier{
		Binary:       "wl-copy",
		MaxCopyBytes: DefaultMaxCopyBytes,
	}
}

func (c *Copier) bin() string {
	if c != nil && c.Binary != "" {
		return c.Binary
	}
	return "wl-copy"
}

func (c *Copier) maxBytes() int {
	if c != nil && c.MaxCopyBytes > 0 {
		return c.MaxCopyBytes
	}
	return DefaultMaxCopyBytes
}

// CheckAvailable verifies that wl-copy exists in PATH.
func (c *Copier) CheckAvailable() error {
	if _, err := exec.LookPath(c.bin()); err != nil {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return nil
}

// Copy writes raw bytes to wl-copy via stdin without modifying binary data.
// If mimeType is empty, it is inferred conservatively from the byte header.
// stdout and stderr are redirected to /dev/null rather than closed because
// wl-clipboard aborts when launched with closed standard streams, and leaving
// pipes open would block waiting on wl-copy's forked background helper.
func (c *Copier) Copy(ctx context.Context, data []byte, mimeType string) (retErr error) {
	if err := c.CheckAvailable(); err != nil {
		return err
	}
	if len(data) > c.maxBytes() {
		return fmt.Errorf("%w (limit %d bytes)", ErrPayloadTooLarge, c.maxBytes())
	}

	resolvedMIME := ResolveMIME(data, mimeType)
	var args []string
	if resolvedMIME != "" {
		args = append(args, "--type", resolvedMIME)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("wl-copy open %s: %w", os.DevNull, err)
	}
	defer func() {
		if closeErr := devNull.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("wl-copy close %s: %w", os.DevNull, closeErr)
		}
	}()

	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = devNull
	cmd.Stderr = devNull

	if err := cmd.Run(); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("wl-copy canceled: %w", err)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("wl-copy failed (exit %d): verify Wayland clipboard service is available", exitErr.ExitCode())
		}
		return fmt.Errorf("wl-copy failed: %w", err)
	}
	return nil
}

// ResolveMIME picks a safe explicit MIME type for wl-copy.
// Plain valid UTF-8 text uses "text/plain;charset=utf-8"; images and known
// binary formats preserve their detected MIME type without converting to text.
func ResolveMIME(data []byte, hint string) string {
	hint = strings.TrimSpace(hint)
	if hint != "" && hint != "application/octet-stream" {
		return hint
	}
	if len(data) == 0 {
		return "text/plain;charset=utf-8"
	}

	// Check common image magic headers directly before generic sniffing.
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 6 && (bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case len(data) >= 2 && data[0] == 'B' && data[1] == 'M':
		return "image/bmp"
	}

	// If data is valid UTF-8 with no NUL bytes, treat as plain UTF-8 text so
	// HTML/JSON/XML snippets are copied as text rather than re-typed.
	if !bytes.ContainsRune(data, 0) && utf8.Valid(data) {
		return "text/plain;charset=utf-8"
	}

	detected := http.DetectContentType(data)
	if detected != "" {
		return detected
	}
	return "application/octet-stream"
}
