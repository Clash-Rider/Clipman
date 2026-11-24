package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"clipman/internal/cliphist"
	"clipman/internal/clipboard"
	"clipman/internal/pins"
)

// Tab represents the two mutually exclusive views in the UI.
type Tab int

const (
	TabAll Tab = iota
	TabPinned
)

// Item is a unified view-model row for either a cliphist entry or a pinned item.
type Item struct {
	Key      string // Unique row key ("h:<id>" or "p:<sha256>")
	OriginID string // cliphist entry ID, when available
	PinHash  string // SHA-256 hash when item is in Pinned
	Preview  string
	MIMEType string
	IsBinary bool
	IsPinned bool
	RawEntry cliphist.Entry
}

// HistoryBackend abstracts cliphist operations for clean testing.
type HistoryBackend interface {
	CheckAvailable() error
	List(ctx context.Context) ([]cliphist.Entry, error)
	Decode(ctx context.Context, entry cliphist.Entry) ([]byte, error)
	Delete(ctx context.Context, entry cliphist.Entry) error
}

// ClipboardBackend abstracts wl-copy operations for clean testing.
type ClipboardBackend interface {
	CheckAvailable() error
	Copy(ctx context.Context, data []byte, mimeType string) error
}

// RefreshResult carries the outcome of an off-thread history reload.
type RefreshResult struct {
	Entries []cliphist.Entry
	Pins    []pins.Pin
	Err     error
}

// State holds all application state independently of Gio rendering so
// selection, tab switching, pinning separation, and editing are fully unit-tested.
// All State mutations happen on the UI goroutine.
type State struct {
	history HistoryBackend
	copier  ClipboardBackend
	pins    *pins.Store

	ActiveTab      Tab
	Query          string
	historyEntries []cliphist.Entry
	pinnedEntries  []pins.Pin

	// Independent selection per tab so switching tabs preserves selection.
	selectedAll    int
	selectedPinned int

	// Transient modes.
	ConfirmDeleteItem *Item
	EditingItem       *Item
	EditDraft         string

	// Persistent storage warning (e.g. corrupt/unsupported index) that should not be
	// silently cleared by a successful cliphist refresh.
	storageWarn string

	// User-facing feedback banners (never contain raw clipboard content).
	ErrorMsg  string
	StatusMsg string
}

// NewState initializes the controller and preserves any initial storage error.
func NewState(history HistoryBackend, copier ClipboardBackend, store *pins.Store, initErr error) *State {
	s := &State{
		history:   history,
		copier:    copier,
		pins:      store,
		ActiveTab: TabAll,
	}
	if store != nil {
		s.pinnedEntries = store.List()
	}
	if initErr != nil {
		s.storageWarn = initErr.Error()
		s.ErrorMsg = initErr.Error()
	}
	return s
}

// FetchRefresh runs dependency checks and `cliphist list` without mutating State,
// so it can execute safely on a background goroutine.
func (s *State) FetchRefresh(ctx context.Context) RefreshResult {
	var res RefreshResult
	if s.pins != nil {
		res.Pins = s.pins.List()
	}

	var missing []string
	if s.history != nil {
		if err := s.history.CheckAvailable(); err != nil {
			missing = append(missing, "cliphist not found in PATH")
		}
	}
	if s.copier != nil {
		if err := s.copier.CheckAvailable(); err != nil {
			missing = append(missing, "wl-copy not found in PATH")
		}
	}
	if len(missing) > 0 {
		res.Err = errors.New("Missing dependency: " + strings.Join(missing, "; "))
		return res
	}

	entries, err := s.history.List(ctx)
	if err != nil {
		res.Err = fmt.Errorf("Failed to load clipboard history: %v", err)
		return res
	}
	res.Entries = entries
	return res
}

// ApplyRefresh applies a RefreshResult on the UI goroutine.
func (s *State) ApplyRefresh(res RefreshResult) error {
	if s.pins != nil {
		s.pinnedEntries = res.Pins
	}
	if res.Err != nil {
		if strings.HasPrefix(res.Err.Error(), "Missing dependency:") {
			s.historyEntries = nil
		}
		s.ErrorMsg = res.Err.Error()
		s.clampSelections()
		return res.Err
	}
	s.historyEntries = res.Entries
	s.ErrorMsg = s.storageWarn
	s.clampSelections()
	return nil
}

// Refresh reloads cliphist history and pinned items synchronously (used in tests).
func (s *State) Refresh(ctx context.Context) error {
	return s.ApplyRefresh(s.FetchRefresh(ctx))
}

// VisibleItems returns the filtered items for the active tab.
// All strictly excludes any cliphist entry currently pinned.
func (s *State) VisibleItems() []Item {
	return s.visibleItemsFor(s.ActiveTab)
}

func (s *State) visibleItemsFor(tab Tab) []Item {
	q := strings.ToLower(strings.TrimSpace(s.Query))
	if tab == TabPinned {
		out := make([]Item, 0, len(s.pinnedEntries))
		for _, p := range s.pinnedEntries {
			if q != "" && !strings.Contains(strings.ToLower(p.Preview), q) {
				continue
			}
			out = append(out, Item{
				Key:      "p:" + p.SHA256,
				OriginID: p.OriginID,
				PinHash:  p.SHA256,
				Preview:  p.Preview,
				MIMEType: p.MIMEType,
				IsBinary: p.IsBinary,
				IsPinned: true,
			})
		}
		return out
	}

	pinnedIDs := make(map[string]struct{}, len(s.pinnedEntries))
	for _, p := range s.pinnedEntries {
		for _, id := range p.AllOriginIDs() {
			pinnedIDs[id] = struct{}{}
		}
	}

	out := make([]Item, 0, len(s.historyEntries))
	for _, e := range s.historyEntries {
		// Pinned items must NEVER appear in All.
		if _, isPinned := pinnedIDs[e.ID]; isPinned {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Preview), q) {
			continue
		}
		out = append(out, Item{
			Key:      "h:" + e.ID,
			OriginID: e.ID,
			Preview:  e.Preview,
			MIMEType: e.MIMEType,
			IsBinary: e.IsBinary,
			IsPinned: false,
			RawEntry: e,
		})
	}
	return out
}

// SetQuery updates the search filter and clamps selection cleanly.
func (s *State) SetQuery(q string) {
	if s.Query == q {
		return
	}
	s.Query = q
	s.clampSelections()
}

// SelectedIndex returns the active tab's selected index (-1 when empty).
func (s *State) SelectedIndex() int {
	items := s.VisibleItems()
	if len(items) == 0 {
		return -1
	}
	idx := s.rawSelectedIndex(s.ActiveTab)
	if idx < 0 {
		return 0
	}
	if idx >= len(items) {
		return len(items) - 1
	}
	return idx
}

// SelectedItem returns a pointer to the currently selected item, or nil.
func (s *State) SelectedItem() *Item {
	items := s.VisibleItems()
	idx := s.SelectedIndex()
	if idx < 0 || idx >= len(items) {
		return nil
	}
	it := items[idx]
	return &it
}

// SelectIndex sets the current tab's selection (used for click and hover).
func (s *State) SelectIndex(idx int) {
	items := s.VisibleItems()
	if len(items) == 0 {
		s.setRawSelectedIndex(s.ActiveTab, 0)
		return
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(items) {
		idx = len(items) - 1
	}
	s.setRawSelectedIndex(s.ActiveTab, idx)
}

// MoveSelection moves selection up (-1) or down (+1) from the current index.
func (s *State) MoveSelection(delta int) {
	items := s.VisibleItems()
	if len(items) == 0 {
		return
	}
	next := s.SelectedIndex() + delta
	if next < 0 {
		next = 0
	}
	if next >= len(items) {
		next = len(items) - 1
	}
	s.setRawSelectedIndex(s.ActiveTab, next)
}

// SetTab switches tabs while preserving each tab's independent selection.
func (s *State) SetTab(tab Tab) {
	if s.ActiveTab == tab {
		return
	}
	s.ActiveTab = tab
	s.ConfirmDeleteItem = nil
	s.clampSelections()
}

// ToggleTab switches between All and Pinned.
func (s *State) ToggleTab() {
	if s.ActiveTab == TabAll {
		s.SetTab(TabPinned)
	} else {
		s.SetTab(TabAll)
	}
}

// ExecuteCopy performs the subprocess/disk I/O to copy an item without mutating State.
func (s *State) ExecuteCopy(ctx context.Context, it Item) error {
	data, mime, err := s.loadItemBytes(ctx, it)
	if err != nil {
		return fmt.Errorf("Failed to read item: %v", err)
	}
	if err := s.copier.Copy(ctx, data, mime); err != nil {
		return fmt.Errorf("Failed to copy: %v", err)
	}
	return nil
}

// ApplyCopyResult updates feedback banners after ExecuteCopy completes.
func (s *State) ApplyCopyResult(err error) error {
	if err != nil {
		s.ErrorMsg = err.Error()
		return err
	}
	s.ErrorMsg = s.storageWarn
	s.StatusMsg = "Copied to clipboard"
	return nil
}

// CopySelected copies the currently selected item synchronously.
func (s *State) CopySelected(ctx context.Context) error {
	it := s.SelectedItem()
	if it == nil {
		return nil
	}
	return s.CopyItem(ctx, *it)
}

// CopyItem copies the given item synchronously.
func (s *State) CopyItem(ctx context.Context, it Item) error {
	return s.ApplyCopyResult(s.ExecuteCopy(ctx, it))
}

// ExecuteTogglePin performs pin/unpin I/O off the UI thread without mutating State.
func (s *State) ExecuteTogglePin(ctx context.Context, it Item) ([]pins.Pin, string, error) {
	if s.pins == nil {
		return nil, "", errors.New("pin storage is unavailable")
	}
	if it.IsPinned {
		if err := s.pins.Unpin(it.PinHash); err != nil {
			return s.pins.List(), "", fmt.Errorf("Failed to unpin item: %v", err)
		}
		return s.pins.List(), "Unpinned item", nil
	}

	data, err := s.history.Decode(ctx, it.RawEntry)
	if err != nil {
		return s.pins.List(), "", fmt.Errorf("Failed to decode entry for pinning: %v", err)
	}
	mime := clipboard.ResolveMIME(data, it.MIMEType)
	isBinary := it.IsBinary || !isEditableText(data, mime)

	if _, err := s.pins.Pin(data, it.OriginID, mime, it.Preview, isBinary); err != nil {
		return s.pins.List(), "", fmt.Errorf("Failed to pin item: %v", err)
	}
	return s.pins.List(), "Moved to Pinned", nil
}

// ApplyTogglePinResult updates State after ExecuteTogglePin completes.
func (s *State) ApplyTogglePinResult(updatedPins []pins.Pin, status string, err error) error {
	if updatedPins != nil {
		s.pinnedEntries = updatedPins
	}
	if err != nil {
		s.ErrorMsg = err.Error()
		s.clampSelections()
		return err
	}
	s.ErrorMsg = s.storageWarn
	s.StatusMsg = status
	s.clampSelections()
	return nil
}

// TogglePinSelected pins (from All) or unpins (from Pinned) synchronously.
func (s *State) TogglePinSelected(ctx context.Context) error {
	it := s.SelectedItem()
	if it == nil {
		return nil
	}
	return s.TogglePinItem(ctx, *it)
}

// TogglePinItem moves an item from All -> Pinned or Pinned -> All synchronously.
func (s *State) TogglePinItem(ctx context.Context, it Item) error {
	updatedPins, status, err := s.ExecuteTogglePin(ctx, it)
	return s.ApplyTogglePinResult(updatedPins, status, err)
}

// RequestDeleteSelected arms the confirmation prompt for the selected item.
func (s *State) RequestDeleteSelected() {
	it := s.SelectedItem()
	if it == nil {
		return
	}
	s.RequestDeleteItem(*it)
}

// RequestDeleteItem arms the confirmation prompt for a specific item.
func (s *State) RequestDeleteItem(it Item) {
	copyItem := it
	s.ConfirmDeleteItem = &copyItem
}

// CancelDelete dismisses the delete confirmation prompt.
func (s *State) CancelDelete() {
	s.ConfirmDeleteItem = nil
}

// ExecuteDelete performs the confirmed deletion I/O without mutating State.
func (s *State) ExecuteDelete(ctx context.Context, it Item) ([]pins.Pin, error) {
	if it.IsPinned {
		if s.pins == nil {
			return nil, errors.New("pin storage is unavailable")
		}
		if err := s.pins.Unpin(it.PinHash); err != nil {
			return s.pins.List(), fmt.Errorf("Failed to remove pinned item: %v", err)
		}
		return s.pins.List(), nil
	}
	if err := s.history.Delete(ctx, it.RawEntry); err != nil {
		return nil, fmt.Errorf("Failed to delete history entry: %v", err)
	}
	return nil, nil
}

// ApplyDeleteResult updates State after ExecuteDelete completes.
func (s *State) ApplyDeleteResult(it Item, updatedPins []pins.Pin, err error) error {
	if it.IsPinned {
		if updatedPins != nil {
			s.pinnedEntries = updatedPins
		}
		if err != nil {
			s.ErrorMsg = err.Error()
			s.clampSelections()
			return err
		}
		s.ErrorMsg = s.storageWarn
		s.StatusMsg = "Removed pinned item"
		s.clampSelections()
		return nil
	}

	if err != nil {
		s.ErrorMsg = err.Error()
		s.clampSelections()
		return err
	}
	filtered := make([]cliphist.Entry, 0, len(s.historyEntries))
	for _, e := range s.historyEntries {
		if e.ID != it.OriginID {
			filtered = append(filtered, e)
		}
	}
	s.historyEntries = filtered
	s.ErrorMsg = s.storageWarn
	s.StatusMsg = "Deleted history entry"
	s.clampSelections()
	return nil
}

// ConfirmDelete executes the confirmed delete synchronously.
func (s *State) ConfirmDelete(ctx context.Context) error {
	if s.ConfirmDeleteItem == nil {
		return nil
	}
	it := *s.ConfirmDeleteItem
	s.ConfirmDeleteItem = nil
	updatedPins, err := s.ExecuteDelete(ctx, it)
	return s.ApplyDeleteResult(it, updatedPins, err)
}

// ExecuteLoadForEdit loads and validates editable UTF-8 text off the UI thread.
func (s *State) ExecuteLoadForEdit(ctx context.Context, it Item) (string, bool, error) {
	if it.IsBinary {
		return "", false, nil
	}
	data, mime, err := s.loadItemBytes(ctx, it)
	if err != nil {
		return "", false, fmt.Errorf("Failed to load text for editing: %v", err)
	}
	if !isEditableText(data, mime) {
		return "", false, nil
	}
	return string(data), true, nil
}

// ApplyLoadForEdit opens the edit view or shows a non-text status hint.
func (s *State) ApplyLoadForEdit(it Item, draft string, editable bool, err error) error {
	if err != nil {
		s.ErrorMsg = err.Error()
		return err
	}
	if !editable {
		s.StatusMsg = "Editing is only available for text items"
		return nil
	}
	copyItem := it
	s.EditingItem = &copyItem
	s.EditDraft = draft
	s.ConfirmDeleteItem = nil
	s.ErrorMsg = s.storageWarn
	return nil
}

// StartEditSelected opens the simple text editor synchronously.
func (s *State) StartEditSelected(ctx context.Context) error {
	it := s.SelectedItem()
	if it == nil {
		return nil
	}
	return s.StartEditItem(ctx, *it)
}

// StartEditItem loads text bytes into the editor synchronously.
func (s *State) StartEditItem(ctx context.Context, it Item) error {
	draft, editable, err := s.ExecuteLoadForEdit(ctx, it)
	return s.ApplyLoadForEdit(it, draft, editable, err)
}

// ExecuteSaveEdit copies edited UTF-8 text via wl-copy without mutating State.
func (s *State) ExecuteSaveEdit(ctx context.Context, editedText string) error {
	if err := s.copier.Copy(ctx, []byte(editedText), "text/plain;charset=utf-8"); err != nil {
		return fmt.Errorf("Failed to copy edited text: %v", err)
	}
	return nil
}

// ApplySaveEdit updates State after ExecuteSaveEdit completes.
func (s *State) ApplySaveEdit(err error) error {
	if err != nil {
		s.ErrorMsg = err.Error()
		return err
	}
	s.EditingItem = nil
	s.EditDraft = ""
	s.ErrorMsg = s.storageWarn
	s.StatusMsg = "Edited text copied to clipboard"
	return nil
}

// SaveEdit copies the edited text to the clipboard synchronously.
func (s *State) SaveEdit(ctx context.Context, editedText string) error {
	if s.EditingItem == nil {
		return nil
	}
	return s.ApplySaveEdit(s.ExecuteSaveEdit(ctx, editedText))
}

// CancelEdit discards edits and closes the editing view.
func (s *State) CancelEdit() {
	s.EditingItem = nil
	s.EditDraft = ""
}

func (s *State) loadItemBytes(ctx context.Context, it Item) ([]byte, string, error) {
	if it.IsPinned {
		if s.pins == nil {
			return nil, "", errors.New("pin storage is unavailable")
		}
		data, err := s.pins.ReadPayload(it.PinHash)
		if err != nil {
			return nil, "", err
		}
		return data, clipboard.ResolveMIME(data, it.MIMEType), nil
	}
	data, err := s.history.Decode(ctx, it.RawEntry)
	if err != nil {
		return nil, "", err
	}
	return data, clipboard.ResolveMIME(data, it.MIMEType), nil
}

func (s *State) rawSelectedIndex(tab Tab) int {
	if tab == TabPinned {
		return s.selectedPinned
	}
	return s.selectedAll
}

func (s *State) setRawSelectedIndex(tab Tab, idx int) {
	if tab == TabPinned {
		s.selectedPinned = idx
		return
	}
	s.selectedAll = idx
}

func (s *State) clampSelections() {
	clamp := func(tab Tab) {
		n := len(s.visibleItemsFor(tab))
		if n == 0 {
			s.setRawSelectedIndex(tab, 0)
			return
		}
		idx := s.rawSelectedIndex(tab)
		if idx < 0 {
			s.setRawSelectedIndex(tab, 0)
		} else if idx >= n {
			s.setRawSelectedIndex(tab, n-1)
		}
	}
	clamp(TabAll)
	clamp(TabPinned)
}

func isEditableText(data []byte, mime string) bool {
	if strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/") {
		return false
	}
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	return utf8.Valid(data)
}
