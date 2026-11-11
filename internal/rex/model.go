//go:build darwin || linux || windows

package rex

import (
	"bytes"
	"encoding/json"
	"log"
	mrand "math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
)

// rsession is a session of the server: its view, and per window the
// blocks by when they had the focus last, the latest last, so that
// closing a block gives the focus back to the one before.
type rsession struct {
	view   SessionView
	recent map[string][]string
}

// ctrl is a control connection: its lines go out in order through out,
// and it hears the events of the sessions it attached to.
type ctrl struct {
	conn     net.Conn
	out      chan []byte
	sessions map[string]bool
}

// call runs one of Rex's methods.
func (s *Server) call(c *ctrl, method string, params json.RawMessage) (any, error) {
	switch method {
	case "session.create":
		var p struct {
			Label string `json:"label"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		label := strings.TrimSpace(p.Label)
		if label == "" {
			label = s.newSessionLabel()
		}
		rs := &rsession{view: SessionView{ID: newID("session"), Label: label, Windows: []*WindowView{}}, recent: map[string][]string{}}
		s.rsessions = append(s.rsessions, rs)
		return map[string]string{"session_id": rs.view.ID, "label": label}, nil

	case "session.list":
		s.mu.Lock()
		defer s.mu.Unlock()
		type item struct {
			ID    string `json:"session_id"`
			Label string `json:"label"`
		}
		out := []item{}
		for _, rs := range s.rsessions {
			out = append(out, item{rs.view.ID, rs.view.Label})
		}
		return map[string]any{"sessions": out}, nil

	case "session.attach":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.rsession(p.SessionID); err != nil {
			return nil, err
		}
		c.sessions[p.SessionID] = true
		return nil, nil

	case "session.view":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rs, err := s.rsession(p.SessionID)
		if err != nil {
			return nil, err
		}
		return viewOf(rs), nil

	case "session.new_window":
		return s.newWindow(params)
	case "session.new_split":
		return s.newSplit(params)

	case "session.close_window":
		var p struct {
			SessionID string `json:"session_id"`
			WindowID  string `json:"window_id"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		rs, err := s.rsession(p.SessionID)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		w, err := rs.windowOrActive(p.WindowID)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		closed := s.removeWindow(rs, w)
		s.bump(rs)
		s.mu.Unlock()
		kill(closed)
		return nil, nil

	case "block.close":
		var p struct {
			SessionID string `json:"session_id"`
			BlockID   string `json:"block_id"`
			WindowID  string `json:"window_id"`
			KeepLast  bool   `json:"keep_last"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if (p.BlockID == "") == (p.WindowID == "") {
			return nil, apiErr(CodeBadRequest, "send exactly one of block_id and window_id")
		}
		s.mu.Lock()
		rs, err := s.rsession(p.SessionID)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		id := p.BlockID
		var w *WindowView
		if id != "" {
			if w = rs.windowOf(id); w == nil {
				s.mu.Unlock()
				return nil, apiErr(CodeNotFound, "block %s is not in the session", id)
			}
		} else {
			if w = rs.window(p.WindowID); w == nil || w.Focus == "" {
				s.mu.Unlock()
				return nil, apiErr(CodeNotFound, "window %s is not in the session, or has no focused block", p.WindowID)
			}
			id = w.Focus
		}
		if p.KeepLast && w.Layout.Block != nil {
			s.mu.Unlock()
			return nil, apiErr(CodeBadRequest, "block %s is the only one left in its window", id)
		}
		b := s.removeBlock(rs, w, id)
		s.bump(rs)
		s.mu.Unlock()
		kill([]*block{b})
		return nil, nil

	case "session.focus_block":
		var p struct {
			SessionID string `json:"session_id"`
			BlockID   string `json:"block_id"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rs, err := s.rsession(p.SessionID)
		if err != nil {
			return nil, err
		}
		w := rs.windowOf(p.BlockID)
		if w == nil {
			return nil, apiErr(CodeNotFound, "block %s is not in a window", p.BlockID)
		}
		activated := s.activate(rs, w)
		if rs.focus(w, p.BlockID) || activated {
			s.bump(rs)
		}
		return nil, nil

	case "session.focus_window":
		var p struct {
			SessionID string `json:"session_id"`
			WindowID  string `json:"window_id"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rs, err := s.rsession(p.SessionID)
		if err != nil {
			return nil, err
		}
		w := rs.window(p.WindowID)
		if w == nil {
			return nil, apiErr(CodeNotFound, "window %s is not in the session", p.WindowID)
		}
		if s.activate(rs, w) {
			s.bump(rs)
		}
		return nil, nil

	case "session.zoom_block":
		var p struct {
			SessionID string `json:"session_id"`
			BlockID   string `json:"block_id"`
			WindowID  string `json:"window_id"`
			Zoom      *bool  `json:"zoom"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if p.BlockID != "" && p.WindowID != "" {
			return nil, apiErr(CodeBadRequest, "send block_id or window_id, not both")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rs, err := s.rsession(p.SessionID)
		if err != nil {
			return nil, err
		}
		var w *WindowView
		id := p.BlockID
		if id != "" {
			if w = rs.windowOf(id); w == nil {
				return nil, apiErr(CodeNotFound, "block %s is not in a window", id)
			}
			s.activate(rs, w)
			rs.focus(w, id)
		} else {
			if w, err = rs.windowOrActive(p.WindowID); err != nil {
				return nil, err
			}
			id = w.Focus
		}
		zoom := w.Zoom != id
		if p.Zoom != nil {
			zoom = *p.Zoom
		}
		if zoom {
			w.Zoom = id
		} else {
			w.Zoom = ""
		}
		s.bump(rs)
		return nil, nil

	case "session.resize_pane":
		var p struct {
			SessionID string  `json:"session_id"`
			Direction string  `json:"direction"`
			Before    string  `json:"before_block_id"`
			After     string  `json:"after_block_id"`
			Ratio     float64 `json:"ratio"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if p.Before == p.After || p.Ratio <= 0 || p.Ratio >= 1 || !validDirection(p.Direction) {
			return nil, apiErr(CodeBadRequest, "resize_pane needs two blocks, a direction, and a ratio between 0 and 1")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rs, err := s.rsession(p.SessionID)
		if err != nil {
			return nil, err
		}
		wb, wa := rs.windowOf(p.Before), rs.windowOf(p.After)
		if wb == nil || wa == nil {
			return nil, apiErr(CodeNotFound, "both blocks must be in a window")
		}
		sp := border(wb.Layout, p.Direction, p.Before, p.After)
		if wb != wa || sp == nil {
			return nil, apiErr(CodeBadRequest, "no %s split has %s before it and %s after it", p.Direction, p.Before, p.After)
		}
		sp.Ratio = p.Ratio
		s.bump(rs)
		return nil, nil

	case "session.set_window_label":
		var p struct {
			SessionID string `json:"session_id"`
			WindowID  string `json:"window_id"`
			Label     string `json:"label"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		label := strings.TrimSpace(p.Label)
		if err := checkLabel(label); err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rs, err := s.rsession(p.SessionID)
		if err != nil {
			return nil, err
		}
		w := rs.window(p.WindowID)
		if w == nil {
			return nil, apiErr(CodeNotFound, "window %s is not in the session", p.WindowID)
		}
		if label == DefaultWindowLabel(w.ID) {
			label = ""
		}
		if w.Label != label {
			w.Label = label
			s.emit(rs, "window_label_changed", map[string]string{"window_id": w.ID, "label": labelOf(w)})
			s.bump(rs)
		}
		return nil, nil

	case "session.move_window":
		var p struct {
			SessionID string `json:"session_id"`
			WindowID  string `json:"window_id"`
			BeforeID  string `json:"before_window_id"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if p.WindowID == p.BeforeID {
			return nil, apiErr(CodeBadRequest, "a window cannot move before itself")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rs, err := s.rsession(p.SessionID)
		if err != nil {
			return nil, err
		}
		w := rs.window(p.WindowID)
		if w == nil || p.BeforeID != "" && rs.window(p.BeforeID) == nil {
			return nil, apiErr(CodeNotFound, "the windows must be in the session")
		}
		ws := slices.DeleteFunc(slices.Clone(rs.view.Windows), func(o *WindowView) bool { return o == w })
		at := len(ws)
		if p.BeforeID != "" {
			at = slices.IndexFunc(ws, func(o *WindowView) bool { return o.ID == p.BeforeID })
		}
		ws = slices.Insert(ws, at, w)
		if !slices.Equal(ws, rs.view.Windows) {
			rs.view.Windows = ws
			s.bump(rs)
		}
		return nil, nil
	}
	return nil, apiErr(CodeMethodNotFound, "no method %q", method)
}

func (s *Server) newWindow(params json.RawMessage) (any, error) {
	var p struct {
		SessionID string      `json:"session_id"`
		Label     string      `json:"window_label"`
		Layout    *LayoutNode `json:"layout"`
		Focus     *bool       `json:"focus"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	label := strings.TrimSpace(p.Label)
	if err := checkLabel(label); err != nil {
		return nil, err
	}
	if err := checkLayout(p.Layout); err != nil {
		return nil, err
	}
	s.mu.Lock()
	_, err := s.rsession(p.SessionID)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	layout, started, err := s.start(p.Layout)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.rsession(p.SessionID)
	if err != nil {
		kill(started)
		return nil, err
	}
	ids := layout.Blocks()
	w := &WindowView{ID: newID("window"), Label: label, Layout: layout}
	rs.view.Windows = append(rs.view.Windows, w)
	rs.focus(w, ids[0])
	s.adopt(rs, started)
	s.emit(rs, "window_created", map[string]string{"window_id": w.ID, "label": labelOf(w)})
	if p.Focus == nil || *p.Focus || rs.view.Active == "" {
		s.activate(rs, w)
	}
	s.bump(rs)
	return map[string]any{"window_id": w.ID, "block_ids": ids}, nil
}

func (s *Server) newSplit(params json.RawMessage) (any, error) {
	var p struct {
		SessionID string      `json:"session_id"`
		AnchorID  string      `json:"anchor_id"`
		WindowID  string      `json:"window_id"`
		Direction string      `json:"direction"`
		Side      string      `json:"side"`
		Ratio     float64     `json:"ratio"`
		Layout    *LayoutNode `json:"layout"`
		Focus     bool        `json:"focus"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.AnchorID != "" && p.WindowID != "" {
		return nil, apiErr(CodeBadRequest, "send anchor_id or window_id, not both")
	}
	if !validDirection(p.Direction) || p.Side != "before" && p.Side != "after" || p.Ratio < 0 || p.Ratio >= 1 {
		return nil, apiErr(CodeBadRequest, "new_split needs a direction, a side, and a ratio between 0 and 1")
	}
	if err := checkLayout(p.Layout); err != nil {
		return nil, err
	}
	// The anchor is chosen before the blocks start, so that a change of
	// focus meanwhile does not move the split.
	s.mu.Lock()
	rs, err := s.rsession(p.SessionID)
	anchor := p.AnchorID
	if err == nil && anchor == "" {
		var w *WindowView
		if w, err = rs.windowOrActive(p.WindowID); err == nil {
			anchor = w.Focus
		}
	}
	if err == nil && rs.windowOf(anchor) == nil {
		err = apiErr(CodeNotFound, "%s is not in a window", anchor)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	layout, started, err := s.start(p.Layout)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err = s.rsession(p.SessionID)
	var w *WindowView
	if err == nil {
		if w = rs.windowOf(anchor); w == nil {
			err = apiErr(CodeNotFound, "%s is not in a window", anchor)
		}
	}
	if err != nil {
		kill(started)
		return nil, err
	}
	n, _ := find(w.Layout, anchor)
	old := *n
	sp := &LayoutSplit{ID: newID("split"), Direction: p.Direction, Ratio: cmp(p.Ratio, 0.5), Before: &old, After: layout}
	if p.Side == "before" {
		sp.Before, sp.After = layout, &old
	}
	*n = LayoutNode{Split: sp}
	w.Zoom = ""
	s.adopt(rs, started)
	ids := layout.Blocks()
	if p.Focus {
		s.activate(rs, w)
		rs.focus(w, ids[0])
	}
	s.bump(rs)
	return map[string]any{"block_ids": ids}, nil
}

// start starts the blocks of a layout a request gives, returning the
// layout with their IDs; if one fails, those it started end.
func (s *Server) start(req *LayoutNode) (*LayoutNode, []*block, error) {
	var started []*block
	var walk func(n *LayoutNode) (*LayoutNode, error)
	walk = func(n *LayoutNode) (*LayoutNode, error) {
		if n.Split != nil {
			before, err := walk(n.Split.Before)
			if err != nil {
				return nil, err
			}
			after, err := walk(n.Split.After)
			if err != nil {
				return nil, err
			}
			return &LayoutNode{Split: &LayoutSplit{ID: newID("split"), Direction: n.Split.Direction, Ratio: cmp(n.Split.Ratio, 0.5), Before: before, After: after}}, nil
		}
		o := BlockOptions{}
		if n.Block.Options != nil {
			o = *n.Block.Options
		}
		b, err := newBlock(newID("block"), o)
		if err != nil {
			return nil, err
		}
		started = append(started, b)
		return &LayoutNode{Block: &LayoutBlock{ID: b.id, Label: n.Block.Label}}, nil
	}
	layout, err := walk(req)
	if err != nil {
		kill(started)
		return nil, nil, err
	}
	return layout, started, nil
}

// adopt makes started blocks the server's.
func (s *Server) adopt(rs *rsession, started []*block) {
	for _, b := range started {
		s.blocks[b.id] = b
		s.blockOrder = append(s.blockOrder, b.id)
		s.emit(rs, "block_created", map[string]string{"block_id": b.id})
		go s.watch(b)
	}
}

// watch closes a block once its program exits, unless the server is
// quitting, which keeps the blocks to start again.
func (s *Server) watch(b *block) {
	<-b.done
	s.mu.Lock()
	if s.quitting || s.blocks[b.id] != b {
		s.mu.Unlock()
		return
	}
	for _, rs := range s.rsessions {
		if w := rs.windowOf(b.id); w != nil {
			s.removeBlock(rs, w, b.id)
			s.bump(rs)
			break
		}
	}
	s.dropBlock(b.id)
	s.mu.Unlock()
	s.save()
}

// removeBlock takes a block out of its window, the other side of its
// split taking the split's place, or closes the window it was alone in.
func (s *Server) removeBlock(rs *rsession, w *WindowView, id string) *block {
	n, parent := find(w.Layout, id)
	if parent == nil {
		if bs := s.removeWindow(rs, w); len(bs) > 0 {
			return bs[0]
		}
		return nil
	}
	other := parent.Split.Before
	if other == n {
		other = parent.Split.After
	}
	*parent = *other
	if w.Zoom == id {
		w.Zoom = ""
	}
	rs.recent[w.ID] = slices.DeleteFunc(rs.recent[w.ID], func(b string) bool { return b == id })
	if w.Focus == id {
		w.Focus = ""
		if r := rs.recent[w.ID]; len(r) > 0 {
			rs.focus(w, r[len(r)-1])
		} else {
			rs.focus(w, w.Layout.Blocks()[0])
		}
	}
	s.emit(rs, "block_closed", map[string]string{"block_id": id})
	return s.dropBlock(id)
}

// removeWindow closes a window, returning its blocks; the window after
// it becomes the active one, or the one before when it was last.
func (s *Server) removeWindow(rs *rsession, w *WindowView) []*block {
	var out []*block
	for _, id := range w.Layout.Blocks() {
		s.emit(rs, "block_closed", map[string]string{"block_id": id})
		if b := s.dropBlock(id); b != nil {
			out = append(out, b)
		}
	}
	i := slices.Index(rs.view.Windows, w)
	rs.view.Windows = slices.Delete(rs.view.Windows, i, i+1)
	delete(rs.recent, w.ID)
	s.emit(rs, "window_closed", map[string]string{"window_id": w.ID})
	if rs.view.Active == w.ID {
		rs.view.Active = ""
		if len(rs.view.Windows) > 0 {
			s.activate(rs, rs.view.Windows[min(i, len(rs.view.Windows)-1)])
		}
	}
	return out
}

// dropBlock forgets a block, returning it.
func (s *Server) dropBlock(id string) *block {
	b := s.blocks[id]
	delete(s.blocks, id)
	s.blockOrder = slices.DeleteFunc(s.blockOrder, func(o string) bool { return o == id })
	if len(s.blocks) == 0 {
		s.idleFrom = time.Now()
	}
	return b
}

// kill ends blocks the model let go of, aside: a program that ignores
// the hangup holds its block a second.
func kill(bs []*block) {
	for _, b := range bs {
		if b != nil {
			go b.kill()
		}
	}
}

// activate makes a window the active one, reporting whether it was not.
func (s *Server) activate(rs *rsession, w *WindowView) bool {
	if rs.view.Active == w.ID {
		return false
	}
	rs.view.Active = w.ID
	s.emit(rs, "active_window_changed", map[string]string{"window_id": w.ID})
	return true
}

// focus focuses a block of a window, ending a zoom of another, and
// reports whether the focus moved.
func (rs *rsession) focus(w *WindowView, id string) bool {
	if w.Focus == id {
		return false
	}
	w.Focus = id
	if w.Zoom != "" && w.Zoom != id {
		w.Zoom = ""
	}
	r := slices.DeleteFunc(rs.recent[w.ID], func(b string) bool { return b == id })
	rs.recent[w.ID] = append(r, id)
	return true
}

// bump raises the session's revision and tells the clients the view
// changed.
func (s *Server) bump(rs *rsession) {
	rs.view.Revision++
	s.emit(rs, "session_view_changed", map[string]int{"revision": rs.view.Revision})
}

// emit sends a session's event to the connections attached to it. A
// connection too far behind to take it is dropped.
func (s *Server) emit(rs *rsession, typ string, payload any) {
	p, _ := json.Marshal(payload)
	se, _ := json.Marshal(SessionEvent{SessionID: rs.view.ID, Event: Event{Type: typ, Payload: p}})
	line, _ := json.Marshal(Response{Event: &Event{Type: "session_event", Payload: se}})
	line = append(line, '\n')
	for c := range s.ctrls {
		if !c.sessions[rs.view.ID] {
			continue
		}
		select {
		case c.out <- line:
		default:
			log.Print("rex: a client fell behind on events; dropping it")
			c.conn.Close()
			delete(s.ctrls, c)
		}
	}
}

func (s *Server) rsession(id string) (*rsession, error) {
	for _, rs := range s.rsessions {
		if rs.view.ID == id {
			return rs, nil
		}
	}
	return nil, apiErr(CodeSessionNotFound, "no session %q", id)
}

func (rs *rsession) window(id string) *WindowView {
	for _, w := range rs.view.Windows {
		if w.ID == id {
			return w
		}
	}
	return nil
}

// windowOrActive returns the window of an ID, or the active one when the
// ID is empty.
func (rs *rsession) windowOrActive(id string) (*WindowView, error) {
	if id == "" {
		if w := rs.window(rs.view.Active); w != nil {
			return w, nil
		}
		return nil, apiErr(CodeConflict, "the session has no windows")
	}
	if w := rs.window(id); w != nil {
		return w, nil
	}
	return nil, apiErr(CodeNotFound, "window %s is not in the session", id)
}

// windowOf returns the window whose layout holds a block or a split.
func (rs *rsession) windowOf(id string) *WindowView {
	if id == "" {
		return nil
	}
	for _, w := range rs.view.Windows {
		if n, _ := find(w.Layout, id); n != nil {
			return w
		}
	}
	return nil
}

// find returns the node of a block or a split, and its parent.
func find(n *LayoutNode, id string) (node, parent *LayoutNode) {
	if n.Block != nil {
		if n.Block.ID == id {
			return n, nil
		}
		return nil, nil
	}
	if n.Split.ID == id {
		return n, nil
	}
	for _, child := range []*LayoutNode{n.Split.Before, n.Split.After} {
		if node, parent = find(child, id); node != nil {
			if parent == nil {
				parent = n
			}
			return node, parent
		}
	}
	return nil, nil
}

// border returns the innermost split of a direction with one block
// before it and the other after it.
func border(n *LayoutNode, direction, before, after string) *LayoutSplit {
	if n.Block != nil {
		return nil
	}
	if sp := border(n.Split.Before, direction, before, after); sp != nil {
		return sp
	}
	if sp := border(n.Split.After, direction, before, after); sp != nil {
		return sp
	}
	if n.Split.Direction == direction && slices.Contains(n.Split.Before.Blocks(), before) && slices.Contains(n.Split.After.Blocks(), after) {
		return n.Split
	}
	return nil
}

// viewOf returns a copy of a session's view, its windows' labels filled
// in.
func viewOf(rs *rsession) SessionView {
	b, _ := json.Marshal(rs.view)
	var v SessionView
	json.Unmarshal(b, &v)
	for _, w := range v.Windows {
		w.Label = labelOf(w)
	}
	return v
}

func labelOf(w *WindowView) string {
	if w.Label == "" {
		return DefaultWindowLabel(w.ID)
	}
	return w.Label
}

// decode decodes a method's arguments, refusing those it does not know,
// as Rex does.
func decode(params json.RawMessage, v any) error {
	if len(params) == 0 {
		params = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(params))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return apiErr(CodeBadRequest, "input: %v", err)
	}
	return nil
}

func checkLabel(label string) error {
	if len([]rune(label)) > 128 || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return apiErr(CodeBadRequest, "a label has at most 128 characters and no control characters")
	}
	return nil
}

// checkLayout checks a layout a request gives before any block starts.
func checkLayout(n *LayoutNode) error {
	switch {
	case n == nil || (n.Block == nil) == (n.Split == nil):
		return apiErr(CodeBadRequest, "a layout node holds either block or split")
	case n.Block != nil:
		if n.Block.Flavor == "" {
			return apiErr(CodeBadRequest, "a block needs a flavor")
		}
		if n.Block.Flavor != ShellFlavor {
			return apiErr(CodeNotFound, "no creator provides %q", n.Block.Flavor)
		}
		return nil
	}
	sp := n.Split
	if !validDirection(sp.Direction) || sp.Ratio < 0 || sp.Ratio >= 1 {
		return apiErr(CodeBadRequest, "a split needs a direction and a ratio between 0 and 1")
	}
	if err := checkLayout(sp.Before); err != nil {
		return err
	}
	return checkLayout(sp.After)
}

func validDirection(d string) bool { return d == "horizontal" || d == "vertical" }

// cmp returns v, or def when v is zero.
func cmp(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

// newSessionLabel picks a label no session has, as Rex's quiet-meadow.
func (s *Server) newSessionLabel() string {
	adj := []string{"quiet", "bright", "calm", "swift", "amber", "misty", "golden", "silver", "gentle", "bold"}
	noun := []string{"meadow", "river", "harbor", "forest", "canyon", "summit", "valley", "island", "garden", "comet"}
	for {
		l := adj[mrand.IntN(len(adj))] + "-" + noun[mrand.IntN(len(noun))]
		if !slices.ContainsFunc(s.rsessions, func(rs *rsession) bool { return rs.view.Label == l }) {
			return l
		}
	}
}

// savedState is what the server keeps on disk, to start its sessions
// again: their views, and how to start each block anew.
type savedState struct {
	Sessions []savedSession `json:"sessions"`
}

type savedSession struct {
	View   SessionView             `json:"view"`
	Recent map[string][]string     `json:"recent,omitempty"`
	Blocks map[string]BlockOptions `json:"blocks"`
}

func statePath() string { return filepath.Join(Dir(), "state.json") }

// save writes the sessions to disk, each block with the directory it
// works in now.
func (s *Server) save() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	st := savedState{Sessions: []savedSession{}}
	blocks := map[string]*block{}
	for _, rs := range s.rsessions {
		ss := savedSession{View: rs.view, Recent: rs.recent, Blocks: map[string]BlockOptions{}}
		for _, w := range rs.view.Windows {
			for _, id := range w.Layout.Blocks() {
				blocks[id] = s.blocks[id]
			}
		}
		st.Sessions = append(st.Sessions, ss)
	}
	b, err := json.Marshal(st)
	s.mu.Unlock()
	if err != nil {
		log.Printf("rex: saving the sessions: %v", err)
		return
	}
	// The blocks' directories, found without holding the server.
	json.Unmarshal(b, &st)
	for i, ss := range st.Sessions {
		for _, w := range ss.View.Windows {
			for _, id := range w.Layout.Blocks() {
				blk := blocks[id]
				if blk == nil {
					continue
				}
				o := blk.opts
				in := blk.info()
				o.Dir, o.Cols, o.Rows = in.Dir, in.Cols, in.Rows
				if o.Dir == "" {
					o.Dir = blk.opts.Dir
				}
				st.Sessions[i].Blocks[id] = o
			}
		}
	}
	b, _ = json.Marshal(st)
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("rex: saving the sessions: %v", err)
		return
	}
	os.Rename(tmp, statePath())
}

// load starts the saved sessions again, each block anew in the directory
// it worked in; a block that cannot start leaves its window's layout.
func (s *Server) load() {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return
	}
	var st savedState
	if err := json.Unmarshal(b, &st); err != nil {
		log.Printf("rex: the saved sessions: %v", err)
		return
	}
	for _, ss := range st.Sessions {
		rs := &rsession{view: ss.View, recent: ss.Recent}
		if rs.recent == nil {
			rs.recent = map[string][]string{}
		}
		var started []*block
		for _, w := range slices.Clone(rs.view.Windows) {
			for _, id := range w.Layout.Blocks() {
				o := ss.Blocks[id]
				if st, err := os.Stat(o.Dir); err != nil || !st.IsDir() {
					o.Dir = ""
				}
				blk, err := newBlock(id, o)
				if err == nil {
					started = append(started, blk)
					continue
				}
				log.Printf("rex: starting block %s again: %v", id, err)
				s.removeBlock(rs, w, id)
			}
		}
		if rs.view.Windows == nil {
			rs.view.Windows = []*WindowView{}
		}
		s.rsessions = append(s.rsessions, rs)
		s.adopt(rs, started)
	}
}
