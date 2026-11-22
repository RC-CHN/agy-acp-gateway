package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type obj = map[string]any

func asObj(v any) obj  { m, _ := v.(map[string]any); return m }
func str(v any) string { s, _ := v.(string); return s }
func arr(v any) []any  { a, _ := v.([]any); return a }
func enc(v any) string { b, _ := json.Marshal(v); return string(b) }
func uid(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

type frame struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  obj             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}
type rpcReply struct {
	wire   *wireError
	result obj
	err    error
}
type peer struct {
	observeStatus bool
	writer        io.Writer
	writeMu       sync.Mutex
	mu            sync.Mutex
	pending       map[string]chan rpcReply
	seq           atomic.Uint64
	done          chan struct{}
	once          sync.Once
	terminalErr   error
	incoming      func(*peer, frame)
}

func newPeer(w io.Writer, receive func(*peer, frame)) *peer {
	return &peer{writer: w, pending: map[string]chan rpcReply{}, done: make(chan struct{}), incoming: receive}
}
func (p *peer) stopWith(err error) { p.once.Do(func() { p.terminalErr = err; close(p.done) }) }
func (p *peer) stop()              { p.stopWith(workerLost) }
func (p *peer) send(v any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	select {
	case <-p.done:
		return p.terminalErr
	default:
	}
	b, err := json.Marshal(v)
	if err == nil {
		if w, ok := p.writer.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = w.SetWriteDeadline(time.Now().Add(5 * time.Second))
		}
		_, err = p.writer.Write(append(b, '\n'))
	}
	if err != nil {
		p.stop()
		return p.terminalErr
	}
	return nil
}
func (p *peer) read(r io.Reader) {
	defer p.stop()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 48<<20)
	for sc.Scan() {
		var f frame
		if json.Unmarshal(sc.Bytes(), &f) != nil || f.Version != "2.0" {
			p.stopWith(protocolFailure)
			return
		}
		if (f.Method == "" && len(f.ID) == 0) || (f.Method != "" && (f.Error != nil || f.Result != nil)) {
			p.stopWith(protocolFailure)
			return
		}
		if f.Method != "" {
			p.incoming(p, f)
			continue
		}
		p.mu.Lock()
		ch := p.pending[string(f.ID)]
		p.mu.Unlock()
		if ch == nil {
			continue
		}
		var reply rpcReply
		if f.Error != nil {
			if f.Error.Code == nil || f.Error.Message == nil || f.Result != nil {
				reply.err = protocolFailure
			} else {
				reply.wire = f.Error
			}
		} else if json.Unmarshal(f.Result, &reply.result) != nil || reply.result == nil {
			reply.err = protocolFailure
		}
		select {
		case ch <- reply:
		default:
		}
	}
	if sc.Err() == bufio.ErrTooLong {
		p.stopWith(protocolFailure)
	}
}
func (p *peer) call(ctx context.Context, method string, params obj) (obj, error) {
	id := fmt.Sprint(p.seq.Add(1))
	ch := make(chan rpcReply, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.send(obj{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params}); err != nil {
		return nil, err
	}
	decode := func(r rpcReply) (obj, error) {
		if r.wire != nil {
			return nil, &rpcFailure{wire: r.wire, method: method, sessionID: str(params["sessionId"]), model: str(params["modelId"]), configID: str(params["configId"]), value: str(params["value"])}
		}
		return r.result, r.err
	}
	var terminal error
	select {
	case r := <-ch:
		return decode(r)
	case <-ctx.Done():
		terminal = ctx.Err()
	case <-p.done:
		terminal = p.terminalErr
	}
	// A reply already delivered by the reader belongs to this request.
	select {
	case r := <-ch:
		return decode(r)
	default:
		return nil, terminal
	}
}
func (p *peer) notify(method string, params obj) {
	_ = p.send(obj{"jsonrpc": "2.0", "method": method, "params": params})
}
func (p *peer) reply(id json.RawMessage, result any) {
	_ = p.send(obj{"jsonrpc": "2.0", "id": id, "result": result})
}
func (p *peer) reject(id json.RawMessage) {
	_ = p.send(obj{"jsonrpc": "2.0", "id": id, "error": obj{"code": -32601, "message": "Method not found"}})
}
