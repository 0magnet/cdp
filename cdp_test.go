package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeBrowser is just enough of a remote-debugging endpoint to exercise the
// client: one page target whose websocket answers a few made-up methods.
//
//	Echo      answers with its params, after emitting a Test.event
//	Hang      is never answered
//	Bad       is answered with a protocol error
//	Slow      is answered after 50ms, so replies arrive out of order
//	Runtime.evaluate  "throw" throws; anything else returns its length
func fakeBrowser(t *testing.T) Browser {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	page := Target{ID: "P1", Type: "page", URL: "http://app.test/index.html", WebSocketDebuggerURL: "ws://" + addr + "/devtools/page/P1"}

	mux.HandleFunc("/json/list", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]Target{ //nolint:errcheck
			{ID: "W1", Type: "service_worker", URL: "http://app.test/sw.js", WebSocketDebuggerURL: "ws://" + addr + "/x"},
			page,
		})
	})
	mux.HandleFunc("/json/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "use PUT", http.StatusMethodNotAllowed)
			return
		}
		nt := page
		nt.ID, nt.URL = "P2", r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(nt) //nolint:errcheck
	})
	mux.HandleFunc("/json/close/", func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.URL.Path, "/json/close/") != "P2" {
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/devtools/page/P1", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow() //nolint:errcheck
		var wmu sync.Mutex
		write := func(v any) {
			b, _ := json.Marshal(v) //nolint:errcheck
			wmu.Lock()
			defer wmu.Unlock()
			_ = ws.Write(context.Background(), websocket.MessageText, b) //nolint:errcheck
		}
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			_ = json.Unmarshal(data, &m) //nolint:errcheck
			switch m.Method {
			case "Echo":
				write(map[string]any{"method": "Test.event", "params": m.Params})
				write(map[string]any{"id": m.ID, "result": m.Params})
			case "Hang":
			case "Bad":
				write(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "'Bad' wasn't found"}})
			case "Slow":
				go func(id int64) {
					time.Sleep(50 * time.Millisecond)
					write(map[string]any{"id": id, "result": map[string]any{"slow": true}})
				}(m.ID)
			case "Runtime.evaluate":
				var p struct{ Expression string }
				_ = json.Unmarshal(m.Params, &p) //nolint:errcheck
				if p.Expression == "throw" {
					write(map[string]any{"id": m.ID, "result": map[string]any{
						"result":           map[string]any{"type": "object"},
						"exceptionDetails": map[string]any{"text": "Uncaught", "exception": map[string]any{"description": "Error: boom\n    at <anonymous>:1:7"}},
					}})
					continue
				}
				write(map[string]any{"id": m.ID, "result": map[string]any{"result": map[string]any{"type": "number", "value": len(p.Expression)}}})
			default:
				write(map[string]any{"id": m.ID, "result": map[string]any{}})
			}
		}
	})
	return Browser{Addr: addr}
}

func dialFake(t *testing.T) *Client {
	t.Helper()
	b := fakeBrowser(t)
	ctx := context.Background()
	tg, err := b.Find(ctx, "app.test")
	if err != nil {
		t.Fatal(err)
	}
	c, err := DialWS(ctx, tg.WebSocketDebuggerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() }) //nolint:errcheck
	return c
}

func TestFindSkipsNonPages(t *testing.T) {
	b := fakeBrowser(t)
	tg, err := b.Find(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if tg.ID != "P1" {
		t.Fatalf("Find(\"\") = %s, want the page P1, not the worker", tg.ID)
	}
	if _, err := b.Find(context.Background(), "nowhere"); err == nil {
		t.Fatal("Find of an absent URL succeeded")
	}
}

func TestNewTabAndCloseTab(t *testing.T) {
	b := fakeBrowser(t)
	ctx := context.Background()
	tg, err := b.NewTab(ctx, "http://app.test/?a=b&c")
	if err != nil {
		t.Fatal(err)
	}
	if tg.ID != "P2" || tg.URL != "http%3A%2F%2Fapp.test%2F%3Fa%3Db%26c" {
		t.Fatalf("NewTab = %+v", tg)
	}
	if err := b.CloseTab(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.CloseTab(ctx, "nope"); err == nil {
		t.Fatal("closing an unknown tab succeeded")
	}
}

func TestDoMatchesRepliesToCallers(t *testing.T) {
	c := dialFake(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%4 == 0 {
				var r struct{ Slow bool }
				if err := c.Do(ctx, "Slow", nil, &r); err != nil || !r.Slow {
					errs <- fmt.Errorf("Slow: %v %+v", err, r)
				}
				return
			}
			var r struct{ N int }
			if err := c.Do(ctx, "Echo", map[string]int{"n": i}, &r); err != nil || r.N != i {
				errs <- fmt.Errorf("Echo %d: got %d, %v", i, r.N, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestProtocolError(t *testing.T) {
	c := dialFake(t)
	err := c.Do(context.Background(), "Bad", nil, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != -32601 {
		t.Fatalf("Do(Bad) = %v, want a *Error with code -32601", err)
	}
	m, err := c.Call("Bad", nil)
	if !errors.As(err, &pe) || m["error"] == nil {
		t.Fatalf("Call(Bad) = %v, %v; want the message and a *Error", m, err)
	}
}

func TestTimeoutFreezesButKeepsTheConnection(t *testing.T) {
	c := dialFake(t)
	c.Timeout = 50 * time.Millisecond
	if _, err := c.Call("Hang", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call(Hang) = %v, want a deadline", err)
	}
	if !c.Frozen() {
		t.Fatal("a timed-out Call did not mark the client Frozen")
	}
	if _, err := c.Call("Echo", map[string]any{"still": "here"}); err != nil {
		t.Fatalf("the connection did not survive a timeout: %v", err)
	}
}

func TestEvents(t *testing.T) {
	c := dialFake(t)
	evs, cancel := c.Events(8)
	if err := c.Do(context.Background(), "Echo", map[string]int{"n": 7}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-evs:
		if ev.Method != "Test.event" || string(ev.Params) != `{"n":7}` {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	cancel()
	cancel() // idempotent
	if _, ok := <-evs; ok {
		t.Fatal("the channel stayed open after cancel")
	}
}

func TestEvaluate(t *testing.T) {
	c := dialFake(t)
	ctx := context.Background()
	v, err := c.Evaluate(ctx, "1+2")
	if err != nil || string(v) != "3" {
		t.Fatalf("Evaluate = %s, %v", v, err)
	}
	if got := c.Eval("abcd"); got != float64(4) {
		t.Fatalf("Eval = %v", got)
	}
	_, err = c.Evaluate(ctx, "throw")
	var ee *ExceptionError
	if !errors.As(err, &ee) || !strings.HasPrefix(ee.Text, "Error: boom") {
		t.Fatalf("Evaluate(throw) = %v, want the exception's description", err)
	}
	if c.Eval("throw") != nil {
		t.Fatal("Eval of a throw is not nil")
	}
}

func TestCloseEndsEverything(t *testing.T) {
	c := dialFake(t)
	evs, _ := c.Events(1)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed")
	}
	if _, ok := <-evs; ok {
		t.Fatal("events channel open after Close")
	}
	if err := c.Do(context.Background(), "Echo", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Do after Close = %v, want ErrClosed", err)
	}
	if !errors.Is(c.Err(), ErrClosed) {
		t.Fatalf("Err = %v", c.Err())
	}
}
