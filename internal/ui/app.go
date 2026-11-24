package ui

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

const (
	cmdTimeout     = 5 * time.Second
	opQueueCap     = 32
	resultQueueCap = 32
)

type asyncOp struct {
	work func(ctx context.Context) func()
}

// App renders the Gio Tokyo Night clipboard manager window.
type App struct {
	runCtx    context.Context
	runCancel context.CancelFunc
	window    *app.Window
	theme     *material.Theme
	state     *State

	// Single-worker FIFO operation queue coordinated by opMu so enqueue() and
	// Close() never race on shutdown, pending WaitGroup is always balanced, and
	// user actions are never silently dropped while another command runs.
	opMu      sync.Mutex
	closed    bool
	ops       chan asyncOp
	results   chan func()
	pending   sync.WaitGroup // Tracks queued + running async operations (used by WaitIdle in tests).
	workerWG  sync.WaitGroup // Tracks worker and signal-watcher goroutines for clean shutdown.
	closeOnce sync.Once
	focusEdit bool

	// Header & tab widgets.
	searchEdit   widget.Editor
	refreshBtn   widget.Clickable
	tabAllBtn    widget.Clickable
	tabPinnedBtn widget.Clickable

	// Scrollable list state and per-row interaction slots (reused across frames).
	list       widget.List
	rowClicks  []widget.Clickable
	rowHover   []int // Unique stable pointer tags per visible slot
	copyBtns   []widget.Clickable
	pinBtns    []widget.Clickable
	editBtns   []widget.Clickable
	deleteBtns []widget.Clickable

	// Delete confirmation & simple text editor widgets.
	confirmYesBtn widget.Clickable
	confirmNoBtn  widget.Clickable
	editEditor    widget.Editor
	editSaveBtn   widget.Clickable
	editCancelBtn widget.Clickable

	// Root keyboard event tag.
	rootTag int
}

// NewApp constructs the Gio UI controller and starts its single FIFO worker.
func NewApp(parentCtx context.Context, w *app.Window, state *State) *App {
	runCtx, runCancel := context.WithCancel(parentCtx)
	a := &App{
		runCtx:    runCtx,
		runCancel: runCancel,
		window:    w,
		theme:     NewTokyoNightTheme(),
		state:     state,
		ops:       make(chan asyncOp, opQueueCap),
		results:   make(chan func(), resultQueueCap),
		list: widget.List{
			List: layout.List{Axis: layout.Vertical},
		},
	}
	a.searchEdit.SingleLine = true
	a.searchEdit.Submit = false
	a.editEditor.SingleLine = false
	a.editEditor.Submit = false

	a.workerWG.Add(1)
	go a.opWorker()
	return a
}

// Close stops accepting new operations, cancels any in-flight subprocesses,
// closes the queue so the worker drains remaining entries cleanly, and waits
// for all worker goroutines to exit.
func (a *App) Close() {
	a.closeOnce.Do(func() {
		a.opMu.Lock()
		a.closed = true
		a.runCancel()
		close(a.ops)
		a.opMu.Unlock()
		a.workerWG.Wait()
	})
}

// WaitIdle blocks until all currently queued operations have finished executing.
func (a *App) WaitIdle() {
	a.pending.Wait()
}

func (a *App) opWorker() {
	defer a.workerWG.Done()
	for op := range a.ops {
		func() {
			defer a.pending.Done()
			if a.runCtx.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(a.runCtx, cmdTimeout)
			defer cancel()

			apply := op.work(ctx)
			if apply == nil {
				return
			}
			select {
			case a.results <- apply:
				if a.window != nil {
					a.window.Invalidate()
				}
			case <-a.runCtx.Done():
			}
		}()
	}
}

// enqueue appends an operation to the FIFO queue using opMu + a context-aware
// select so operations are never accepted after shutdown and pending is always
// balanced on every rejected operation.
func (a *App) enqueue(work func(ctx context.Context) func()) bool {
	a.opMu.Lock()
	defer a.opMu.Unlock()

	if a.closed || a.runCtx.Err() != nil {
		return false
	}

	a.pending.Add(1)
	select {
	case <-a.runCtx.Done():
		a.pending.Done()
		return false
	case a.ops <- asyncOp{work: work}:
		return true
	default:
		a.pending.Done()
		a.state.ErrorMsg = "Operation queue is busy; please try again"
		return false
	}
}

// Run executes the Gio event loop until the window closes or the parent context
// is canceled, then cancels and waits for all goroutines and subprocesses cleanly.
func (a *App) Run() error {
	defer a.Close()

	// Load initial clipboard history asynchronously so the window opens immediately.
	a.triggerRefresh()

	// Forward external cancellation (SIGINT/SIGTERM) to window close cleanly.
	a.workerWG.Add(1)
	go func() {
		defer a.workerWG.Done()
		<-a.runCtx.Done()
		if a.window != nil {
			a.window.Perform(system.ActionClose)
		}
	}()

	var ops op.Ops
	for {
		switch e := a.window.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			a.Layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

// DrainResults applies any completed async results on the UI goroutine.
func (a *App) DrainResults() {
	for {
		select {
		case fn := <-a.results:
			fn()
		default:
			return
		}
	}
}

func (a *App) triggerRefresh() {
	a.enqueue(func(ctx context.Context) func() {
		res := a.state.FetchRefresh(ctx)
		return func() {
			_ = a.state.ApplyRefresh(res)
		}
	})
}

func (a *App) triggerCopy(it Item) {
	a.enqueue(func(ctx context.Context) func() {
		err := a.state.ExecuteCopy(ctx, it)
		return func() {
			if applyErr := a.state.ApplyCopyResult(err); applyErr == nil && err == nil {
				if a.window != nil {
					a.window.Perform(system.ActionClose)
				}
			}
		}
	})
}

func (a *App) triggerTogglePin(it Item) {
	a.enqueue(func(ctx context.Context) func() {
		updatedPins, status, err := a.state.ExecuteTogglePin(ctx, it)
		return func() {
			_ = a.state.ApplyTogglePinResult(updatedPins, status, err)
		}
	})
}

func (a *App) triggerConfirmDelete() {
	if a.state.ConfirmDeleteItem == nil {
		return
	}
	it := *a.state.ConfirmDeleteItem
	queued := a.enqueue(func(ctx context.Context) func() {
		updatedPins, err := a.state.ExecuteDelete(ctx, it)
		return func() {
			_ = a.state.ApplyDeleteResult(it, updatedPins, err)
		}
	})
	// Only clear the confirmation state once the delete operation is accepted into the queue.
	if queued {
		a.state.ConfirmDeleteItem = nil
	}
}

func (a *App) triggerStartEdit(it Item) {
	a.enqueue(func(ctx context.Context) func() {
		draft, editable, err := a.state.ExecuteLoadForEdit(ctx, it)
		return func() {
			if applyErr := a.state.ApplyLoadForEdit(it, draft, editable, err); applyErr == nil && a.state.EditingItem != nil {
				a.editEditor.SetText(a.state.EditDraft)
				a.focusEdit = true
			}
		}
	})
}

func (a *App) triggerSaveEdit(editedText string) {
	a.enqueue(func(ctx context.Context) func() {
		err := a.state.ExecuteSaveEdit(ctx, editedText)
		return func() {
			if applyErr := a.state.ApplySaveEdit(err); applyErr == nil && err == nil {
				if a.window != nil {
					a.window.Perform(system.ActionClose)
				}
			}
		}
	})
}

// Layout draws one frame of the UI. Exported for headless Gio router testing.
func (a *App) Layout(gtx layout.Context) layout.Dimensions {
	a.DrainResults()

	if a.focusEdit && a.state.EditingItem != nil {
		a.focusEdit = false
		gtx.Execute(key.FocusCmd{Tag: &a.editEditor})
	}

	// Register root event handler and process keyboard shortcuts.
	area := clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops)
	event.Op(gtx.Ops, &a.rootTag)
	a.handleKeyboard(gtx)
	area.Pop()

	// Paint Tokyo Night background.
	fillRect(gtx, ColorBg)

	// Handle search text updates.
	for {
		if _, ok := a.searchEdit.Update(gtx); !ok {
			break
		}
	}
	a.state.SetQuery(a.searchEdit.Text())

	// Handle top-bar button clicks.
	if a.refreshBtn.Clicked(gtx) {
		a.triggerRefresh()
	}
	if a.tabAllBtn.Clicked(gtx) {
		a.state.SetTab(TabAll)
	}
	if a.tabPinnedBtn.Clicked(gtx) {
		a.state.SetTab(TabPinned)
	}

	// If editing a text item, show the clean multiline editing view.
	if a.state.EditingItem != nil {
		return a.layoutEditView(gtx)
	}

	return layout.UniformInset(unit.Dp(12)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(a.layoutHeader),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(a.layoutSearchBox),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(a.layoutTabs),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(horizontalDivider),
			layout.Rigid(a.layoutBanners),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Flexed(1, a.layoutList),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(a.layoutFooter),
		)
	})
}

func (a *App) handleKeyboard(gtx layout.Context) {
	inEdit := a.state.EditingItem != nil
	inConfirm := a.state.ConfirmDeleteItem != nil
	searchFocused := gtx.Focused(&a.searchEdit)

	filters := []event.Filter{
		key.Filter{Name: key.NameEscape},
		key.Filter{Name: "F", Required: key.ModCtrl},
	}

	if inEdit {
		filters = append(filters, key.Filter{Name: "S", Required: key.ModCtrl})
	}

	if !inEdit && !inConfirm {
		filters = append(filters, key.Filter{Name: key.NameTab})

		if !searchFocused {
			filters = append(filters,
				key.Filter{Name: key.NameUpArrow},
				key.Filter{Name: key.NameDownArrow},
				key.Filter{Name: key.NameLeftArrow},
				key.Filter{Name: key.NameRightArrow},
				key.Filter{Name: key.NameReturn},
				key.Filter{Name: key.NameEnter},
				key.Filter{Name: key.NameDeleteForward},
				key.Filter{Name: key.NameSpace},
				key.Filter{Name: "E", Required: key.ModCtrl},
			)
		}
	} else if inConfirm {
		filters = append(filters,
			key.Filter{Name: key.NameReturn},
			key.Filter{Name: key.NameEnter},
			key.Filter{Name: "Y"},
			key.Filter{Name: "N"},
		)
	}

	for {
		ev, ok := gtx.Event(filters...)
		if !ok {
			break
		}
		ke, ok := ev.(key.Event)
		if !ok || ke.State != key.Press {
			continue
		}

		if inEdit {
			if ke.Modifiers.Contain(key.ModCtrl) && ke.Name == "S" {
				a.triggerSaveEdit(a.editEditor.Text())
				continue
			}

			if ke.Name == key.NameEscape {
				a.state.CancelEdit()
			}
			continue
		}

		if inConfirm {
			switch ke.Name {
			case key.NameEscape, "N":
				a.state.CancelDelete()
			case key.NameReturn, key.NameEnter, "Y":
				a.triggerConfirmDelete()
			}
			continue
		}

		if ke.Modifiers.Contain(key.ModCtrl) {
			switch ke.Name {
			case "F":
				gtx.Execute(key.FocusCmd{Tag: &a.searchEdit})
				continue
			case "E":
				if !searchFocused {
					if it := a.state.SelectedItem(); it != nil {
						a.triggerStartEdit(*it)
					}
				}
				continue
			}
		}

		switch ke.Name {
		case key.NameEscape:
			if searchFocused {
				gtx.Execute(key.FocusCmd{Tag: nil})
			} else if a.searchEdit.Text() != "" {
				a.searchEdit.SetText("")
				a.state.SetQuery("")
			} else if a.window != nil {
				a.window.Perform(system.ActionClose)
			}
		case key.NameTab:
			a.state.ToggleTab()
		case key.NameUpArrow:
			if !searchFocused {
				a.state.MoveSelection(-1)
				a.ensureSelectionVisible()
			}
		case key.NameDownArrow:
			if !searchFocused {
				a.state.MoveSelection(1)
				a.ensureSelectionVisible()
			}
		case key.NameLeftArrow:
			if !searchFocused {
				a.state.SetTab(TabAll)
			}
		case key.NameRightArrow:
			if !searchFocused {
				a.state.SetTab(TabPinned)
			}
		case key.NameReturn, key.NameEnter:
			if !searchFocused {
				if it := a.state.SelectedItem(); it != nil {
					a.triggerCopy(*it)
				}
			}
		case key.NameDeleteForward:
			if !searchFocused {
				a.state.RequestDeleteSelected()
			}
		case key.NameSpace:
			if !searchFocused {
				if it := a.state.SelectedItem(); it != nil {
					a.triggerTogglePin(*it)
				}
			}
		}
	}
}

func (a *App) ensureSelectionVisible() {
	idx := a.state.SelectedIndex()
	if idx < 0 {
		return
	}
	if idx < a.list.Position.First {
		a.list.Position.First = idx
		a.list.Position.Offset = 0
	} else if a.list.Position.Count > 0 && idx >= a.list.Position.First+a.list.Position.Count {
		a.list.Position.First = idx - a.list.Position.Count + 1
		if a.list.Position.First < 0 {
			a.list.Position.First = 0
		}
		a.list.Position.Offset = 0
	}
}

// func (a *App) layoutHeader(gtx layout.Context) layout.Dimensions {
// 	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
// 		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
// 			lbl := material.Body1(a.theme, "clipman")
// 			lbl.Color = ColorTextPrimary
// 			lbl.Font.Weight = font.SemiBold
// 			lbl.TextSize = unit.Sp(15)
// 			return lbl.Layout(gtx)
// 		}),
// 		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
// 			return a.smallButton(gtx, &a.refreshBtn, "Refresh", ColorBgSecondary, ColorTextMuted)
// 		}),
// 	)
// }

func (a *App) layoutHeader(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body1(a.theme, "󰅌")
					lbl.Color = ColorCyan
					lbl.TextSize = unit.Sp(28)
					return lbl.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body1(a.theme, "Clipman")
					lbl.Color = ColorTextPrimary
					lbl.Font.Weight = font.SemiBold
					lbl.Font.Style = font.Italic
					lbl.TextSize = unit.Sp(15)
					return layout.Inset{
						Top: unit.Dp(8),
					}.Layout(gtx, lbl.Layout)
				}),
			)
		}),
	)
}

func (a *App) layoutSearchBox(gtx layout.Context) layout.Dimensions {
	borderCol := ColorBorder
	if gtx.Focused(&a.searchEdit) {
		borderCol = ColorBlue
	}
	return widget.Border{
		Color:        borderCol,
		CornerRadius: unit.Dp(1),
		Width:        unit.Dp(1),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return fillRoundedBox(gtx, ColorBg, unit.Dp(1), func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(10), Right: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				ed := material.Editor(a.theme, &a.searchEdit, "Search... (Ctrl+F)")
				ed.Color = ColorTextPrimary
				ed.HintColor = ColorTextMuted
				ed.Font.Style = font.Italic
				ed.TextSize = unit.Sp(13)
				return ed.Layout(gtx)
			})
		})
	})
}

func (a *App) layoutTabs(gtx layout.Context) layout.Dimensions {
	allCount := len(a.state.visibleItemsFor(TabAll))
	pinCount := len(a.state.visibleItemsFor(TabPinned))

	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return a.tabButton(gtx, &a.tabAllBtn, fmt.Sprintf("󰅍 Clips(%d)", allCount), a.state.ActiveTab == TabAll, ColorBlue, ColorTextMuted)
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return a.tabButton(gtx, &a.tabPinnedBtn, fmt.Sprintf("󰉉 Pinned(%d)", pinCount), a.state.ActiveTab == TabPinned, ColorPurple, ColorRed)
		}),
	)
}

func (a *App) tabButton(gtx layout.Context, btn *widget.Clickable, label string, active bool, accent color.NRGBA, inactiveColor color.NRGBA) layout.Dimensions {
	bg := ColorBgSecondary
	fg := inactiveColor
	if active {
		bg = ColorSurface
		fg = accent
	}
	return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return fillRoundedBox(gtx, bg, unit.Dp(1), func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(5), Bottom: unit.Dp(5), Left: unit.Dp(10), Right: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				l := material.Body2(a.theme, label)
				l.Color = fg
				if active {
					l.Font.Weight = font.SemiBold
				}
				return l.Layout(gtx)
			})
		})
	})
}

func (a *App) layoutErrorBanner(gtx layout.Context) layout.Dimensions {
	if a.state.ErrorMsg == "" {
		return layout.Dimensions{}
	}
	errText := a.state.ErrorMsg
	return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return widget.Border{Color: ColorRed, CornerRadius: unit.Dp(6), Width: unit.Dp(1)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return fillRoundedBox(gtx, ColorBgSecondary, unit.Dp(6), func(gtx layout.Context) layout.Dimensions {
				return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					l := material.Caption(a.theme, errText)
					l.Color = ColorRed
					return l.Layout(gtx)
				})
			})
		})
	})
}

// layoutBanners renders both the delete confirmation bar and any active error
// message simultaneously so busy/error messages remain visible during confirmation.
func (a *App) layoutBanners(gtx layout.Context) layout.Dimensions {
	hasConfirm := a.state.ConfirmDeleteItem != nil
	hasError := a.state.ErrorMsg != ""
	if !hasConfirm && !hasError {
		return layout.Dimensions{}
	}

	children := make([]layout.FlexChild, 0, 2)

	if hasConfirm {
		if a.confirmYesBtn.Clicked(gtx) {
			a.triggerConfirmDelete()
		}
		if a.confirmNoBtn.Clicked(gtx) {
			a.state.CancelDelete()
		}
		if a.state.ConfirmDeleteItem != nil {
			isPinned := a.state.ConfirmDeleteItem.IsPinned
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return widget.Border{Color: ColorRed, CornerRadius: unit.Dp(6), Width: unit.Dp(1)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return fillRoundedBox(gtx, ColorBgSecondary, unit.Dp(6), func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(10), Right: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								msg := "Delete selected history item?"
								if isPinned {
									msg = "Remove selected pinned item?"
								}
								return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
									layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
										l := material.Body2(a.theme, msg)
										l.Color = ColorRed
										return l.Layout(gtx)
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
											layout.Rigid(func(gtx layout.Context) layout.Dimensions {
												return a.smallButton(gtx, &a.confirmYesBtn, "Delete", ColorRed, ColorBg)
											}),
											layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
											layout.Rigid(func(gtx layout.Context) layout.Dimensions {
												return a.smallButton(gtx, &a.confirmNoBtn, "Cancel", ColorSurface, ColorTextPrimary)
											}),
										)
									}),
								)
							})
						})
					})
				})
			}))
		}
	}

	if a.state.ErrorMsg != "" {
		children = append(children, layout.Rigid(a.layoutErrorBanner))
	}

	if len(children) == 0 {
		return layout.Dimensions{}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (a *App) layoutList(gtx layout.Context) layout.Dimensions {
	items := a.state.VisibleItems()
	if len(items) == 0 {
		msg := "No clipboard history available."
		if a.state.ActiveTab == TabPinned {
			msg = "No pinned items yet. Press Space on an item in All to pin it."
		} else if strings.TrimSpace(a.state.Query) != "" {
			msg = "No matching clipboard entries."
		}
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			l := material.Body2(a.theme, msg)
			l.Color = ColorTextMuted
			return l.Layout(gtx)
		})
	}

	a.ensureRowSlots(len(items))

	return material.List(a.theme, &a.list).Layout(gtx, len(items), func(gtx layout.Context, i int) layout.Dimensions {
		it := items[i]

		for {
			ev, ok := gtx.Event(pointer.Filter{
				Target: &a.rowHover[i],
				Kinds:  pointer.Enter | pointer.Move,
			})
			if !ok {
				break
			}
			if pe, ok := ev.(pointer.Event); ok && (pe.Kind == pointer.Enter || pe.Kind == pointer.Move) {
				if a.state.SelectedIndex() != i {
					a.state.SelectIndex(i)
				}
			}
		}

		if a.rowClicks[i].Clicked(gtx) {
			a.state.SelectIndex(i)
		}
		if a.copyBtns[i].Clicked(gtx) {
			a.state.SelectIndex(i)
			a.triggerCopy(it)
		}
		if a.pinBtns[i].Clicked(gtx) {
			a.state.SelectIndex(i)
			a.triggerTogglePin(it)
		}
		if !it.IsBinary && a.editBtns[i].Clicked(gtx) {
			a.state.SelectIndex(i)
			a.triggerStartEdit(it)
		}
		if a.deleteBtns[i].Clicked(gtx) {
			a.state.SelectIndex(i)
			a.state.RequestDeleteItem(it)
		}

		isSelected := i == a.state.SelectedIndex()

		return layout.Inset{Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return a.layoutRow(gtx, i, it, isSelected)
		})
	})
}

func (a *App) layoutRow(gtx layout.Context, i int, it Item, selected bool) layout.Dimensions {
	bg := ColorBgSecondary
	borderCol := ColorBgSecondary
	if selected {
		bg = ColorSurface
		borderCol = ColorBlue
		if it.IsPinned {
			borderCol = ColorPurple
		}
	}

	dims := widget.Border{
		Color:        borderCol,
		CornerRadius: unit.Dp(1),
		Width:        unit.Dp(1),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return fillRoundedBox(gtx, bg, unit.Dp(6), func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(10), Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return a.rowClicks[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return a.layoutRowPreview(gtx, it)
						})
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if !selected {
							return layout.Dimensions{}
						}
						return a.layoutRowActions(gtx, i, it)
					}),
				)
			})
		})
	})

	pass := pointer.PassOp{}.Push(gtx.Ops)
	rect := clip.Rect{Max: dims.Size}.Push(gtx.Ops)
	event.Op(gtx.Ops, &a.rowHover[i])
	rect.Pop()
	pass.Pop()

	return dims
}

func (a *App) layoutRowPreview(gtx layout.Context, it Item) layout.Dimensions {
	preview := singleLinePreview(it.Preview, 110)
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !it.IsBinary {
				return layout.Dimensions{}
			}
			tag := "IMG"
			if !strings.HasPrefix(it.MIMEType, "image/") {
				tag = "BIN"
			}
			return layout.Inset{Right: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return fillRoundedBox(gtx, ColorBg, unit.Dp(4), func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(2), Left: unit.Dp(5), Right: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						l := material.Caption(a.theme, tag)
						l.Color = ColorCyan
						l.Font.Weight = font.SemiBold
						return l.Layout(gtx)
					})
				})
			})
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			l := material.Body2(a.theme, preview)
			l.Color = ColorTextPrimary
			l.MaxLines = 2
			return l.Layout(gtx)
		}),
	)
}

func (a *App) layoutRowActions(gtx layout.Context, i int, it Item) layout.Dimensions {
	pinLabel := "󰐃"
	if it.IsPinned {
		pinLabel = "󰐄"
	}
	return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		children := []layout.FlexChild{
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return a.smallButton(gtx, &a.copyBtns[i], "󰆏", ColorBlue, ColorBg)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return a.smallButton(gtx, &a.pinBtns[i], pinLabel, ColorBgSecondary, ColorPurple)
			}),
		}
		if !it.IsBinary {
			children = append(children,
				layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return a.smallButton(gtx, &a.editBtns[i], "󰏫", ColorBgSecondary, ColorCyan)
				}),
			)
		}
		children = append(children,
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return a.smallButton(gtx, &a.deleteBtns[i], "󰆴", ColorBgSecondary, ColorRed)
			}),
		)
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, children...)
	})
}

func (a *App) layoutEditView(gtx layout.Context) layout.Dimensions {
	if a.editSaveBtn.Clicked(gtx) {
		a.triggerSaveEdit(a.editEditor.Text())
	}
	if a.editCancelBtn.Clicked(gtx) {
		a.state.CancelEdit()
	}

	return layout.UniformInset(unit.Dp(12)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						l := material.Body1(a.theme, "Edit text (copies as new clipboard entry)")
						l.Color = ColorCyan
						l.Font.Weight = font.SemiBold
						return l.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return a.smallButton(gtx, &a.editSaveBtn, "Save(ctrl+s)", ColorGreen, ColorBg)
							}),
							layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return a.smallButton(gtx, &a.editCancelBtn, "Esc", ColorSurface, ColorTextPrimary)
							}),
						)
					}),
				)
			}),
			layout.Rigid(a.layoutErrorBanner),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return widget.Border{Color: ColorBlue, CornerRadius: unit.Dp(1), Width: unit.Dp(1)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return fillRoundedBox(gtx, ColorBg, unit.Dp(6), func(gtx layout.Context) layout.Dimensions {
						return layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							ed := material.Editor(a.theme, &a.editEditor, "")
							ed.Color = ColorTextPrimary
							ed.TextSize = unit.Sp(15)
							return ed.Layout(gtx)
						})
					})
				})
			}),
		)
	})
}

func (a *App) layoutFooter(gtx layout.Context) layout.Dimensions {
	status := a.state.StatusMsg
	statusCol := ColorGreen
	if status == "" {
		status = "Enter Copy  ·  Space Pin  ·  Ctrl+E Edit  ·  Del Delete  ·  Tab Switch  ·  Esc Close"
		statusCol = ColorTextMuted
	}
	l := material.Caption(a.theme, status)
	l.Color = statusCol
	l.TextSize = unit.Sp(11.5)
	return l.Layout(gtx)
}

func (a *App) smallButton(gtx layout.Context, btn *widget.Clickable, label string, bg, fg color.NRGBA) layout.Dimensions {
	return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return fillRoundedBox(gtx, bg, unit.Dp(4), func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(7), Right: unit.Dp(7)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				l := material.Caption(a.theme, label)
				l.Color = fg
				l.Font.Weight = font.Medium
				l.TextSize = unit.Sp(11.5)
				return l.Layout(gtx)
			})
		})
	})
}

func (a *App) ensureRowSlots(n int) {
	if len(a.rowClicks) < n {
		a.rowClicks = make([]widget.Clickable, n)
		a.rowHover = make([]int, n)
		a.copyBtns = make([]widget.Clickable, n)
		a.pinBtns = make([]widget.Clickable, n)
		a.editBtns = make([]widget.Clickable, n)
		a.deleteBtns = make([]widget.Clickable, n)
	}
}

func singleLinePreview(s string, maxRunes int) string {
	clean := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
	r := []rune(clean)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return clean
}
