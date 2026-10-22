// SPDX-License-Identifier: Apache-2.0

package esl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitConnect waits for a Link's OnConnect hook.
func waitConnect(t *testing.T, connects <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-connects:
	case <-time.After(5 * time.Second):
		t.Fatalf("the link did not connect (%s)", what)
	}
}

// The switch stops reading its socket while commands keep coming: the kernel
// buffers fill and a write blocks. No caller may wait past the command
// timeout for it (#302). A command whose bytes, any of them, reached the
// connection is ErrTimeout, because the switch may read and run it; one that
// never got to write is ErrDown. The link is closed, since a command cut off
// mid-write leaves the stream unframed, and the Link reconnects: the next
// command gets its own reply.
func TestABlockedWriteEndsAtTheTimeoutAndTheLinkReconnects(t *testing.T) {
	t.Parallel()
	const timeout = 300 * time.Millisecond
	f := newFakeSwitchBuffered(t, "ClueCon", 4096)
	f.deafen = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	link := NewLink(f.addr(), "ClueCon", []string{"CHANNEL_CREATE"})
	link.timeout = timeout
	connects := make(chan struct{}, 4)
	link.OnConnect(func(context.Context) { connects <- struct{}{} })
	go link.Run(ctx)
	fc := f.waitConn(t)
	waitConnect(t, connects, "first")
	client := link.client.Load()
	// A small send buffer as well, so a few commands fill both ends.
	if err := client.conn.(*net.TCPConn).SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}

	// An originate with 4 KB of variables, from 20 goroutines, each sending
	// until the link is gone. Every command names itself first, so what the
	// switch read can be matched to what each caller was told.
	pad := strings.Repeat("x", 4096)
	type result struct {
		id      string
		err     error
		elapsed time.Duration
	}
	var (
		mu      sync.Mutex
		results []result
		wg      sync.WaitGroup
	)
	for g := range 20 {
		wg.Go(func() {
			for i := range 500 {
				id := fmt.Sprintf("leg-%02d-%03d", g, i)
				start := time.Now()
				_, err := client.BgAPI("originate {origination_uuid=" + id + ",aicc_pad=" + pad + "}sofia/gateway/x/1000 &park()")
				mu.Lock()
				results = append(results, result{id, err, time.Since(start)})
				mu.Unlock()
				if err == nil || errors.Is(err, ErrDown) {
					return
				}
			}
		})
	}
	wg.Wait()

	select {
	case <-client.Done():
	default:
		t.Fatal("the link is still open after a write was cut off")
	}
	// What the switch had been sent, read now that it listens again.
	close(f.deafen)
	select {
	case <-fc.heardDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the switch could not read what it was sent")
	}
	heard := map[string]bool{}
	for _, m := range regexp.MustCompile(`origination_uuid=(leg-\d\d-\d\d\d)`).FindAllStringSubmatch(string(fc.heard), -1) {
		heard[m[1]] = true
	}

	var timedOut, down, cutOff, unheard int
	for _, r := range results {
		if r.elapsed > timeout+time.Second {
			t.Errorf("%s waited %s, past the timeout %s + 1 s", r.id, r.elapsed, timeout)
		}
		switch {
		case r.err == nil:
			t.Errorf("%s was answered by a switch that answers nothing", r.id)
		case errors.Is(r.err, ErrTimeout) && errors.Is(r.err, ErrDown):
			t.Errorf("%s: %v reads as both", r.id, r.err)
		case errors.Is(r.err, ErrTimeout):
			timedOut++
			if strings.Contains(r.err.Error(), "write cut off") {
				cutOff++
			}
			if !heard[r.id] {
				// Only a command cut off within its first few bytes can be
				// written and not be recognisable.
				unheard++
			}
		case errors.Is(r.err, ErrDown):
			down++
			if heard[r.id] {
				t.Errorf("%s reached the switch but was reported as not sent: %v", r.id, r.err)
			}
		default:
			t.Errorf("%s: %v, want ErrTimeout or ErrDown", r.id, r.err)
		}
	}
	if timedOut == 0 || down == 0 {
		t.Errorf("%d timed out, %d down; want both: commands written and commands queued behind the closed link", timedOut, down)
	}
	if cutOff+unheard > 1 || unheard > cutOff {
		t.Errorf("%d cut off, %d timed out but unheard: only the one cut-off command may be unrecognisable", cutOff, unheard)
	}
	for id := range heard {
		found := false
		for _, r := range results {
			if r.id == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the switch read %s, which no caller sent", id)
		}
	}
	t.Logf("%d commands: %d timed out (%d cut off), %d not sent", len(results), timedOut, cutOff, down)

	// The Link reconnects on its own, and the replies on the new connection
	// are the new commands' own.
	waitConnect(t, connects, "after the blocked write")
	f.waitConn(t)
	body, err := link.API("uuid_exists leg-after")
	if err != nil || body != "+OK uuid_exists leg-after" {
		t.Errorf("API() after the reconnect = %q, %v; want its own reply", body, err)
	}
}

// The switch reads the originate and the connection then closes, with no
// reply. The switch may have started the job, which runs whatever happens to
// the socket: ErrTimeout, never ErrDown (#302). On the reconnected link the
// switch's events for that leg arrive like any others.
func TestALinkThatClosesAfterTheWriteIsAmbiguous(t *testing.T) {
	t.Parallel()
	f := newFakeSwitch(t, "ClueCon")
	f.hangUpOnBgapi.Store(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link := NewLink(f.addr(), "ClueCon", []string{"CHANNEL_CREATE"})
	connects := make(chan struct{}, 4)
	link.OnConnect(func(context.Context) { connects <- struct{}{} })
	go link.Run(ctx)
	fc := f.waitConn(t)
	waitConnect(t, connects, "first")

	start := time.Now()
	_, err := link.BgAPI("originate {origination_uuid=leg-302}sofia/gateway/x/1000 &park()")
	if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrDown) {
		t.Fatalf("BgAPI() error = %v, want ErrTimeout alone", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("BgAPI() took %s: a closed link is known at once", d)
	}
	<-fc.cmds // event plain
	if cmd := <-fc.cmds; !strings.Contains(cmd, "origination_uuid=leg-302") {
		t.Fatalf("the switch read %q", cmd)
	}

	waitConnect(t, connects, "after the close")
	fc2 := f.waitConn(t)
	fc2.pushEvent("Event-Name: CHANNEL_CREATE\nUnique-ID: leg-302\n")
	select {
	case ev := <-link.Events():
		if ev.UniqueID() != "leg-302" {
			t.Errorf("event for %q", ev.UniqueID())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the leg's event never arrived on the reconnected link")
	}
}

// The connection is gone before the command is issued: nothing is written,
// and only that is ErrDown.
func TestALinkClosedBeforeTheWriteIsDown(t *testing.T) {
	t.Parallel()
	f := newFakeSwitch(t, "ClueCon")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client, err := Dial(ctx, f.addr(), "ClueCon")
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()
	fc := f.waitConn(t)
	fc.Conn.Close() // the switch goes away
	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the client did not notice the switch went away")
	}

	_, err = client.BgAPI("originate {origination_uuid=leg-0}sofia/gateway/x/1000 &park()")
	if !errors.Is(err, ErrDown) || errors.Is(err, ErrTimeout) {
		t.Errorf("BgAPI() error = %v, want ErrDown alone", err)
	}
}
