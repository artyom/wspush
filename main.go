// Command wspush implements http service relaying redis pubsub messages to
// websocket connections or as server-sent events.
//
// wspush subscribes to redis channel(s) using "PSUBSCRIBE prefix*" command
// where prefix can be set with -prefix flag. When it receives a message
// published to "prefixFoo" channel, it looks up any connected client(s) with
// query string parameter "token=Foo" and sends message to each client as a
// single websocket binary frame.
//
// If client connects to path ending with /sse, or having "Accept:
// text/event-stream" header, messages are delivered as server-sent events
// (https://www.w3.org/TR/eventsource/), with default "message" event type. It
// is expected that message published to redis is valid utf8 string.
//
// If program started with -hmac flag set to base64 encoded (url-compatible,
// padless) secret key, this key is used to verify tokens, which then must be
// base64 encoded (url-compatible, padless) values of payload concatenated with
// its md5 HMAC signature. Alternatively, key value can also be passed with a
// WSPUSH_KEY environment variable.
package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/base64"
	"expvar"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/artyom/autoflags"
	"github.com/artyom/resp"

	"golang.org/x/net/websocket"
)

func main() {
	args := &struct {
		Addr   string `flag:"addr,address to listen"`
		Redis  string `flag:"redis,redis address"`
		Prefix string `flag:"prefix,prefix for 'PSUBSCRIBE prefix*'"`
		Expvar string `flag:"expvar,address to serve expvar statistics"`
		Key    string `flag:"hmac,optional base64 (url-compatible, padless) encoded key for md5 HMAC verification"`
	}{
		Addr:   "localhost:8080",
		Redis:  "localhost:6379",
		Prefix: "example.",
		Key:    os.Getenv("WSPUSH_KEY"),
	}
	autoflags.Parse(args)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialFunc := func() (net.Conn, error) {
		conn, err := net.DialTimeout("tcp", args.Redis, 5*time.Second)
		if err != nil {
			return nil, err
		}
		conn.(*net.TCPConn).SetKeepAlive(true)
		conn.(*net.TCPConn).SetKeepAlivePeriod(time.Minute)
		return conn, nil
	}
	log := log.New(os.Stderr, "", log.LstdFlags)
	handler := New(ctx, log, dialFunc, args.Prefix)
	if args.Key != "" {
		var err error
		if handler.key, err = base64.RawURLEncoding.DecodeString(args.Key); err != nil {
			log.Fatal("cannot decode key:", err)
		}
	}
	if args.Expvar != "" {
		expvar.Publish("wspush", handler)
		ln, err := net.Listen("tcp", args.Expvar)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("exposing statistics on http://%s/debug/vars", ln.Addr())
		go func() { log.Fatal(http.Serve(ln, nil)) }()
	}
	srv := &http.Server{
		Addr:    args.Addr,
		Handler: handler,
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

func New(ctx context.Context, log *log.Logger, dial DialFunc, prefix string) *hub {
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
	key  []byte // non-nil value enables hmac verification of token
	mu   sync.Mutex
	m    map[string][]subscription

	// statistics updated with atomics
	delivers  uint64 // number of deliver method calls
	successes uint64 // successful writes to client
}

// String returns json-encoded value of published counters; implements
// expvar.Var.
func (h *hub) String() string {
	b := []byte(`{"sessions_seen": `)
	b = strconv.AppendUint(b, atomic.LoadUint64(&h.next), 10)
	b = append(b, `, "messages_seen": `...)
	b = strconv.AppendUint(b, atomic.LoadUint64(&h.delivers), 10)
	b = append(b, `, "successful_deliveries": `...)
	b = strconv.AppendUint(b, atomic.LoadUint64(&h.successes), 10)
	b = append(b, '}')
	return string(b)
}

// register creates and registers channel to receive messages for given key,
// also returning function to deregister this channel and status: if false,
// connection should be aborted immediately due to exceeded limit of per-token
// connections
func (h *hub) register(key string) (<-chan string, func(), bool) {
	id := atomic.AddUint64(&h.next, 1)
	ch := make(chan string, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.m[key]) >= 15 {
		return nil, nil, false
	}
	h.m[key] = append(h.m[key], subscription{id: id, ch: ch})
	fn := func() {
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
	return ch, fn, true
}

// deliver pushes payload to channel registered at key, if any. Push is done in
// a non-blocking manner, if channel is full, message is dropped.
func (h *hub) deliver(key string, payload string) {
	atomic.AddUint64(&h.delivers, 1)
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		conn, err := dial()
		if err != nil {
			h.log.Print("redis dial:", err)
			continue
		}
		if err := h.ingestPubSub(ctx, conn, prefix); err != nil {
			h.log.Print("pubsub ingest:", err)
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
	if r.URL.Path == "/ping" {
		return
	}
	token := r.URL.Query().Get("token")
	if !h.tokenValid(token) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	if strings.HasSuffix(r.URL.Path, "/sse") || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Server-Sent Events are not supported", http.StatusNotImplemented)
			return
		}
		ch, closeFn, ok := h.register(token)
		if !ok {
			return
		}
		defer closeFn()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for {
			select {
			case <-h.ctx.Done():
				return
			case <-ticker.C:
				// https://www.w3.org/TR/eventsource/#event-stream-interpretation
				// "The steps to process the field" state that
				// unknown field names should be ignored, so use
				// unspecified "ping" field.
				if _, err := w.Write([]byte("ping:\n\n")); err != nil {
					panic(http.ErrAbortHandler)
				}
				flusher.Flush()
			case msg := <-ch:
				if len(msg) == 0 {
					continue
				}
				if !utf8.ValidString(msg) {
					h.log.Println("message is not a valid utf8 sequence")
					panic(http.ErrAbortHandler)
				}
				for _, chunk := range strings.Split(msg, "\n") {
					if _, err := fmt.Fprintf(w, "data:%s\n", chunk); err != nil {
						panic(http.ErrAbortHandler)
					}
				}
				if _, err := w.Write([]byte("\n")); err != nil {
					panic(http.ErrAbortHandler)
				}
				atomic.AddUint64(&h.successes, 1)
				flusher.Flush()
			}
		}
	}
	fn := func(ws *websocket.Conn) {
		defer ws.Close()
		ch, closeFn, ok := h.register(token)
		if !ok {
			return
		}
		defer closeFn()
		for {
			select {
			case <-h.ctx.Done():
				return
			case <-ticker.C:
				// https://tools.ietf.org/html/rfc6455#section-5.5.3
				// > A Pong frame MAY be sent unsolicited.
				// > This serves as a unidirectional heartbeat.
				// > A response to an unsolicited Pong frame is
				// > not expected.
				ws.PayloadType = websocket.PongFrame
				if _, err := ws.Write([]byte{}); err != nil {
					return
				}
			case msg := <-ch:
				if err := websocket.Message.Send(ws, msg); err != nil {
					return
				}
				atomic.AddUint64(&h.successes, 1)
			}
		}
	}
	websocket.Handler(fn).ServeHTTP(w, r)
}

func (h *hub) tokenValid(token string) bool {
	if token == "" || len(token) > 64 {
		return false
	}
	if h.key == nil {
		return true
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(data) <= md5.Size {
		return false
	}
	cutoff := len(data) - md5.Size
	messageMAC := data[cutoff:]
	mac := hmac.New(md5.New, h.key)
	mac.Write(data[:cutoff])
	return hmac.Equal(messageMAC, mac.Sum(nil))
}
