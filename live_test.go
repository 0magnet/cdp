package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"net/url"
	"os"
	"testing"
	"time"
)

// TestLive drives a real browser, in a tab of its own that it closes again:
//
//	CDP_LIVE=127.0.0.1:9222 go test -run Live -v
//
// It is skipped when CDP_LIVE is unset, which is how CI runs it.
func TestLive(t *testing.T) {
	addr := os.Getenv("CDP_LIVE")
	if addr == "" {
		t.Skip("set CDP_LIVE=host:port to run against a browser")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := Browser{Addr: addr}

	v, err := b.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("browser %s, protocol %s", v.Browser, v.ProtocolVersion)

	tab, err := b.NewTab(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := b.CloseTab(context.Background(), tab.ID); err != nil {
			t.Errorf("CloseTab: %v", err)
		}
	}()

	c, err := DialWS(ctx, tab.WebSocketDebuggerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck
	for _, m := range []string{"Runtime.enable", "Page.enable"} {
		if err := c.Do(ctx, m, nil, nil); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}

	page := "data:text/html," + url.PathEscape(`<body style="margin:0;background:#000">`+
		`<div id=b style="width:200px;height:100px;background:#fff" onclick="window.hits=(window.hits||0)+1"></div>`+
		`<input id=q style="background:#000;color:#000;border:0" onkeyup="window.ups=(window.ups||0)+1">`)
	loaded, stop := c.Events(64)
	if err := c.Navigate(ctx, page); err != nil {
		t.Fatal(err)
	}
	waitFor(t, loaded, "Page.loadEventFired")
	stop()

	raw, err := c.Evaluate(ctx, `new Promise(r => setTimeout(() => r(6*7), 10))`)
	if err != nil || string(raw) != "42" {
		t.Fatalf("Evaluate awaiting a promise = %s, %v", raw, err)
	}
	var ee *ExceptionError
	if _, err := c.Evaluate(ctx, `null.x`); !errors.As(err, &ee) {
		t.Fatalf("Evaluate(null.x) = %v, want an ExceptionError", err)
	}
	t.Logf("exception: %s", ee.Text)

	logs, stop := c.Events(64)
	if _, err := c.Evaluate(ctx, `console.log("hello from the page")`); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, logs, "Runtime.consoleAPICalled")
	stop()
	var cl struct {
		Args []struct{ Value string } `json:"args"`
	}
	if err := json.Unmarshal(ev.Params, &cl); err != nil || len(cl.Args) == 0 || cl.Args[0].Value != "hello from the page" {
		t.Fatalf("console event = %s", ev.Params)
	}

	c.Click(100, 50)
	c.Click(100, 50)
	c.Click(300, 300) // off the box
	if got := c.Eval(`window.hits`); got != float64(2) {
		t.Fatalf("hits after two clicks on the box = %v", got)
	}

	if err := c.Focus(ctx, "#q"); err != nil {
		t.Fatal(err)
	}
	if err := c.Type(ctx, "a=b é"); err != nil {
		t.Fatal(err)
	}
	if err := c.Press(ctx, "Enter"); err != nil {
		t.Fatal(err)
	}
	if got := c.Eval(`document.getElementById("q").value + " " + window.ups`); got != "a=b é 1" {
		t.Fatalf(`typed field and keyups = %q, want "a=b é 1"`, got)
	}

	png1, err := c.ScreenshotPNG(ctx)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(png1))
	if err != nil {
		t.Fatal(err)
	}
	box := BrightBBox(img, img.Bounds())
	t.Logf("screenshot %v, bright box %v", img.Bounds(), box)
	if box.Empty() || box.Min.X > 2 || box.Min.Y > 2 {
		t.Fatalf("the white box is not at the top left: %v", box)
	}
}

func waitFor(t *testing.T, evs <-chan Event, method string) Event {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-evs:
			if !ok {
				t.Fatalf("connection ended waiting for %s", method)
			}
			if ev.Method == method {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s", method)
		}
	}
}
