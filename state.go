package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"gorex/internal/rex"
)

// Tab is a tab of the window: a window of the server's session, its
// panes in a tree of splits. The server owns them: the app shows its view
// and asks it for every change.
type Tab struct {
	ID   int
	WID  string // the server's window
	Name string // the name the user gave it, "" to follow its panes
	Root *Node
	// Focus is the pane with the focus, and Zoom the pane that fills the
	// tab, when one does.
	Focus *Pane
	Zoom  *Pane
}

// Node is a pane, or a split of two nodes.
type Node struct {
	ID     int
	Pane   *Pane
	Parent *Node
	// Vertical stacks A above B; otherwise A is left of B. Ratio is A's
	// share of the room.
	Vertical bool
	Ratio    float32
	A, B     *Node
}

// Pane is a terminal attached to a block of the server.
type Pane struct {
	ID     int
	Block  string
	Tab    *Tab
	Node   *Node
	term   *terminal.Terminal
	stream *rex.Stream

	// info is what the server last said of the block.
	info rex.BlockInfo
	// title is the last title the program set.
	title string
	// lastData is when the block last printed, in Unix nanoseconds.
	lastData atomic.Int64
	// attention marks a pane that rang the bell or whose program finished
	// while it was out of sight; seen clears it.
	attention bool
	// bounds is where the pane's card was in the last frame.
	bounds   ui.Rect
	closed   bool
	startDir string
}

// App is the state of GoRex's window.
type App struct {
	win    *mygo.Window
	client *rex.Client
	hello  rex.Hello
	err    string

	// session is the server's session the window shows, and revision the
	// revision of its view the tabs show.
	session  string
	revision int
	tabs     []*Tab
	active   int
	// panes are the panes by their blocks, and keys the keys of the
	// server's IDs, which name tabs, splits and panes in the view.
	panes  map[string]*Pane
	keys   map[string]int
	nextID int
	// infos are what the server last said of the blocks.
	infos map[string]rex.BlockInfo

	// focusReq asks the view to give the keyboard focus to a pane's
	// terminal.
	focusReq *Pane

	// The command palette, the host's popover and the tab being renamed.
	paletteOpen  bool
	paletteQuery string
	paletteSel   int
	hostOpen     bool
	renaming     *Tab
	renameText   string
	renameFocus  bool
	quitting     bool
	focusedWin   bool
	title        string

	// posted are changes to make in the next frame, from terminals of a
	// view without a window, as in tests.
	postMu sync.Mutex
	posted []func()
	// services keeps redraw requests available to menu callbacks.
	services ui.Services
}

func (a *App) post(fn func()) {
	a.postMu.Lock()
	a.posted = append(a.posted, fn)
	a.postMu.Unlock()
}

// update runs fn on the main thread, in the window, or in the next frame
// of a view without one.
func (a *App) update(fn func()) {
	if win := a.win; win != nil {
		win.Update(fn)
	} else {
		a.post(fn)
	}
}

// later changes the tree of tabs and panes once the frame building now
// is built, as closing what it is building would pull it from under it.
func (a *App) later(c *ui.Context, fn func()) {
	a.post(fn)
	c.Invalidate()
}

// runPosted makes the changes posted since the last frame.
func (a *App) runPosted() {
	a.postMu.Lock()
	fns := a.posted
	a.posted = nil
	a.postMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func (a *App) id() int {
	a.nextID++
	return a.nextID
}

// key returns the key of a server's ID, the same as long as the app runs.
func (a *App) key(id string) int {
	if a.keys == nil {
		a.keys = map[string]int{}
	}
	k, ok := a.keys[id]
	if !ok {
		k = a.id()
		a.keys[id] = k
	}
	return k
}

func (a *App) tab() *Tab {
	if a.active >= 0 && a.active < len(a.tabs) {
		return a.tabs[a.active]
	}
	return nil
}

// panes returns the panes of a node in order, left to right and top to
// bottom.
func (n *Node) panes() []*Pane {
	if n == nil {
		return nil
	}
	if n.Pane != nil {
		return []*Pane{n.Pane}
	}
	return append(n.A.panes(), n.B.panes()...)
}

func (t *Tab) panes() []*Pane { return t.Root.panes() }

// setFocus shows a pane as its tab's focused one; focus asks the server.
func (t *Tab) setFocus(p *Pane) {
	if p == nil {
		return
	}
	t.Focus = p
	p.attention = false
}

// call calls one of the server's methods on the window's session.
func (a *App) call(method string, params map[string]any, out any) error {
	params["session_id"] = a.session
	err := a.client.Call(method, params, out)
	if err != nil {
		log.Printf("%s: %v", method, err)
		a.err = err.Error()
	}
	return err
}

// start shows the server's first session, or a new one, with a tab in
// home when it has none.
func (a *App) start() {
	var list struct {
		Sessions []struct {
			ID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := a.client.Call("session.list", map[string]any{}, &list); err != nil {
		a.err = err.Error()
		return
	}
	if len(list.Sessions) > 0 {
		a.session = list.Sessions[0].ID
	} else {
		var s struct {
			ID string `json:"session_id"`
		}
		if err := a.client.Call("session.create", map[string]any{}, &s); err != nil {
			a.err = err.Error()
			return
		}
		a.session = s.ID
	}
	a.client.OnEvent(a.onEvent)
	if a.call("session.attach", map[string]any{}, nil) != nil {
		return
	}
	a.refreshInfos()
	a.sync()
	if len(a.tabs) == 0 {
		home, _ := os.UserHomeDir()
		a.newTab(home)
	}
}

// onEvent takes an event of the session, on the client's goroutine: a
// view newer than the tabs show is fetched on the main thread.
func (a *App) onEvent(e rex.Event) {
	var se rex.SessionEvent
	if e.Type != "session_event" || json.Unmarshal(e.Payload, &se) != nil || se.Event.Type != "session_view_changed" {
		return
	}
	var p struct {
		Revision int `json:"revision"`
	}
	if json.Unmarshal(se.Event.Payload, &p) != nil {
		return
	}
	a.update(func() {
		if se.SessionID == a.session && p.Revision > a.revision {
			a.sync()
		}
	})
}

// sync fetches the session's view and shows it.
func (a *App) sync() {
	var v rex.SessionView
	if a.call("session.view", map[string]any{}, &v) == nil {
		a.show(v)
	}
}

// show makes the tabs those of a view of the session: tabs and panes it
// still has stay, their terminals attached; those it lost close.
func (a *App) show(v rex.SessionView) {
	if a.panes == nil {
		a.panes = map[string]*Pane{}
	}
	var focusBefore *Pane
	if t := a.tab(); t != nil {
		focusBefore = t.Focus
	}
	byWID := map[string]*Tab{}
	for _, t := range a.tabs {
		byWID[t.WID] = t
	}
	live := map[string]bool{}
	var tabs []*Tab
	active := 0
	for i, w := range v.Windows {
		t := byWID[w.ID]
		if t == nil {
			t = &Tab{ID: a.key(w.ID), WID: w.ID}
		}
		t.Name = w.Label
		if t.Name == rex.DefaultWindowLabel(w.ID) {
			t.Name = ""
		}
		t.Root = a.layoutNode(w.Layout, t, nil, live)
		focus := a.panes[w.Focus]
		if focus != t.Focus {
			t.setFocus(focus)
		}
		t.Zoom = a.panes[w.Zoom]
		tabs = append(tabs, t)
		if w.ID == v.Active {
			active = i
		}
	}
	for id, p := range a.panes {
		if !live[id] {
			p.closed = true
			if p.term != nil {
				p.term.Close()
			}
			delete(a.panes, id)
		}
	}
	a.tabs, a.active, a.revision = tabs, active, v.Revision
	if a.renaming != nil && !slices.Contains(tabs, a.renaming) {
		a.renaming = nil
	}
	if t := a.tab(); t != nil && t.Focus != focusBefore {
		a.focusReq = t.Focus
	}
	if len(tabs) == 0 && a.win != nil {
		a.win.Close()
	}
}

// layoutNode makes the node of a layout, its panes those of the blocks the app
// has, or new ones attached to theirs.
func (a *App) layoutNode(l *rex.LayoutNode, t *Tab, parent *Node, live map[string]bool) *Node {
	if l.Block != nil {
		id := l.Block.ID
		p := a.panes[id]
		if p == nil {
			in := a.infos[id]
			p = &Pane{ID: a.key(id), Block: id, info: in, startDir: in.Dir}
			if in.Cols == 0 {
				in.Cols, in.Rows = 80, 24
			}
			a.attach(p, in.Cols, in.Rows)
			a.panes[id] = p
		}
		live[id] = true
		n := &Node{ID: p.ID, Pane: p, Parent: parent}
		p.Tab, p.Node = t, n
		return n
	}
	sp := l.Split
	n := &Node{ID: a.key(sp.ID), Parent: parent, Vertical: sp.Direction == "vertical", Ratio: float32(sp.Ratio)}
	n.A, n.B = a.layoutNode(sp.Before, t, n, live), a.layoutNode(sp.After, t, n, live)
	return n
}

// attach makes the pane's terminal, attached to its block.
func (a *App) attach(p *Pane, cols, rows int) {
	p.stream = a.client.Stream(p.Block, cols, rows)
	p.stream.OnData = func(int) { p.lastData.Store(time.Now().UnixNano()) }
	term, err := terminal.New(terminal.Options{
		Conn:        p.stream,
		Font:        termFont,
		Theme:       lightTerm,
		DarkTheme:   darkTerm,
		Transparent: true,
		OnTitle: func(title string) {
			a.update(func() { p.title = title })
		},
		OnBell: func() {
			a.update(func() { a.ring(p) })
		},
		OnNotify: func(title, body string) {
			if title == "" {
				title = "GoRex"
			}
			mygo.NewNotification(mygo.NotificationOptions{Title: title, Body: body}).Show()
		},
		// A program that exits closes its block on the server, whose view
		// takes the pane away.
	})
	if err != nil {
		log.Printf("terminal: %v", err)
		a.err = err.Error()
		return
	}
	// Until a view shows it, the terminal has the block's size, so that
	// attaching changes nothing in the block.
	term.Resize(cols, rows)
	p.term = term
}

func (a *App) ring(p *Pane) {
	if t := a.tab(); t == nil || t.Focus != p || p.Tab != t || !a.focusedWin {
		p.attention = true
	}
}

// shell is the layout of a block of the user's shell in dir.
func shell(dir string, cols, rows int) *rex.LayoutNode {
	return &rex.LayoutNode{Block: &rex.LayoutBlock{Flavor: rex.ShellFlavor, Options: &rex.BlockOptions{Dir: dir, Cols: cols, Rows: rows}}}
}

// newTab opens a tab with a shell in dir, after the active tab.
func (a *App) newTab(dir string) *Tab {
	var w struct {
		WindowID string `json:"window_id"`
	}
	if a.call("session.new_window", map[string]any{"layout": shell(dir, 0, 0)}, &w) != nil {
		return nil
	}
	if at := a.active + 1; at < len(a.tabs) {
		a.call("session.move_window", map[string]any{"window_id": w.WindowID, "before_window_id": a.tabs[at].WID}, nil)
	}
	a.sync()
	for _, t := range a.tabs {
		if t.WID == w.WindowID {
			return t
		}
	}
	return nil
}

// currentDir returns the directory new panes open in: the focused pane's.
func (a *App) currentDir() string {
	if t := a.tab(); t != nil && t.Focus != nil {
		if d := t.Focus.info.Dir; d != "" {
			return d
		}
	}
	home, _ := os.UserHomeDir()
	return home
}

// split splits the focused pane, the new pane right of it or below it.
func (a *App) split(vertical bool) {
	t := a.tab()
	if t == nil || t.Focus == nil {
		return
	}
	cols, rows := 80, 24
	if term := t.Focus.term; term != nil {
		cols, rows = term.Size()
		if vertical {
			rows = max(rows/2, 2)
		} else {
			cols = max(cols/2, 10)
		}
	}
	a.call("session.new_split", map[string]any{
		"anchor_id": t.Focus.Block, "direction": direction(vertical), "side": "after",
		"layout": shell(a.currentDir(), cols, rows), "focus": true,
	}, nil)
	a.sync()
}

func direction(vertical bool) string {
	if vertical {
		return "vertical"
	}
	return "horizontal"
}

// closePane closes a pane and ends its block.
func (a *App) closePane(p *Pane) {
	if p.closed {
		return
	}
	a.call("block.close", map[string]any{"block_id": p.Block}, nil)
	a.sync()
}

// closeTab closes a tab and ends the blocks of its panes.
func (a *App) closeTab(t *Tab) {
	a.call("session.close_window", map[string]any{"window_id": t.WID}, nil)
	a.sync()
}

func (a *App) selectTab(i int) {
	if i < 0 || i >= len(a.tabs) {
		return
	}
	a.call("session.focus_window", map[string]any{"window_id": a.tabs[i].WID}, nil)
	a.sync()
	if t := a.tab(); t != nil && t.Focus != nil {
		t.Focus.attention = false
		a.focusReq = t.Focus
	}
}

// moveTab moves a tab to the place of the nth.
func (a *App) moveTab(t *Tab, to int) {
	order := slices.DeleteFunc(slices.Clone(a.tabs), func(o *Tab) bool { return o == t })
	before := ""
	if to < len(order) {
		before = order[to].WID
	}
	if slices.Index(a.tabs, t) == to {
		return
	}
	a.call("session.move_window", map[string]any{"window_id": t.WID, "before_window_id": before}, nil)
	a.sync()
}

// setTabName names a tab; an empty name follows its panes again.
func (a *App) setTabName(t *Tab, name string) {
	a.call("session.set_window_label", map[string]any{"window_id": t.WID, "label": name}, nil)
	a.sync()
}

// focus gives a pane the focus of its tab.
func (a *App) focus(p *Pane) {
	if p == nil || p.Tab == nil {
		return
	}
	p.Tab.setFocus(p)
	a.call("session.focus_block", map[string]any{"block_id": p.Block}, nil)
}

// moveFocus gives the focus to the pane next to the focused one, toward
// dx, dy.
func (a *App) moveFocus(dx, dy int) {
	t := a.tab()
	if t == nil || t.Focus == nil || t.Zoom != nil {
		return
	}
	from := t.Focus.bounds
	cx, cy := from.X+from.W/2, from.Y+from.H/2
	var best *Pane
	bestScore := float32(1e9)
	for _, p := range t.panes() {
		if p == t.Focus {
			continue
		}
		r := p.bounds
		var dist, overlap float32
		switch {
		case dx > 0:
			dist = r.X - (from.X + from.W)
			overlap = min(r.Y+r.H, from.Y+from.H) - max(r.Y, from.Y)
		case dx < 0:
			dist = from.X - (r.X + r.W)
			overlap = min(r.Y+r.H, from.Y+from.H) - max(r.Y, from.Y)
		case dy > 0:
			dist = r.Y - (from.Y + from.H)
			overlap = min(r.X+r.W, from.X+from.W) - max(r.X, from.X)
		default:
			dist = from.Y - (r.Y + r.H)
			overlap = min(r.X+r.W, from.X+from.W) - max(r.X, from.X)
		}
		if dist < -4 || overlap <= 0 {
			continue
		}
		// The nearest, then the one most in line with the focused pane's
		// center.
		px, py := r.X+r.W/2, r.Y+r.H/2
		off := px - cx
		if dx != 0 {
			off = py - cy
		}
		score := dist*4 + abs(off)
		if score < bestScore {
			best, bestScore = p, score
		}
	}
	if best != nil {
		a.focus(best)
		a.focusReq = best
	}
}

func abs(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

// setRatio moves the divider of a split, giving its first side ratio of
// the room.
func (a *App) setRatio(n *Node, ratio float32) {
	n.Ratio = ratio
	a.call("session.resize_pane", map[string]any{
		"direction": direction(n.Vertical), "ratio": float64(ratio),
		"before_block_id": n.A.panes()[0].Block, "after_block_id": n.B.panes()[0].Block,
	}, nil)
}

// resizeFocused moves the nearest divider of the focused pane on the
// axis of dx, dy.
func (a *App) resizeFocused(dx, dy int) {
	t := a.tab()
	if t == nil || t.Focus == nil {
		return
	}
	vertical := dy != 0
	for n := t.Focus.Node; n.Parent != nil; n = n.Parent {
		p := n.Parent
		if p.Vertical != vertical {
			continue
		}
		step := float32(0.05)
		if dx < 0 || dy < 0 {
			step = -step
		}
		a.setRatio(p, min(max(p.Ratio+step, 0.1), 0.9))
		a.sync()
		return
	}
}

// equalize gives every split of the tab equal shares, by the panes on
// either side.
func (a *App) equalize() {
	t := a.tab()
	if t == nil {
		return
	}
	var walk func(n *Node) (cols, rows int)
	walk = func(n *Node) (int, int) {
		if n.Pane != nil {
			return 1, 1
		}
		ac, ar := walk(n.A)
		bc, br := walk(n.B)
		if n.Vertical {
			a.setRatio(n, float32(ar)/float32(ar+br))
			return max(ac, bc), ar + br
		}
		a.setRatio(n, float32(ac)/float32(ac+bc))
		return ac + bc, max(ar, br)
	}
	walk(t.Root)
	a.sync()
}

func (a *App) toggleZoom() {
	t := a.tab()
	if t == nil || t.Focus == nil {
		return
	}
	zoom := t.Zoom == nil
	if zoom && len(t.panes()) < 2 {
		return
	}
	a.call("session.zoom_block", map[string]any{"block_id": t.Focus.Block, "zoom": zoom}, nil)
	a.sync()
	a.focusReq = t.Focus
}

// label returns how a pane names itself: its program, or the title the
// program set, and the directory it works in.
func (p *Pane) label() (name, detail string) {
	in := p.info
	prog := programOf(in.Program)
	dir := shortDir(in.Dir)
	if in.Program == "" {
		return in.Shell, shortDir(p.startDir)
	}
	if in.Idle || prog.Shell {
		name := in.Program
		if name == "" {
			name = in.Shell
		}
		return name, dir
	}
	if t := strings.TrimSpace(p.title); t != "" && !titleOfShell(t, in) {
		return t, ""
	}
	name = prog.Name
	if name == "ssh" || in.Program == "ssh" {
		if host := sshHost(in.Args); host != "" {
			return "SSH", host
		}
	}
	return name, dir
}

// titleOfShell tells a title a shell set, naming the program or the
// directory, from one a program set of its own.
func titleOfShell(title string, in rex.BlockInfo) bool {
	l := strings.ToLower(title)
	for _, w := range []string{strings.ToLower(in.Program), strings.ToLower(in.Shell)} {
		// Windows's consoles title themselves with the shell's file:
		// "C:\WINDOWS\system32\cmd.exe - ping".
		if w != "" && (strings.HasPrefix(l, w) || strings.Contains(l, `\`+w+".exe")) {
			return true
		}
	}
	if home, _ := os.UserHomeDir(); strings.Contains(title, "~") || (home != "" && strings.Contains(title, home)) {
		return true
	}
	return strings.Contains(title, "@") && strings.Contains(title, ":")
}

func sshHost(args []string) string {
	for i := 1; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if len(a) == 2 && strings.Contains("bcDEeFIiJLlmOopQRSWw", a[1:]) {
				i++
			}
			continue
		}
		return a
	}
	return ""
}

// shortDir shows a directory as shells do, from ~, and only its last
// parts when it is deep.
func shortDir(d string) string {
	if d == "" {
		return ""
	}
	sep := string(filepath.Separator)
	if home, _ := os.UserHomeDir(); home != "" {
		if samePath(d, home) {
			return "~"
		}
		if len(d) > len(home) && samePath(d[:len(home)], home) && d[len(home)] == filepath.Separator {
			d = "~" + d[len(home):]
		}
	}
	parts := strings.Split(d, sep)
	if len(parts) > 5 || len(d) > 48 && len(parts) > 3 {
		return "…" + sep + strings.Join(parts[len(parts)-2:], sep)
	}
	return d
}

// samePath reports whether two paths are the same, without case on
// Windows.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// tabLabel names a tab: its name, or its focused pane's.
func (t *Tab) label() (name, detail string) {
	if t.Name != "" {
		return t.Name, ""
	}
	if t.Focus == nil {
		return "Shell", ""
	}
	return t.Focus.label()
}

// refreshInfos asks the server what runs in each block.
func (a *App) refreshInfos() {
	infos, err := a.client.List()
	if err != nil {
		return
	}
	byID := make(map[string]rex.BlockInfo, len(infos))
	for _, in := range infos {
		byID[in.ID] = in
	}
	a.apply(byID)
}

// poll asks the server what runs in each block, every half second,
// until the window closes.
func (a *App) poll(win *mygo.Window, client *rex.Client) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-client.Closed():
			win.Update(func() {
				if !a.quitting {
					a.err = "Lost the connection to the session server."
				}
			})
			return
		case <-tick.C:
		}
		infos, err := client.List()
		if err != nil {
			continue
		}
		byID := make(map[string]rex.BlockInfo, len(infos))
		for _, in := range infos {
			byID[in.ID] = in
		}
		win.Update(func() {
			if a.win == win {
				a.apply(byID)
			}
		})
	}
}

// apply takes what the server said of the blocks.
func (a *App) apply(byID map[string]rex.BlockInfo) {
	a.infos = byID
	for ti, t := range a.tabs {
		for _, p := range t.panes() {
			in, ok := byID[p.Block]
			if !ok {
				continue
			}
			was := p.info
			p.info = in
			// A program that ran a while and finished out of sight
			// asks for attention.
			if !was.Idle && was.Program != "" && in.Idle && was.Program != in.Program {
				seen := ti == a.active && t.Focus == p && a.focusedWin
				if !seen {
					p.attention = true
				}
				if !a.focusedWin && time.Since(was.LastInput) > 8*time.Second {
					prog := programOf(was.Program)
					n := mygo.NewNotification(mygo.NotificationOptions{
						Title: prog.Name + " finished",
						Body:  "in " + shortDir(in.Dir),
					})
					n.Show()
				}
			}
		}
	}
}

// laterService queues a menu action without keeping a build context.
func (a *App) laterService(fn func()) { a.post(fn); a.services.Invalidate() }
