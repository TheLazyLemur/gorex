package rex

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// The names of Rex's terminal blocks: the creator, and the flavor of a
// block that runs a shell, or a command in its place.
const (
	TerminalCreator = "com.superlogical.terminal"
	ShellFlavor     = "com.superlogical.terminal.shell"
)

// LayoutNode is a node of a window's layout, as Rex has it: a block, or a
// split of two nodes. Requests create blocks from a flavor; a view names
// the blocks and splits by their IDs.
type LayoutNode struct {
	Block *LayoutBlock `json:"block,omitempty"`
	Split *LayoutSplit `json:"split,omitempty"`
}

type LayoutBlock struct {
	ID      string        `json:"block_id,omitempty"`
	Flavor  string        `json:"flavor,omitempty"`
	Options *BlockOptions `json:"options,omitempty"`
	Label   string        `json:"label,omitempty"`
}

// LayoutSplit divides its space between Before and After: side by side
// when Direction is "horizontal", one above the other when "vertical".
// Ratio is Before's share.
type LayoutSplit struct {
	ID        string      `json:"split_id,omitempty"`
	Direction string      `json:"direction"`
	Ratio     float64     `json:"ratio,omitempty"`
	Before    *LayoutNode `json:"before"`
	After     *LayoutNode `json:"after"`
}

// SessionView is a session's windows and their layouts: session.view's
// answer. Rex does not document the shape of its answer; this one has the
// names its arguments and events use.
type SessionView struct {
	ID       string        `json:"session_id"`
	Label    string        `json:"label"`
	Revision int           `json:"revision"`
	Active   string        `json:"active_window_id,omitempty"`
	Windows  []*WindowView `json:"windows"`
}

type WindowView struct {
	ID     string      `json:"window_id"`
	Label  string      `json:"label"`
	Layout *LayoutNode `json:"layout"`
	Focus  string      `json:"focused_block_id,omitempty"`
	Zoom   string      `json:"zoomed_block_id,omitempty"`
}

// Event is one of Rex's events, as "rex events --json" prints it: a
// session's event comes wrapped in a "session_event", whose payload
// names the session and holds the event.
type Event struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// SessionEvent is the payload of a "session_event".
type SessionEvent struct {
	SessionID string `json:"session_id"`
	Event     Event  `json:"event"`
}

// Rex's error codes.
const (
	CodeBadRequest      = "bad_request"
	CodeNotFound        = "not_found"
	CodeSessionNotFound = "session_not_found"
	CodeConflict        = "conflict"
	CodeMethodNotFound  = "method_not_found"
)

// APIError is a method's failure, with Rex's code for it.
type APIError struct {
	Code string
	Msg  string
}

func (e *APIError) Error() string { return e.Msg }

func apiErr(code, format string, args ...any) error {
	return &APIError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// newID returns a new ID of a kind, as Rex's: "block:" and a UUID of
// version 7, ordered by time.
func newID(kind string) string {
	var b [16]byte
	rand.Read(b[:])
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(time.Now().UnixMilli()))
	copy(b[:6], ms[2:])
	b[6] = 0x70 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	h := hex.EncodeToString(b[:])
	return kind + ":" + h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// DefaultWindowLabel is a window's label when it has none of its own, as
// Rex makes it from the window's ID: "Window 89abcdef".
func DefaultWindowLabel(id string) string {
	if len(id) < 8 {
		return "Window"
	}
	return "Window " + id[len(id)-8:]
}

// Blocks returns the IDs of a layout's blocks in order, left to right and
// top to bottom.
func (n *LayoutNode) Blocks() []string {
	switch {
	case n == nil:
		return nil
	case n.Block != nil:
		return []string{n.Block.ID}
	}
	return append(n.Split.Before.Blocks(), n.Split.After.Blocks()...)
}
