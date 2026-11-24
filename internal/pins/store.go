package pins

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	indexFileName       = "pins.json"
	lockFileName        = ".clipman.lock"
	currentIndexVersion = 1
	dirPerm             = 0o700
	filePerm            = 0o600
	MaxPayloadBytes     = 16 * 1024 * 1024 // 16 MiB cap per pinned payload
	maxIndexBytes       = 8 * 1024 * 1024
	stagedPrefix        = ".removing-"
	stagedSuffix        = ".tmp"
)

var (
	// ErrStoreClosed is returned when Pin or Unpin is called after Close().
	ErrStoreClosed = errors.New("pin storage is closed")
	// ErrAlreadyLocked indicates another clipman process already holds the pin storage lock.
	ErrAlreadyLocked = errors.New("another clipman instance is already using pin storage")
	// ErrCorruptIndex is returned when pins.json was invalid and has been quarantined.
	ErrCorruptIndex = errors.New("pin storage index was corrupted and quarantined")
	// ErrCorruptUnquarantined is returned when pins.json is invalid and could not be quarantined;
	// writes are blocked so the corrupted file is never silently overwritten.
	ErrCorruptUnquarantined = errors.New("pin storage index is corrupted and could not be quarantined")
	// ErrUnsupportedIndexVersion is returned when pins.json uses an unsupported schema version.
	ErrUnsupportedIndexVersion = errors.New("pin storage index has an unsupported version")
	// ErrSymlinkRejected is returned when an unexpected symlink is encountered in pin storage.
	ErrSymlinkRejected = errors.New("refusing to follow symlink in pin storage")
	// ErrPayloadTooLarge is returned when a payload exceeds MaxPayloadBytes.
	ErrPayloadTooLarge = errors.New("pinned payload exceeds maximum allowed size")
	// ErrPayloadMissing is returned when a pinned item's payload file is missing.
	ErrPayloadMissing = errors.New("pinned payload file is missing")
	// ErrPayloadIntegrity is returned when a payload's SHA-256 does not match its index entry.
	ErrPayloadIntegrity = errors.New("pinned payload failed SHA-256 integrity check")
	// ErrPayloadCleanupFailed indicates the pin was removed from pins.json, but
	// deleting the staged payload file from disk failed.
	ErrPayloadCleanupFailed = errors.New("pin removed from index, but failed to delete staged payload file")
	// ErrStagedFileExists indicates a staged removal file already exists for a hash
	// and Unpin refuses to overwrite it.
	ErrStagedFileExists = errors.New("staged removal file already exists; refusing to overwrite")
	// ErrRecoveryFailed indicates startup recovery encountered an ambiguous or
	// unrecoverable staged file state and preserved files in read-only mode.
	ErrRecoveryFailed = errors.New("pin storage recovery preserved files due to an unrecoverable state")
)

// Pin represents a persistently pinned clipboard item independent of cliphist.
type Pin struct {
	SHA256    string    `json:"sha256"`
	OriginID  string    `json:"origin_id,omitempty"`  // Primary/legacy cliphist ID for backward compatibility.
	OriginIDs []string  `json:"origin_ids,omitempty"` // All cliphist IDs associated with this payload.
	MIMEType  string    `json:"mime_type"`
	Preview   string    `json:"preview"`
	IsBinary  bool      `json:"is_binary"`
	SizeBytes int       `json:"size_bytes"`
	PinnedAt  time.Time `json:"pinned_at"`
}

// AllOriginIDs returns every unique non-empty cliphist ID associated with this pin.
func (p Pin) AllOriginIDs() []string {
	return normalizeOriginIDs(p.OriginID, p.OriginIDs)
}

type indexFile struct {
	Version int   `json:"version"`
	Pins    []Pin `json:"pins"`
}

// Store manages pinned items inside an application-owned user directory,
// protected across processes by an advisory exclusive flock.
type Store struct {
	mu          sync.Mutex
	dir         string
	lockFile    *os.File
	closed      bool
	indexLoaded bool // True only when a valid current-version pins.json was loaded.
	pins        []Pin
	writeLocked error // Non-nil when storage is read-only until corruption/recovery is resolved.

	// Optional unexported filesystem hooks for deterministic unit tests (nil in production).
	renameFn     func(oldpath, newpath string) error
	writeIndexFn func(targetPath string, data []byte) error
}

// DefaultDir returns $XDG_DATA_HOME/clipman/pins or ~/.local/share/clipman/pins.
func DefaultDir() (string, error) {
	base := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "clipman", "pins"), nil
}

// Open acquires an exclusive non-blocking inter-process lock on the pin storage
// directory, loads pins.json, and reconciles any `.removing-<sha256>.tmp` staged
// files against the loaded index. Callers should call Close() on shutdown.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("pin storage directory is empty")
	}
	cleanDir := filepath.Clean(dir)
	if err := ensureSafeDir(cleanDir); err != nil {
		return nil, err
	}

	lockFile, err := acquireStoreLock(cleanDir)
	if err != nil {
		return nil, err
	}

	s := &Store{
		dir:      cleanDir,
		lockFile: lockFile,
	}
	if err := s.load(); err != nil {
		if errors.Is(err, ErrCorruptIndex) {
			// Check for unexplained staged files before allowing writes to create
			// a fresh pins.json that would treat those staged files as orphans later.
			if recErr := s.recoverStagedFiles(); recErr != nil {
				s.writeLocked = recErr
				return s, errors.Join(err, recErr)
			}
			return s, err
		}
		if errors.Is(err, ErrCorruptUnquarantined) ||
			errors.Is(err, ErrUnsupportedIndexVersion) {
			// Writes are already locked and index is untrusted; leave staged files untouched.
			return s, err
		}
		_ = s.Close()
		return nil, err
	}

	if err := s.recoverStagedFiles(); err != nil {
		// Preserve readable pins, but lock writes until recovery issue is resolved.
		s.writeLocked = err
		return s, err
	}
	return s, nil
}

// Close marks the store closed, releases the inter-process lock, and closes its
// file descriptor. Close is idempotent; subsequent Pin/Unpin calls fail with ErrStoreClosed.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.lockFile == nil {
		return nil
	}
	f := s.lockFile
	s.lockFile = nil
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	return errors.Join(unlockErr, closeErr)
}

// List returns a copy of all pinned items (newest pinned first).
func (s *Store) List() []Pin {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Pin, len(s.pins))
	for i, p := range s.pins {
		p.OriginIDs = append([]string(nil), p.OriginIDs...)
		out[i] = p
	}
	return out
}

// PinnedOriginIDs returns a lookup set of all cliphist IDs that are currently pinned.
func (s *Store) PinnedOriginIDs() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make(map[string]struct{}, len(s.pins))
	for _, p := range s.pins {
		for _, id := range p.AllOriginIDs() {
			ids[id] = struct{}{}
		}
	}
	return ids
}

// Pin saves raw clipboard bytes and metadata persistently.
// Duplicate payloads (same SHA-256) keep a single pin entry while recording
// every distinct cliphist origin ID associated with that payload.
func (s *Store) Pin(data []byte, originID, mimeType, preview string, isBinary bool) (Pin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return Pin{}, ErrStoreClosed
	}
	if s.writeLocked != nil {
		return Pin{}, fmt.Errorf("pin storage is read-only until resolved: %w", s.writeLocked)
	}
	if len(data) > MaxPayloadBytes {
		return Pin{}, fmt.Errorf("%w (limit %d bytes)", ErrPayloadTooLarge, MaxPayloadBytes)
	}
	if err := verifyDirNoSymlink(s.dir); err != nil {
		return Pin{}, err
	}

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	for i, existing := range s.pins {
		if existing.SHA256 == hash {
			if originID != "" && !containsString(existing.OriginIDs, originID) {
				prevID := s.pins[i].OriginID
				prevIDs := append([]string(nil), s.pins[i].OriginIDs...)

				updatedIDs := normalizeOriginIDs(existing.OriginID, append(append([]string(nil), existing.OriginIDs...), originID))
				s.pins[i].OriginIDs = updatedIDs
				if s.pins[i].OriginID == "" && len(updatedIDs) > 0 {
					s.pins[i].OriginID = updatedIDs[0]
				}
				if err := s.saveIndexLocked(); err != nil {
					s.pins[i].OriginID = prevID
					s.pins[i].OriginIDs = prevIDs
					return Pin{}, err
				}
			}
			res := s.pins[i]
			res.OriginIDs = append([]string(nil), res.OriginIDs...)
			return res, nil
		}
	}

	payloadPath, err := s.payloadPath(hash)
	if err != nil {
		return Pin{}, err
	}
	if err := writeFileAtomicNoFollow(payloadPath, data); err != nil {
		return Pin{}, fmt.Errorf("write pin payload: %w", err)
	}

	originIDs := normalizeOriginIDs(originID, nil)
	pin := Pin{
		SHA256:    hash,
		OriginID:  originID,
		OriginIDs: originIDs,
		MIMEType:  mimeType,
		Preview:   sanitizePreview(preview, data, isBinary, mimeType),
		IsBinary:  isBinary,
		SizeBytes: len(data),
		PinnedAt:  time.Now().UTC(),
	}

	s.pins = append([]Pin{pin}, s.pins...)
	if err := s.saveIndexLocked(); err != nil {
		s.pins = s.pins[1:]
		if rmErr := os.Remove(payloadPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return Pin{}, errors.Join(
				fmt.Errorf("save pin index: %w", err),
				fmt.Errorf("rollback payload removal failed: %w", rmErr),
			)
		}
		return Pin{}, fmt.Errorf("save pin index: %w", err)
	}
	s.indexLoaded = true
	res := pin
	res.OriginIDs = append([]string(nil), res.OriginIDs...)
	return res, nil
}

// Unpin removes a pin by its SHA-256 hash and deletes its stored payload file.
//  1. Verify store is open, writable, and no staged file already exists.
//  2. Stage-rename `<sha256>.bin` -> `.removing-<sha256>.tmp`.
//  3. Atomically write the updated index; if writing fails, restore the staged file.
//     If restoring the staged file ALSO fails, lock the store read-only (`s.writeLocked`)
//     and preserve the staged file without attempting further destructive cleanup.
//  4. Remove the staged file; if physical deletion fails after the index was
//     updated, return ErrPayloadCleanupFailed.
func (s *Store) Unpin(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}
	if s.writeLocked != nil {
		return fmt.Errorf("pin storage is read-only until resolved: %w", s.writeLocked)
	}
	if !isValidSHA256(hash) {
		return errors.New("invalid pin hash")
	}
	if err := verifyDirNoSymlink(s.dir); err != nil {
		return err
	}

	idx := -1
	for i, p := range s.pins {
		if p.SHA256 == hash {
			idx = i
			break
		}
	}
	if idx == -1 {
		return nil
	}

	payloadPath, err := s.payloadPath(hash)
	if err != nil {
		return err
	}

	var stagedPath string
	info, lerr := os.Lstat(payloadPath)
	switch {
	case lerr == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlinkRejected, filepath.Base(payloadPath))
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("pinned payload is not a regular file: %s", filepath.Base(payloadPath))
		}
		stagedPath = filepath.Join(s.dir, stagedPrefix+hash+stagedSuffix)
		if stInfo, stErr := os.Lstat(stagedPath); stErr == nil {
			if stInfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: %s", ErrSymlinkRejected, filepath.Base(stagedPath))
			}
			return fmt.Errorf("%w: %s", ErrStagedFileExists, filepath.Base(stagedPath))
		} else if !errors.Is(stErr, os.ErrNotExist) {
			return fmt.Errorf("inspect staged removal path: %w", stErr)
		}
		if err := s.rename(payloadPath, stagedPath); err != nil {
			return fmt.Errorf("stage pinned payload for removal: %w", err)
		}
	case errors.Is(lerr, os.ErrNotExist):
		// Payload file was already absent; still clean up the stale index entry.
	default:
		return fmt.Errorf("inspect pinned payload before removal: %w", lerr)
	}

	removed := s.pins[idx]
	next := make([]Pin, 0, len(s.pins)-1)
	next = append(next, s.pins[:idx]...)
	next = append(next, s.pins[idx+1:]...)
	s.pins = next

	if err := s.saveIndexLocked(); err != nil {
		restored := make([]Pin, 0, len(s.pins)+1)
		restored = append(restored, s.pins[:idx]...)
		restored = append(restored, removed)
		restored = append(restored, s.pins[idx:]...)
		s.pins = restored

		if stagedPath != "" {
			if rbErr := s.rename(stagedPath, payloadPath); rbErr != nil {
				combined := errors.Join(
					fmt.Errorf("update pin index: %w", err),
					fmt.Errorf("restore staged payload failed: %w", rbErr),
				)
				// Lock writes to prevent further mutations while index and payload may disagree.
				s.writeLocked = combined
				return combined
			}
		}
		return fmt.Errorf("update pin index: %w", err)
	}

	if stagedPath != "" {
		if rmErr := os.Remove(stagedPath); rmErr != nil {
			return fmt.Errorf("%w: %v", ErrPayloadCleanupFailed, rmErr)
		}
	}
	return nil
}

// ReadPayload reads the exact raw clipboard bytes for a pinned item and verifies
// both the maximum size cap and SHA-256 integrity against the expected hash.
func (s *Store) ReadPayload(hash string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := verifyDirNoSymlink(s.dir); err != nil {
		return nil, err
	}
	payloadPath, err := s.payloadPath(hash)
	if err != nil {
		return nil, err
	}
	data, err := readFileNoFollow(payloadPath, MaxPayloadBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w (%s)", ErrPayloadMissing, hash[:12])
		}
		return nil, fmt.Errorf("read pinned payload: %w", err)
	}

	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != hash {
		return nil, fmt.Errorf("%w (%s)", ErrPayloadIntegrity, hash[:12])
	}
	return data, nil
}

// recoverStagedFiles reconciles `.removing-<sha256>.tmp` files against pins.json:
//   - If no trustworthy index was loaded (e.g. missing or quarantined corrupt pins.json),
//     any staged files are preserved untouched and ErrRecoveryFailed is returned.
//   - Referenced hash, original payload absent: verify staged SHA-256 and restore to `<sha256>.bin`.
//   - Referenced hash, original payload present: verify SHA-256 of BOTH files.
//     Only if BOTH match the expected hash is the redundant staged file removed;
//     if either fails verification, both files are preserved and ErrRecoveryFailed is returned.
//   - Unreferenced hash (only when a valid index was loaded): remove the orphaned staged file.
//   - Symlinks, non-regular files, or unrelated filenames are never followed or deleted.
func (s *Store) recoverStagedFiles() error {
	if s.writeLocked != nil {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("%w: read pin directory: %v", ErrRecoveryFailed, err)
	}

	referenced := make(map[string]struct{}, len(s.pins))
	for _, p := range s.pins {
		referenced[p.SHA256] = struct{}{}
	}

	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		hash, ok := parseStagedRemovalHash(name)
		if !ok {
			continue
		}

		// Without a trustworthy loaded pins.json, never delete unexplained staged files.
		if !s.indexLoaded {
			errs = append(errs, fmt.Errorf("staged file %s found without a valid pin index; preserved", name))
			continue
		}

		stagedPath := filepath.Join(s.dir, name)
		stInfo, stErr := os.Lstat(stagedPath)
		if stErr != nil {
			if !errors.Is(stErr, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("inspect %s: %w", name, stErr))
			}
			continue
		}
		if stInfo.Mode()&os.ModeSymlink != 0 {
			errs = append(errs, fmt.Errorf("%w: %s", ErrSymlinkRejected, name))
			continue
		}
		if !stInfo.Mode().IsRegular() {
			errs = append(errs, fmt.Errorf("staged entry is not a regular file: %s", name))
			continue
		}

		if _, isRef := referenced[hash]; !isRef {
			if rmErr := os.Remove(stagedPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove orphaned %s: %w", name, rmErr))
			}
			continue
		}

		payloadPath, pErr := s.payloadPath(hash)
		if pErr != nil {
			errs = append(errs, pErr)
			continue
		}

		pInfo, pStatErr := os.Lstat(payloadPath)
		switch {
		case errors.Is(pStatErr, os.ErrNotExist):
			if err := verifyFileSHA256(stagedPath, hash); err != nil {
				errs = append(errs, fmt.Errorf("staged file %s failed verification; preserved: %w", name, err))
				continue
			}
			if rnErr := s.rename(stagedPath, payloadPath); rnErr != nil {
				errs = append(errs, fmt.Errorf("restore %s to %s: %w", name, filepath.Base(payloadPath), rnErr))
			}

		case pStatErr == nil:
			if pInfo.Mode()&os.ModeSymlink != 0 {
				errs = append(errs, fmt.Errorf("%w: %s", ErrSymlinkRejected, filepath.Base(payloadPath)))
				continue
			}
			if !pInfo.Mode().IsRegular() {
				errs = append(errs, fmt.Errorf("existing payload is not a regular file: %s", filepath.Base(payloadPath)))
				continue
			}
			origErr := verifyFileSHA256(payloadPath, hash)
			stagedVerifyErr := verifyFileSHA256(stagedPath, hash)
			if origErr == nil && stagedVerifyErr == nil {
				if rmErr := os.Remove(stagedPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
					errs = append(errs, fmt.Errorf("remove redundant staged %s: %w", name, rmErr))
				}
				continue
			}
			errs = append(errs, fmt.Errorf(
				"both %s and %s exist with verification mismatch (original=%v, staged=%v); preserved both",
				filepath.Base(payloadPath), name, origErr, stagedVerifyErr,
			))

		default:
			errs = append(errs, fmt.Errorf("inspect payload %s: %w", filepath.Base(payloadPath), pStatErr))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%w: %v", ErrRecoveryFailed, errors.Join(errs...))
	}
	return nil
}

func (s *Store) rename(oldpath, newpath string) error {
	if s.renameFn != nil {
		return s.renameFn(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

func acquireStoreLock(dir string) (*os.File, error) {
	lockPath := filepath.Join(dir, lockFileName)
	if info, err := os.Lstat(lockPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %s", ErrSymlinkRejected, lockFileName)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("lock path is not a regular file: %s", lockFileName)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect pin lock file: %w", err)
	}

	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, filePerm)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%w: %s", ErrSymlinkRejected, lockFileName)
		}
		return nil, fmt.Errorf("open pin lock file: %w", err)
	}
	f := os.NewFile(uintptr(fd), lockPath)
	if err := f.Chmod(filePerm); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("chmod pin lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAlreadyLocked
		}
		return nil, fmt.Errorf("acquire pin storage lock: %w", err)
	}
	return f, nil
}

func verifyFileSHA256(path, expectedHash string) error {
	data, err := readFileNoFollow(path, MaxPayloadBytes)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != expectedHash {
		return ErrPayloadIntegrity
	}
	return nil
}

func parseStagedRemovalHash(name string) (string, bool) {
	if !strings.HasPrefix(name, stagedPrefix) || !strings.HasSuffix(name, stagedSuffix) {
		return "", false
	}
	hash := strings.TrimSuffix(strings.TrimPrefix(name, stagedPrefix), stagedSuffix)
	if !isValidSHA256(hash) {
		return "", false
	}
	return hash, true
}

func (s *Store) load() error {
	s.indexLoaded = false
	idxPath := filepath.Join(s.dir, indexFileName)
	data, err := readFileNoFollow(idxPath, maxIndexBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.pins = nil
			return nil
		}
		return fmt.Errorf("read pin index: %w", err)
	}

	var idx indexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		return s.quarantineCorruptIndex(idxPath, fmt.Errorf("invalid JSON: %w", err))
	}

	if idx.Version > currentIndexVersion {
		verErr := fmt.Errorf("%w: found version %d, supported version %d", ErrUnsupportedIndexVersion, idx.Version, currentIndexVersion)
		s.writeLocked = verErr
		s.pins = nil
		return verErr
	}
	if idx.Version != currentIndexVersion {
		return s.quarantineCorruptIndex(idxPath, fmt.Errorf("invalid index version %d", idx.Version))
	}

	valid := make([]Pin, 0, len(idx.Pins))
	seen := make(map[string]struct{}, len(idx.Pins))
	for _, p := range idx.Pins {
		if !isValidSHA256(p.SHA256) {
			continue
		}
		if _, dup := seen[p.SHA256]; dup {
			continue
		}
		seen[p.SHA256] = struct{}{}
		p.OriginIDs = normalizeOriginIDs(p.OriginID, p.OriginIDs)
		if p.OriginID == "" && len(p.OriginIDs) > 0 {
			p.OriginID = p.OriginIDs[0]
		}
		valid = append(valid, p)
	}
	s.pins = valid
	s.indexLoaded = true
	return nil
}

func (s *Store) quarantineCorruptIndex(idxPath string, cause error) error {
	quarantineName := fmt.Sprintf("%s.corrupt.%d", indexFileName, time.Now().UnixNano())
	quarantinePath := filepath.Join(s.dir, quarantineName)

	if renameErr := s.rename(idxPath, quarantinePath); renameErr != nil {
		lockedErr := fmt.Errorf("%w: %v; quarantine rename failed: %w", ErrCorruptUnquarantined, cause, renameErr)
		s.writeLocked = lockedErr
		s.pins = nil
		return lockedErr
	}

	s.pins = nil
	return fmt.Errorf("%w (%s): %v", ErrCorruptIndex, quarantineName, cause)
}

func (s *Store) saveIndexLocked() error {
	payload, err := json.MarshalIndent(indexFile{
		Version: currentIndexVersion,
		Pins:    s.pins,
	}, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	idxPath := filepath.Join(s.dir, indexFileName)
	if s.writeIndexFn != nil {
		return s.writeIndexFn(idxPath, payload)
	}
	return writeFileAtomicNoFollow(idxPath, payload)
}

func (s *Store) payloadPath(hash string) (string, error) {
	if !isValidSHA256(hash) {
		return "", errors.New("invalid pin hash")
	}
	p := filepath.Join(s.dir, hash+".bin")
	if filepath.Dir(p) != s.dir {
		return "", errors.New("path traversal detected")
	}
	return p, nil
}

func normalizeOriginIDs(primary string, extra []string) []string {
	out := make([]string, 0, len(extra)+1)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || containsString(out, id) {
			return
		}
		out = append(out, id)
	}
	add(primary)
	for _, id := range extra {
		add(id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func containsString(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

func isValidSHA256(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func ensureSafeDir(dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, dirPerm); err != nil {
		return fmt.Errorf("create parent data directory: %w", err)
	}
	if err := os.Mkdir(dir, dirPerm); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create pin storage directory: %w", err)
	}
	if err := verifyDirNoSymlink(dir); err != nil {
		return err
	}
	return os.Chmod(dir, dirPerm)
}

func verifyDirNoSymlink(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect pin directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkRejected, dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("pin storage path is not a directory: %s", dir)
	}
	return nil
}

func readFileNoFollow(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", ErrSymlinkRejected, filepath.Base(path))
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", filepath.Base(path))
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%w (limit %d bytes)", ErrPayloadTooLarge, maxBytes)
	}

	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%w: %s", ErrSymlinkRejected, filepath.Base(path))
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() {
		_ = f.Close()
	}()

	limited := io.LimitReader(f, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w (limit %d bytes)", ErrPayloadTooLarge, maxBytes)
	}
	return data, nil
}

func writeFileAtomicNoFollow(targetPath string, data []byte) error {
	if info, err := os.Lstat(targetPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlinkRejected, filepath.Base(targetPath))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	dir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(dir, ".clipman-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func sanitizePreview(preview string, data []byte, isBinary bool, mimeType string) string {
	if strings.TrimSpace(preview) != "" {
		return preview
	}
	if isBinary {
		if mimeType == "" {
			mimeType = "binary"
		}
		return fmt.Sprintf("[[ %s (%d B) ]]", mimeType, len(data))
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(data), "\n", " "))
	if len(text) > 120 {
		return text[:120] + "..."
	}
	return text
}
