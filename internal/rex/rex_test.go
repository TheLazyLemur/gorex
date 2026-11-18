//go:build darwin || linux || windows

package rex

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// testShell is the shell the tests' blocks run, and where.
var testShell, testWork = func() (string, string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", os.TempDir()
	}
	return "/bin/sh", "/tmp"
}()

// serve starts a server in a directory of its own and connects to it; the
// channel gets what Serve returns.
func serve(t *testing.T, dir string) (*Client, chan error) {
	t.Helper()
	t.Setenv("GOREX_DIR", dir)
	done := make(chan error, 1)
	go func() { done <- Serve() }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(filepath.Join(dir, "server.sock")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	c, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	return c, done
}

// testDir is a directory of the test's own: in /tmp, whose path is short
// enough for the socket, on macOS.
func testDir(t *testing.T) string {
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = ""
	}
	dir, err := os.MkdirTemp(base, "rex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func shellBlock() *LayoutNode {
	return &LayoutNode{Block: &LayoutBlock{Flavor: ShellFlavor, Options: &BlockOptions{Command: []string{testShell}, Dir: testWork}}}
}

func TestServer(t *testing.T) {
	// The command prints "hello" without the line typed showing it, and
	// runs a program for a while.
	typed, sleeper := "echo he''llo; sleep 1\n", "sleep"
	if runtime.GOOS == "windows" {
		typed, sleeper = "echo he^llo & ping -n 3 127.0.0.1 >nul\r", "ping"
	}
	c, _ := serve(t, testDir(t))
	defer c.Shutdown()
	h, err := c.Hello()
	if err != nil || h.Version != ProtocolVersion {
		t.Fatalf("hello %+v %v", h, err)
	}
	t.Logf("host %+v", h.Host)
	var sess struct {
		SessionID string `json:"session_id"`
	}
	if err := c.Call("session.create", map[string]any{}, &sess); err != nil {
		t.Fatal(err)
	}
	var win struct {
		BlockIDs []string `json:"block_ids"`
	}
	if err := c.Call("session.new_window", map[string]any{"session_id": sess.SessionID, "layout": shellBlock()}, &win); err != nil {
		t.Fatal(err)
	}
	id := win.BlockIDs[0]
	s := c.Stream(id, 80, 24)
	s.Resize(100, 30)
	s.Write([]byte(typed))
	var out bytes.Buffer
	buf := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "hello") && time.Now().Before(deadline) {
		n, err := s.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("output %q", out.String())
	}
	time.Sleep(200 * time.Millisecond)
	list, err := c.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	in := list[0]
	t.Logf("info %+v", in)
	if !strings.EqualFold(in.Program, sleeper) || in.Idle || in.Cols != 100 {
		t.Errorf("info %+v", in)
	}
	if !strings.HasSuffix(strings.ToLower(in.Dir), strings.ToLower(testWork)) {
		t.Errorf("dir %q", in.Dir)
	}
	s.Close()
	// Attaching again replays what it printed.
	s2 := c.Stream(id, 100, 30)
	s2.Resize(100, 30)
	n, _ := s2.Read(buf)
	if !strings.Contains(string(buf[:n]), "hello") {
		t.Errorf("replay %q", buf[:n])
	}
	if err := c.Call("block.close", map[string]any{"session_id": sess.SessionID, "block_id": id}, nil); err != nil {
		t.Fatal(err)
	}
	if list, _ := c.List(); len(list) != 0 {
		t.Errorf("blocks left %+v", list)
	}
}

// TestSessions drives a session's windows and blocks through Rex's
// methods, and checks its events, its errors, and that a new server
// starts it again.
func TestSessions(t *testing.T) {
	dir := testDir(t)
	c, done := serve(t, dir)
	events := make(chan Event, 256)
	c.OnEvent(func(e Event) { events <- e })
	var sess struct {
		ID    string `json:"session_id"`
		Label string `json:"label"`
	}
	if err := c.Call("session.create", map[string]any{}, &sess); err != nil || sess.Label == "" {
		t.Fatalf("create %+v %v", sess, err)
	}
	sid := sess.ID
	call := func(method string, params map[string]any, out any) {
		t.Helper()
		params["session_id"] = sid
		if err := c.Call(method, params, out); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	view := func() SessionView {
		t.Helper()
		var v SessionView
		call("session.view", map[string]any{}, &v)
		return v
	}
	call("session.attach", map[string]any{}, nil)

	// A window of two blocks, then a split of the second.
	var w1 struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
	}
	call("session.new_window", map[string]any{"layout": &LayoutNode{Split: &LayoutSplit{Direction: "horizontal", Ratio: 0.3, Before: shellBlock(), After: shellBlock()}}}, &w1)
	left, right := w1.BlockIDs[0], w1.BlockIDs[1]
	var sp struct {
		BlockIDs []string `json:"block_ids"`
	}
	call("session.new_split", map[string]any{"anchor_id": right, "direction": "vertical", "side": "after", "layout": shellBlock(), "focus": true}, &sp)
	below := sp.BlockIDs[0]
	v := view()
	w := v.Windows[0]
	if v.Active != w1.WindowID || w.Focus != below || w.Label != DefaultWindowLabel(w1.WindowID) {
		t.Fatalf("view %+v window %+v", v, w)
	}
	if got := w.Layout.Blocks(); !slices.Equal(got, []string{left, right, below}) {
		t.Fatalf("blocks %v", got)
	}
	if after := w.Layout.Split.After.Split; after == nil || after.Direction != "vertical" || w.Layout.Split.Ratio != 0.3 {
		t.Fatalf("layout %+v", w.Layout.Split)
	}

	// Focus, zoom and a border's move.
	call("session.focus_block", map[string]any{"block_id": left}, nil)
	call("session.zoom_block", map[string]any{}, nil)
	if w := view().Windows[0]; w.Focus != left || w.Zoom != left {
		t.Fatalf("focus %s zoom %s", w.Focus, w.Zoom)
	}
	call("session.focus_block", map[string]any{"block_id": below}, nil)
	call("session.resize_pane", map[string]any{"direction": "horizontal", "before_block_id": left, "after_block_id": below, "ratio": 0.6}, nil)
	if w := view().Windows[0]; w.Zoom != "" || w.Layout.Split.Ratio != 0.6 {
		t.Fatalf("zoom %s ratio %v after focusing another block and moving the border", w.Zoom, w.Layout.Split.Ratio)
	}

	// Closing the focused block gives the focus back to the one before,
	// and its neighbor takes its split's place.
	call("block.close", map[string]any{"block_id": below}, nil)
	w = view().Windows[0]
	if w.Focus != left || w.Layout.Split.After.Block == nil || w.Layout.Split.After.Block.ID != right {
		t.Fatalf("after close: focus %s layout %+v", w.Focus, w.Layout.Split)
	}

	// A second window, labeled and moved first; a shell that exits closes
	// its block, and the window with it.
	var w2 struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
	}
	call("session.new_window", map[string]any{"layout": shellBlock()}, &w2)
	call("session.set_window_label", map[string]any{"window_id": w2.WindowID, "label": " logs "}, nil)
	call("session.move_window", map[string]any{"window_id": w2.WindowID, "before_window_id": w1.WindowID}, nil)
	v = view()
	if v.Active != w2.WindowID || v.Windows[0].ID != w2.WindowID || v.Windows[0].Label != "logs" {
		t.Fatalf("second window %+v", v)
	}
	s := c.Stream(w2.BlockIDs[0], 80, 24)
	s.Write([]byte("exit\r\n"))
	deadline := time.Now().Add(5 * time.Second)
	for len(view().Windows) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the window of the shell that exited stays")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if v := view(); v.Active != w1.WindowID {
		t.Errorf("active %s, not the window left", v.Active)
	}

	// The events came wrapped as Rex's, each change's before its view's.
	var seen []string
	for len(events) > 0 {
		e := <-events
		var se SessionEvent
		if e.Type != "session_event" || json.Unmarshal(e.Payload, &se) != nil || se.SessionID != sid {
			t.Fatalf("event %s %s", e.Type, e.Payload)
		}
		seen = append(seen, se.Event.Type)
	}
	for _, want := range []string{"block_created", "window_created", "active_window_changed", "session_view_changed", "block_closed", "window_label_changed", "window_closed"} {
		if !slices.Contains(seen, want) {
			t.Errorf("no %s event in %v", want, seen)
		}
	}
	if seen[len(seen)-1] != "session_view_changed" {
		t.Errorf("last event %s", seen[len(seen)-1])
	}

	// Errors carry Rex's codes.
	for _, tc := range []struct {
		method string
		params map[string]any
		code   string
	}{
		{"session.view", map[string]any{"session_id": "session:nope"}, CodeSessionNotFound},
		{"session.view", map[string]any{"session_id": sid, "lable": "x"}, CodeBadRequest},
		{"block.close", map[string]any{"session_id": sid}, CodeBadRequest},
		{"session.focus_block", map[string]any{"session_id": sid, "block_id": "block:nope"}, CodeNotFound},
		{"session.new_window", map[string]any{"session_id": sid, "layout": &LayoutNode{Block: &LayoutBlock{Flavor: "com.example.nope"}}}, CodeNotFound},
		{"session.nope", map[string]any{}, CodeMethodNotFound},
	} {
		var ae *APIError
		if err := c.Call(tc.method, tc.params, nil); !errors.As(err, &ae) || ae.Code != tc.code {
			t.Errorf("%s %v: %v, not %s", tc.method, tc.params, err, tc.code)
		}
	}

	// A new server starts the session again, as it was, with new shells.
	before := view()
	c.Shutdown()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c, _ = serve(t, dir)
	defer c.Shutdown()
	after := view()
	if len(after.Windows) != 1 || !slices.Equal(after.Windows[0].Layout.Blocks(), before.Windows[0].Layout.Blocks()) ||
		after.Windows[0].Layout.Split.Ratio != 0.6 || after.Active != before.Active {
		t.Fatalf("after a restart %+v, before %+v", after, before)
	}
	if list, _ := c.List(); len(list) != 2 || list[0].Exited {
		t.Errorf("blocks after a restart %+v", list)
	}
}

// TestRelabel sets and restores the labels of a session and of a block,
// through Rex's methods, and checks that they and the label the server
// chose outlive a restart.
func TestRelabel(t *testing.T) {
	dir := testDir(t)
	c, done := serve(t, dir)
	events := make(chan Event, 256)
	c.OnEvent(func(e Event) { events <- e })
	var sess struct {
		ID string `json:"session_id"`
	}
	if err := c.Call("session.create", map[string]any{"label": "mine"}, &sess); err != nil {
		t.Fatal(err)
	}
	sid := sess.ID
	call := func(method string, params map[string]any) {
		t.Helper()
		params["session_id"] = sid
		if err := c.Call(method, params, nil); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	listed := func() string {
		t.Helper()
		var out struct {
			Sessions []struct {
				ID    string `json:"session_id"`
				Label string `json:"label"`
			} `json:"sessions"`
		}
		if err := c.Call("session.list", map[string]any{}, &out); err != nil || len(out.Sessions) != 1 {
			t.Fatalf("list %+v %v", out, err)
		}
		return out.Sessions[0].Label
	}
	view := func() SessionView {
		t.Helper()
		var v SessionView
		if err := c.Call("session.view", map[string]any{"session_id": sid}, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	// sent drains the events heard so far, by type.
	sent := func() (types []string) {
		t.Helper()
		time.Sleep(100 * time.Millisecond)
		for len(events) > 0 {
			var se SessionEvent
			json.Unmarshal((<-events).Payload, &se)
			types = append(types, se.Event.Type)
		}
		return types
	}
	call("session.attach", map[string]any{})

	// A new label is trimmed, shown in the list and the view, and sent as
	// its event and then the view's; the same label again sends nothing.
	call("session.set_label", map[string]any{"label": "  work "})
	if v := view(); listed() != "work" || v.Label != "work" {
		t.Fatalf("list %q view %q", listed(), v.Label)
	}
	if got := sent(); !slices.Equal(got, []string{"session_label_changed", "session_view_changed"}) {
		t.Errorf("events %v", got)
	}
	revision := view().Revision
	call("session.set_label", map[string]any{"label": "work"})
	if got := sent(); len(got) != 0 || view().Revision != revision {
		t.Errorf("setting the same label sent %v, revision %d to %d", got, revision, view().Revision)
	}

	// An empty label restores the label the server chose, though the
	// session was created with one of its own; so does a missing one.
	call("session.set_label", map[string]any{"label": " "})
	chosen := listed()
	if chosen == "" || chosen == "mine" || chosen == "work" {
		t.Fatalf("restored label %q", chosen)
	}
	call("session.set_label", map[string]any{"label": "again"})
	call("session.set_label", map[string]any{})
	if listed() != chosen {
		t.Errorf("a missing label restored %q, not %q", listed(), chosen)
	}

	// A block's label is the default made from its ID until set.
	var w struct {
		BlockIDs []string `json:"block_ids"`
	}
	if err := c.Call("session.new_window", map[string]any{"session_id": sid, "layout": &LayoutNode{Split: &LayoutSplit{Direction: "horizontal", Before: shellBlock(), After: shellBlock()}}}, &w); err != nil {
		t.Fatal(err)
	}
	first, second := w.BlockIDs[0], w.BlockIDs[1]
	labels := func() (string, string) {
		t.Helper()
		l := view().Windows[0].Layout.Split
		return l.Before.Block.Label, l.After.Block.Label
	}
	if a, b := labels(); a != DefaultBlockLabel(first) || b != DefaultBlockLabel(second) || !strings.HasPrefix(a, "Block ") {
		t.Fatalf("new blocks' labels %q %q", a, b)
	}
	sent()
	call("session.set_block_label", map[string]any{"block_id": first, "label": " build "})
	if a, b := labels(); a != "build" || b != DefaultBlockLabel(second) {
		t.Fatalf("labels %q %q", a, b)
	}
	if got := sent(); !slices.Equal(got, []string{"session_view_changed"}) {
		t.Errorf("events %v", got)
	}
	call("session.set_block_label", map[string]any{"block_id": second, "label": "logs"})
	call("session.set_block_label", map[string]any{"block_id": second})
	if _, b := labels(); b != DefaultBlockLabel(second) {
		t.Errorf("a missing label restored %q", b)
	}

	// Errors carry Rex's codes.
	split := view().Windows[0].Layout.Split.ID
	for _, tc := range []struct {
		method string
		params map[string]any
		code   string
	}{
		{"session.set_label", map[string]any{"session_id": "session:nope", "label": "x"}, CodeSessionNotFound},
		{"session.set_label", map[string]any{"session_id": sid, "label": strings.Repeat("x", 129)}, CodeBadRequest},
		{"session.set_label", map[string]any{"session_id": sid, "label": "a\tb"}, CodeBadRequest},
		{"session.set_label", map[string]any{"session_id": sid, "lable": "x"}, CodeBadRequest},
		{"session.set_block_label", map[string]any{"session_id": "session:nope", "block_id": first}, CodeSessionNotFound},
		{"session.set_block_label", map[string]any{"session_id": sid, "label": "x"}, CodeBadRequest},
		{"session.set_block_label", map[string]any{"session_id": sid, "block_id": first, "label": strings.Repeat("x", 129)}, CodeBadRequest},
		{"session.set_block_label", map[string]any{"session_id": sid, "block_id": first, "label": "a\nb"}, CodeBadRequest},
		{"session.set_block_label", map[string]any{"session_id": sid, "block_id": first, "lable": "x"}, CodeBadRequest},
		{"session.set_block_label", map[string]any{"session_id": sid, "block_id": "block:nope", "label": "x"}, CodeNotFound},
		{"session.set_block_label", map[string]any{"session_id": sid, "block_id": split, "label": "x"}, CodeNotFound},
	} {
		var ae *APIError
		if err := c.Call(tc.method, tc.params, nil); !errors.As(err, &ae) || ae.Code != tc.code {
			t.Errorf("%s %v: %v, not %s", tc.method, tc.params, err, tc.code)
		}
	}
	if a, _ := labels(); a != "build" || listed() != chosen {
		t.Errorf("a failed call changed a label: %q, %q", a, listed())
	}

	// The labels and the chosen one outlive a restart.
	call("session.set_label", map[string]any{"label": "kept"})
	c.Shutdown()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c, _ = serve(t, dir)
	defer c.Shutdown()
	if a, _ := labels(); a != "build" || listed() != "kept" {
		t.Fatalf("after a restart: block %q session %q", a, listed())
	}
	call("session.set_label", map[string]any{})
	if listed() != chosen {
		t.Errorf("after a restart an empty label restored %q, not %q", listed(), chosen)
	}
}
