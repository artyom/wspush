package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/artyom/autoflags"
	"github.com/artyom/resp"

	"golang.org/x/net/websocket"
)

func main() {
	args := &struct {
		Addr   string `flag:"addr,address to listen"`
		Redis  string `flag:"redis,redis address"`
		Prefix string `flag:"prefix,prefix for 'PSUBSCRIBE prefix*'"`
	}{
		Addr:   "localhost:8080",
		Redis:  "localhost:6379",
		Prefix: "example.",
	}
	autoflags.Parse(args)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialFunc := func() (net.Conn, error) { return net.DialTimeout("tcp", args.Redis, 5*time.Second) }
	log := log.New(os.Stderr, "", log.LstdFlags)
	srv := &http.Server{
		Addr:    args.Addr,
		Handler: New(ctx, log, dialFunc, args.Prefix),
	}
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		log.Println(<-sigCh)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type DialFunc func() (net.Conn, error)

func New(ctx context.Context, log *log.Logger, dial DialFunc, prefix string) http.Handler {
	h := &hub{
		ctx: ctx,
		log: log,
		m:   make(map[string][]subscription),
	}
	go h.loop(ctx, dial, prefix)
	return h
}

// subscription describes single client connection subscribed to receive
// messages
type subscription struct {
	id uint64
	ch chan string
}

type hub struct {
	ctx  context.Context
	log  *log.Logger
	next uint64 // next subscription id counter
	mu   sync.Mutex
	m    map[string][]subscription
}

// register creates and registers channel to receive messages for given key,
// also returning function to deregister this channel
func (h *hub) register(key string) (<-chan string, func()) {
	id := atomic.AddUint64(&h.next, 1)
	ch := make(chan string, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	// TODO: limit max. number of connections per token
	h.m[key] = append(h.m[key], subscription{id: id, ch: ch})
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		subs := h.m[key]
		var idx int
		for i, s := range subs {
			if s.id == id {
				idx = i
				goto found
			}
		}
		return
	found:
		subsLen := len(subs)
		if subsLen <= 1 { // shortcut for single connection from one client case
			delete(h.m, key)
			return
		}
		subs[idx] = subs[subsLen-1]
		subs = subs[:subsLen-1]
		h.m[key] = subs
	}
}

// deliver pushes payload to channel registered at key, if any. Push is done in
// a non-blocking manner, if channel is full, message is dropped.
func (h *hub) deliver(key string, payload string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.m[key] {
		select {
		case s.ch <- payload:
		default:
		}
	}
}

// loop is expected to be running in a separate goroutine for the duration of
// hub lifetime. It (re-)dials redis using dial, issues PSUBSCRIBE command on
// redis pubsub channel with pattern constructed from prefix followed by an
// asterisk ("prefix*") then delivers received messages to registered channels,
// deriving key from pubsub channel name by stripping prefix from it.
//
// E.g.: given prefix "news.", message pushed to pubsub channel "news.foobar"
// would be delivered to client subscribed to "foobar".
func (h *hub) loop(ctx context.Context, dial DialFunc, prefix string) {
	sleepCh := time.After(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sleepCh:
		}
		conn, err := dial()
		if err != nil {
			h.log.Print(err)
			sleepCh = time.After(time.Second)
			continue
		}
		if err := h.ingestPubSub(ctx, conn, prefix); err != nil {
			h.log.Print(err)
		}
	}
}

// ingestPubSub accepts established redis connection, subscribes to redis pubsub
// channel and delivers messages to registered clients. See documentation for
// loop().
func (h *hub) ingestPubSub(ctx context.Context, conn io.ReadWriteCloser, prefix string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); conn.Close() }()
	defer conn.Close()
	if err := resp.Encode(conn, resp.Array{"psubscribe", prefix + "*"}); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	for {
		msg, err := decodePubsub(r)
		if err != nil {
			return err
		}
		if msg.typ != "pmessage" {
			continue
		}
		h.deliver(strings.TrimPrefix(msg.recip, prefix), msg.data)
	}
}

// pubsubMsg describes 3-strings array returned by redis over pubsub channel
type pubsubMsg struct {
	typ, recip string
	data       string
}

// decodePubsub decodes single pubsub message from redis protocol
func decodePubsub(r resp.BytesReader) (*pubsubMsg, error) {
	val, err := resp.Decode(r)
	if err != nil {
		return nil, err
	}
	ar, ok := val.(resp.Array)
	if !ok {
		return nil, resp.ErrNotAnArray
	}
	if l := len(ar); l != 3 && l != 4 {
		return nil, fmt.Errorf("invalid array length: %d", l)
	}
	v, ok := ar[0].(string)
	if !ok {
		return nil, fmt.Errorf("array element 0 is not a string")
	}
	msg := &pubsubMsg{typ: v}
	switch msg.typ {
	case "pmessage":
	default:
		return msg, nil
	}
	for i, v := range ar[2:] {
		v, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("array element %d is not a string", i+2)
		}
		switch i {
		case 0:
			msg.recip = v
		case 1:
			msg.data = v
		}
	}
	return msg, nil
}

func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" || len(token) > 64 {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	fn := func(ws *websocket.Conn) {
		defer ws.Close()
		ch, closeFn := h.register(token)
		defer closeFn()
		for {
			select {
			case <-h.ctx.Done():
				return
			case msg := <-ch:
				if err := websocket.Message.Send(ws, msg); err != nil {
					h.log.Print(err)
					return
				}
			}
		}
	}
	websocket.Handler(fn).ServeHTTP(w, r)
}
