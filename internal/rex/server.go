//go:build darwin || linux || windows

package rex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Server owns the sessions, and the blocks of their windows.
type Server struct {
	mu        sync.Mutex
	rsessions []*rsession
	// blocks are the blocks by ID, and blockOrder their IDs as they were
	// made.
	blocks     map[string]*block
	blockOrder []string
	ctrls      map[*ctrl]struct{}
	// quitting keeps the blocks in their windows as the server ends them,
	// to start them again next time.
	quitting bool
	saveMu   sync.Mutex
	started  time.Time
	host     HostInfo
	hostDone chan struct{}
	ln       net.Listener
	exe      string
	exeTime  time.Time
	controls int
	idleFrom time.Time
	quit     chan struct{}
	quitOnce sync.Once
}

// idleTimeout is how long a server with no blocks and no app waits
// before it exits.
const idleTimeout = 10 * time.Minute

// Serve runs the server until it is shut down: it returns an error at
// once when another server already runs.
func Serve() error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := lockFile(lock); err != nil {
		return errors.New("rex: a server already runs")
	}
	defer lock.Close()
	sock := SocketPath()
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	os.Chmod(sock, 0o600)
	s := &Server{
		blocks: map[string]*block{}, ctrls: map[*ctrl]struct{}{}, started: time.Now(), ln: ln,
		hostDone: make(chan struct{}), idleFrom: time.Now(), quit: make(chan struct{}),
	}
	// The app's layout of before the server kept sessions.
	os.Remove(filepath.Join(dir, "layout.json"))
	s.load()
	s.exe, s.exeTime = Executable()
	go func() {
		s.host = hostInfo()
		close(s.hostDone)
	}()
	go s.watchIdle()
	go func() {
		<-s.quit
		ln.Close()
	}()
	log.Printf("rex: serving on %s (pid %d)", sock, os.Getpid())
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.quit:
				s.save()
				s.mu.Lock()
				s.quitting = true
				all := make([]*block, 0, len(s.blocks))
				for _, b := range s.blocks {
					all = append(all, b)
				}
				s.mu.Unlock()
				for _, b := range all {
					b.kill()
				}
				os.Remove(sock)
				return nil
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) watchIdle() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
		}
		s.mu.Lock()
		idle := len(s.blocks) == 0 && s.controls == 0 && time.Since(s.idleFrom) > idleTimeout
		s.mu.Unlock()
		if idle {
			log.Print("rex: idle, exiting")
			s.shutdown()
			return
		}
	}
}

func (s *Server) shutdown() { s.quitOnce.Do(func() { close(s.quit) }) }

func (s *Server) handle(conn net.Conn) {
	r := bufio.NewReaderSize(conn, 64<<10)
	first, err := r.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return
	}
	var a Attach
	if json.Unmarshal(first, &a) == nil && a.Op == "attach" {
		s.mu.Lock()
		b := s.blocks[a.SID]
		s.mu.Unlock()
		if b == nil {
			conn.Write([]byte(`{"error":"no such block"}` + "\n"))
			conn.Close()
			return
		}
		conn.Write([]byte(`{"ok":true}` + "\n"))
		// What the reader buffered past the first line is input.
		if n := r.Buffered(); n > 0 {
			p, _ := r.Peek(n)
			b.input(p)
		}
		b.attach(conn, a.Cols, a.Rows)
		return
	}
	s.control(conn, r, first)
}

// control serves a control connection: a request per line, answered in
// order with the events of the sessions it attached to.
func (s *Server) control(conn net.Conn, r *bufio.Reader, first []byte) {
	c := &ctrl{conn: conn, out: make(chan []byte, 4096), sessions: map[string]bool{}}
	s.mu.Lock()
	s.controls++
	s.ctrls[c] = struct{}{}
	s.mu.Unlock()
	written := make(chan struct{})
	go func() {
		defer close(written)
		for line := range c.out {
			if _, err := conn.Write(line); err != nil {
				conn.Close()
				for range c.out {
				}
				return
			}
		}
	}()
	defer func() {
		s.mu.Lock()
		s.controls--
		delete(s.ctrls, c)
		s.idleFrom = time.Now()
		s.mu.Unlock()
		close(c.out)
		<-written
		conn.Close()
	}()
	line := first
	for {
		var req Request
		if err := json.Unmarshal(line, &req); err == nil {
			b, _ := json.Marshal(s.request(c, req))
			c.out <- append(b, '\n')
		}
		var err error
		line, err = r.ReadBytes('\n')
		if err != nil {
			return
		}
	}
}

// readOnly are the methods that change nothing to save.
var readOnly = map[string]bool{"session.list": true, "session.view": true, "session.attach": true}

func (s *Server) request(c *ctrl, req Request) Response {
	var data any
	var err error
	if req.Method != "" {
		data, err = s.call(c, req.Method, req.Params)
		if err == nil && !readOnly[req.Method] {
			s.save()
		}
	} else {
		data, err = s.do(req)
	}
	res := Response{ID: req.ID}
	if err != nil {
		res.Error = err.Error()
		var ae *APIError
		if errors.As(err, &ae) {
			res.Code = ae.Code
		}
	} else if data != nil {
		res.Data, _ = json.Marshal(data)
	}
	return res
}

func (s *Server) block(id string) (*block, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.blocks[id]; b != nil {
		return b, nil
	}
	return nil, fmt.Errorf("no block %q", id)
}

// do runs one of GoRex's own operations.
func (s *Server) do(req Request) (any, error) {
	switch req.Op {
	case "hello":
		select {
		case <-s.hostDone:
		case <-time.After(3 * time.Second):
		}
		return Hello{Version: ProtocolVersion, PID: os.Getpid(), Started: s.started, Host: s.host, Exe: s.exe, ExeTime: s.exeTime}, nil
	case "list":
		s.mu.Lock()
		all := make([]*block, 0, len(s.blockOrder))
		for _, id := range s.blockOrder {
			all = append(all, s.blocks[id])
		}
		s.mu.Unlock()
		infos := make([]BlockInfo, len(all))
		for i, b := range all {
			infos[i] = b.info()
		}
		return infos, nil
	case "resize":
		b, err := s.block(req.SID)
		if err != nil {
			return nil, err
		}
		b.resize(req.Cols, req.Rows)
		return nil, nil
	case "shutdown":
		go func() {
			time.Sleep(50 * time.Millisecond)
			s.shutdown()
		}()
		return nil, nil
	}
	return nil, fmt.Errorf("unknown op %q", req.Op)
}

// Spawn starts a server in the background, as a process of its own that
// outlives the app: the executable run with -server.
func Spawn() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(Dir(), "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, "-server")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	cmd.SysProcAttr = detached()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
