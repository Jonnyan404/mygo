//go:build windows

package d3d11

import (
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/egoist/mygo/internal/pacing"
	"github.com/egoist/mygo/internal/scene"
	"github.com/egoist/mygo/internal/wait"
)

// TestUncappedSwapChain checks the swap chains of renderers made
// uncapped (MYGO_UNCAPPED): no frame latency object, at least three
// buffers, kept as the swap chain resizes, tearing allowed exactly where
// the system shows frames torn and the swap chain is the window's; and
// that frames following each other never wait for the display, whether
// they present or change nothing. The default, paced, keeps the object and
// no tearing.
func TestUncappedSwapChain(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func(on bool) { pacing.Uncapped = on }(pacing.Uncapped)
	const scGetDesc1 = 18 // IDXGISwapChain1
	desc := func(r *Renderer) swapChainDesc1 {
		var d swapChainDesc1
		call(r.swapChain, scGetDesc1, uintptr(unsafe.Pointer(&d)))
		return d
	}
	for _, uncapped := range []bool{false, true} {
		pacing.Uncapped = uncapped
		for _, composed := range []bool{false, true} {
			newRenderer := New
			if composed {
				newRenderer = NewComposed
			}
			r, err := newRenderer(hiddenWindow(t, 200, 150))
			if err != nil {
				t.Skip("no Direct3D 11:", err)
			}
			var posted []func()
			r.SetPost(func(fn func()) { posted = append(posted, fn) })
			bg := scene.Color{R: 24, G: 24, B: 27, A: 255}
			if err := r.Render(&scene.Scene{Width: 200, Height: 150, Background: bg}); err != nil {
				r.Release()
				t.Fatal(err)
			}
			d := desc(r)
			waitable := d.Flags&swapChainWaitable != 0
			tearing := d.Flags&swapChainTearing != 0
			if !uncapped {
				if tearing || r.tearing || r.uncapped {
					t.Errorf("composed %v, paced: tearing %v (flag %v), uncapped %v", composed, r.tearing, tearing, r.uncapped)
				}
				if r.swapChain2 != 0 && (!waitable || r.lat.handle == 0) {
					t.Errorf("composed %v, paced: waitable %v, latency object %#x", composed, waitable, r.lat.handle)
				}
				r.Release()
				continue
			}
			wantTearing := !composed && tearingSupported(r.factory)
			if waitable || r.lat.handle != 0 || tearing != wantTearing || r.tearing != wantTearing || d.BufferCount < uncappedBuffers {
				t.Errorf("composed %v, uncapped: waitable %v, latency object %#x, tearing %v (renderer %v), want %v, %d buffers",
					composed, waitable, r.lat.handle, tearing, r.tearing, wantTearing, d.BufferCount)
			}
			if f := r.presentFlags(0); (f == presentTearing) != wantTearing || r.presentFlags(1) != 0 {
				t.Errorf("composed %v, uncapped: present flags %#x at once, %#x after a refresh", composed, f, r.presentFlags(1))
			}
			// Frames follow each other at once: changed, unchanged, and of
			// a new size, which resizes the swap chain with its flags.
			for i, s := range []*scene.Scene{
				{Width: 200, Height: 150, Background: scene.Color{R: 255, G: 255, B: 255, A: 255}},
				{Width: 200, Height: 150, Background: scene.Color{R: 255, G: 255, B: 255, A: 255}},
				{Width: 260, Height: 180, Background: bg},
			} {
				if !r.Ready(func() { t.Error("woken: a frame waited") }) {
					t.Fatalf("composed %v, uncapped: frame %d waits", composed, i)
				}
				if err := r.Render(s); err != nil {
					r.Release()
					t.Fatal(err)
				}
				if r.pending || r.refreshing || len(posted) != 0 {
					t.Errorf("composed %v, uncapped, frame %d: pending %v, awaiting a refresh %v, %d posted", composed, i, r.pending, r.refreshing, len(posted))
				}
			}
			if d := desc(r); d.Flags != uint32(r.swapChainFlags()) || d.BufferCount < uncappedBuffers {
				t.Errorf("composed %v, uncapped, resized: flags %#x, want %#x, %d buffers", composed, d.Flags, r.swapChainFlags(), d.BufferCount)
			}
			r.Release()
		}
	}
}

// TestUncappedResizePaced checks that uncapped (MYGO_UNCAPPED), frames
// present paced while the window changes size, as they would without it:
// a frame of a new size without tearing, the others without tearing too,
// each waiting for the composition that shows it, one that changed nothing
// for the next refresh, and none taking long (uncapped.go); and that once
// the swap chain settled, frames present uncapped again. Uncapped frames
// flooding DWM as it resized the window had a present take 2 s on a
// Radeon 780M; WARP, without a display, may not show that, but the frames
// are those of a live resize, the size changing every other frame, and no
// frame may take near as long. Real DXGI takes DXGI_PRESENT_DO_NOT_WAIT,
// and a frame of a new size that it refused for a refresh, as WARP on a
// busy runner may, waits to present (holdFrame) and presents before the
// frame after draws.
func TestUncappedResizePaced(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func(on bool) { pacing.Uncapped = on }(pacing.Uncapped)
	defer func(d time.Duration) { settleDelay = d }(settleDelay)
	var composed atomic.Int32
	defer func(f func(_, _ time.Duration)) { awaitRefresh = f }(awaitRefresh)
	awaitRefresh = func(_, _ time.Duration) { composed.Add(1) }
	// Real DXGI presents, each call's flags and result recorded.
	var calls []presentResult
	defer func(f func(_, _, _ uintptr, _ *presentParameters) uintptr) { presentCall = f }(presentCall)
	present := presentCall
	presentCall = func(sc, sync, flags uintptr, params *presentParameters) uintptr {
		hr := present(sc, sync, flags, params)
		calls = append(calls, presentResult{flags, uint32(hr)})
		return hr
	}
	// A frame waiting to present presents once DXGI takes it, which it
	// does; TestUncappedHeldFrameGivenUp gives one up.
	defer func(d time.Duration) { maxHeld = d }(maxHeld)
	maxHeld = wait.Guard
	pacing.Uncapped = true
	// Frames come far sooner than half of it after the last change, so the
	// swap chain settles only once the size stops changing.
	settleDelay = time.Second
	for _, newRenderer := range []func(uintptr) (*Renderer, error){New, NewComposed} {
		r, err := newRenderer(hiddenWindow(t, 200, 150))
		if err != nil {
			t.Skip("no Direct3D 11:", err)
		}
		uncappedResize(t, r, &composed, &calls)
		r.Release()
	}
}

// presentResult is a call to Present: its flags and what it returned.
type presentResult struct {
	flags uintptr
	hr    uint32
}

// lastPresent returns the last of calls, none for none.
func lastPresent(calls []presentResult) presentResult {
	if len(calls) == 0 {
		return presentResult{}
	}
	return calls[len(calls)-1]
}

// presentedNoWait reports whether the call presented with
// DXGI_PRESENT_DO_NOT_WAIT, and stillDrawing whether DXGI refused it.
func (c presentResult) presentedNoWait() bool {
	return c.flags&presentDoNotWait != 0 && !failed(uintptr(c.hr))
}

func (c presentResult) stillDrawing() bool {
	return c.flags&presentDoNotWait != 0 && c.hr == dxgiErrorWasStillDrawing
}

func uncappedResize(t *testing.T, r *Renderer, composed *atomic.Int32, calls *[]presentResult) {
	t.Helper()
	kind := "window"
	if r.composed {
		kind = "composition"
	}
	posted := make(chan func(), 16)
	r.SetPost(func(f func()) { posted <- f })
	// ready has the next frame wait until it can draw, as the window host
	// does, and reports whether it waited. A frame waiting to present is
	// tried again at each composition, which comes at once here: DXGI
	// takes it long before the guard.
	ready := func() bool {
		t.Helper()
		woke := false
		if r.Ready(func() { woke = true }) {
			return false
		}
		for end := time.Now().Add(wait.Guard); !woke; {
			if time.Now().After(end) {
				t.Fatalf("%s: the frame after one waiting to present never drew (waited %v)", kind, wait.Guard)
			}
			wait.For(t, kind+": the composition a frame waits for", posted)()
		}
		return true
	}
	const tooLong = 200 * time.Millisecond
	colors := []scene.Color{{R: 24, G: 24, B: 27, A: 255}, {R: 250, G: 250, B: 250, A: 255}}
	n := 0
	// frame draws and presents a frame of w×h, which changes what it starts
	// with and the background unless same, and reports whether it waited to
	// start: a swap chain for composition shows no background, and a frame
	// changing it alone would present nothing.
	frame := func(w, h int, same bool) bool {
		t.Helper()
		waited := ready()
		if !same {
			n++
		}
		start := time.Now()
		if err := r.Render(&scene.Scene{Width: w, Height: h, Clear: colors[n%2], Background: colors[n%2]}); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > tooLong {
			t.Errorf("%s: a %dx%d frame took %v, resizing %v", kind, w, h, d, r.resizing)
		}
		return waited
	}
	wantTearing := !r.composed && tearingSupported(r.factory)
	frame(200, 150, false)
	if r.swapChain2 == 0 {
		t.Skip("no IDXGISwapChain2: the swap chain resizes whole at each size")
	}
	for range 3 {
		if frame(200, 150, false) || r.refreshing || r.resizing {
			t.Fatalf("%s: before the resize, a frame waited (refreshing %v, resizing %v)", kind, r.refreshing, r.resizing)
		}
	}
	if (r.presentFlags(0) == presentTearing) != wantTearing {
		t.Fatalf("%s: before the resize, present flags %#x", kind, r.presentFlags(0))
	}
	// A live resize: growing, shrinking within the buffers and growing past
	// them, the size changing every other frame.
	w, h := 200, 150
	held := 0
	for i := range 24 {
		switch {
		case i%6 == 5:
			w, h = w-11, h-5
		default:
			w, h = w+9, h+6
		}
		frame(w, h, false)
		if r.noWaitRefused {
			t.Fatalf("%s: the swap chain took no DXGI_PRESENT_DO_NOT_WAIT", kind)
		}
		if !r.resizing || r.presentFlags(0) != presentDoNotWait || r.pending {
			t.Fatalf("%s: a frame of a new size, %dx%d: resizing %v, present flags %#x, pending %v; want resizing, no tearing, DXGI_PRESENT_DO_NOT_WAIT, none pending",
				kind, w, h, r.resizing, r.presentFlags(0), r.pending)
		}
		info := r.FrameInfo()
		if !strings.Contains(info, "paced while resizing") {
			t.Fatalf("%s: a frame of a new size says %q", kind, info)
		}
		if r.unpresented {
			// DXGI refused it for a refresh: the frame waits to present,
			// the main thread back to the window's messages, and presents
			// before the frame after draws.
			held++
			if last := lastPresent(*calls); !r.refreshing || !last.stillDrawing() || !strings.Contains(info, "presenting once DXGI can") {
				t.Fatalf("%s: a frame of a new size, %dx%d, waiting to present: refreshing %v, last present %#x (flags %#x), says %q; want waiting for the composition, refused with DXGI_PRESENT_DO_NOT_WAIT",
					kind, w, h, r.refreshing, last.hr, last.flags, info)
			}
			ready()
			if last := lastPresent(*calls); r.unpresented || !last.presentedNoWait() {
				t.Fatalf("%s: a frame of a new size, %dx%d, waiting to present: unpresented %v, last present %#x (flags %#x) once the frame after can draw; want presented with DXGI_PRESENT_DO_NOT_WAIT",
					kind, w, h, r.unpresented, last.hr, last.flags)
			}
		} else if last := lastPresent(*calls); r.refreshing || !last.presentedNoWait() {
			t.Fatalf("%s: a frame of a new size, %dx%d: refreshing %v, last present %#x (flags %#x); want no wait, presented with DXGI_PRESENT_DO_NOT_WAIT",
				kind, w, h, r.refreshing, last.hr, last.flags)
		}
		before := composed.Load()
		frame(w, h, false)
		if !r.refreshing {
			t.Fatalf("%s: a %dx%d frame while resizing does not wait for its composition", kind, w, h)
		}
		if r.held {
			// It changes nothing, and so presents nothing.
			if !frame(w, h, true) {
				t.Fatalf("%s: a frame after one presented while resizing did not wait", kind)
			}
			if r.redrew != redrewNothing || !r.refreshing {
				t.Fatalf("%s: a frame that changed nothing while resizing: redrew %v, refreshing %v", kind, r.redrew, r.refreshing)
			}
		}
		ready()
		if composed.Load() == before {
			t.Fatalf("%s: frames while resizing waited for no composition", kind)
		}
	}
	if held > 0 {
		t.Logf("%s: %d of 24 frames of a new size waited to present, DXGI refusing them for a refresh", kind, held)
	}
	// Once the size stopped changing, a frame settles the swap chain, and
	// frames present uncapped again.
	time.Sleep(settleDelay/2 + 10*time.Millisecond)
	frame(w, h, false)
	if r.resizing || r.bw != w || r.bh != h {
		t.Fatalf("%s: settled: resizing %v, buffers %dx%d; want %dx%d", kind, r.resizing, r.bw, r.bh, w, h)
	}
	for range 3 {
		if frame(w, h, false) || r.refreshing {
			t.Fatalf("%s: after the resize, a frame waited (refreshing %v)", kind, r.refreshing)
		}
	}
	if (r.presentFlags(0) == presentTearing) != wantTearing || strings.Contains(r.FrameInfo(), "paced") {
		t.Fatalf("%s: after the resize, present flags %#x, frame %q", kind, r.presentFlags(0), r.FrameInfo())
	}
	if r.noWaitRefused {
		t.Errorf("%s: the swap chain took no DXGI_PRESENT_DO_NOT_WAIT", kind)
	}
}

// TestUncappedResizeWhileDWMStalls checks that uncapped (MYGO_UNCAPPED),
// no frame of a live resize waits long for DWM on the main thread, whose
// messages DWM may wait on, though DWM composes nothing for as long as the
// test runs: a frame of a new size waits for it at most two refreshes
// (waitShown), DwmFlush called on another thread, and the others not at
// all, from the uncapped frames before the resize, through its first frame
// and its last change, to the uncapped frames once the swap chain settled.
// A resize the app made with SetWindowPos had a present take 2 s on a
// Radeon 780M, at its first frame or after its last change, while the
// frame of a new size called DwmFlush on the main thread.
func TestUncappedResizeWhileDWMStalls(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func(on bool) { pacing.Uncapped = on }(pacing.Uncapped)
	defer func(d time.Duration) { settleDelay = d }(settleDelay)
	pacing.Uncapped = true
	settleDelay = 400 * time.Millisecond
	// A frame DXGI refused waits to present for as long as it does here;
	// TestUncappedRefusedThroughSettle gives one up.
	defer func(d time.Duration) { maxHeld = d }(maxHeld)
	maxHeld = wait.Guard
	// A call to DwmFlush under way from another test returns first.
	if done := inFlightFlush(); done != nil {
		wait.For(t, "the window manager", done)
	}
	stalled := make(chan struct{})
	defer func(f func()) {
		close(stalled)
		// The call under way returns before DwmFlush is called again.
		for done := inFlightFlush(); done != nil; done = inFlightFlush() {
			<-done
		}
		dwmFlushCall = f
	}(dwmFlushCall)
	dwmFlushCall = func() { <-stalled }
	// Count the waits of frames of a new size, the only ones with no least.
	var shown atomic.Int32
	defer func(f func(_, _ time.Duration)) { awaitRefresh = f }(awaitRefresh)
	await := awaitRefresh
	awaitRefresh = func(least, most time.Duration) {
		if least == 0 {
			shown.Add(1)
		}
		await(least, most)
	}
	for _, newRenderer := range []func(uintptr) (*Renderer, error){New, NewComposed} {
		r, err := newRenderer(hiddenWindow(t, 200, 150))
		if err != nil {
			t.Skip("no Direct3D 11:", err)
		}
		resizeWhileDWMStalls(t, r, &shown)
		r.Release()
	}
}

// inFlightFlush returns what closes once the call to DwmFlush under way
// returns, nil for none.
func inFlightFlush() <-chan struct{} {
	dwmFlush.Lock()
	defer dwmFlush.Unlock()
	return dwmFlush.done
}

func resizeWhileDWMStalls(t *testing.T, r *Renderer, shown *atomic.Int32) {
	t.Helper()
	kind := "window"
	if r.composed {
		kind = "composition"
	}
	posted := make(chan func(), 16)
	r.SetPost(func(f func()) { posted <- f })
	// Two refreshes, the longest a frame waits, and time for a slow
	// machine to draw, far below the 2 s seen.
	tooLong := 2*r.cost.refreshPeriod() + 200*time.Millisecond
	colors := []scene.Color{{R: 24, G: 24, B: 27, A: 255}, {R: 250, G: 250, B: 250, A: 255}}
	n := 0
	// frame draws a frame of w×h, changing the background, once it can, as
	// the window host has it wait: the waits for a composition end at their
	// bound.
	frame := func(w, h int) {
		t.Helper()
		woke := false
		if !r.Ready(func() { woke = true }) {
			for !woke {
				wait.For(t, kind+": the composition a frame waits for, or its bound", posted)()
			}
		}
		n++
		start := time.Now()
		if err := r.Render(&scene.Scene{Width: w, Height: h, Background: colors[n%2]}); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > tooLong {
			t.Errorf("%s: a %dx%d frame took %v, resizing %v; want at most %v", kind, w, h, d, r.resizing, tooLong)
		}
	}
	frame(200, 150)
	if r.swapChain2 == 0 {
		t.Skip("no IDXGISwapChain2: the swap chain resizes whole at each size")
	}
	for range 10 {
		frame(200, 150) // uncapped, with tearing where the system has it
	}
	// Growing, past the buffers too, and shrinking within them.
	w, h := 200, 150
	for i := range 12 {
		if i%4 == 3 {
			w, h = w-13, h-7
		} else {
			w, h = w+17, h+9
		}
		before := shown.Load()
		frame(w, h)
		// It waits for DWM to show it once presented; one DXGI refused for
		// a refresh, as WARP on a busy runner may, waits to present
		// instead, and the frame after for it (holdFrame).
		want := before + 1
		if r.unpresented {
			want = before
		}
		if !r.resizing || shown.Load() != want {
			t.Fatalf("%s: a %dx%d frame of a new size: resizing %v, waiting to present %v, %d waits for DWM to show it; want resizing, %d",
				kind, w, h, r.resizing, r.unpresented, shown.Load()-before, want-before)
		}
		frame(w, h)
		frame(w, h)
	}
	// The frames after the last change, until one settles the swap chain.
	for end := time.Now().Add(settleDelay/2 + 20*time.Millisecond); time.Now().Before(end); {
		frame(w, h)
	}
	frame(w, h)
	if r.resizing || r.bw != w || r.bh != h {
		t.Fatalf("%s: settled: resizing %v, buffers %dx%d; want %dx%d", kind, r.resizing, r.bw, r.bh, w, h)
	}
	for range 10 {
		frame(w, h)
		if r.refreshing {
			t.Fatalf("%s: after the resize, a frame waits for a composition", kind)
		}
	}
}

// TestUncappedResizePresentRefused checks that uncapped (MYGO_UNCAPPED),
// while the window changes size, no frame waits in Present long on the
// main thread: frames present with DXGI_PRESENT_DO_NOT_WAIT, and one that
// Present refuses for a refresh (DXGI_ERROR_WAS_STILL_DRAWING) is left to
// present after the next composition, the frame after waiting for it, or
// gives way to a frame drawn meanwhile, as one of a new size within
// SetWindowPos (holdFrame). A resize the app made with SetWindowPos every
// 50 ms had a present of a frame redrawn in part take 2 s on a Radeon
// 780M, most likely waiting in Present for DWM to give a buffer back.
// Present here refuses for as long as the test says, as it would while DWM
// is held up, and only then; a frame waits for as long as it refuses
// (TestUncappedHeldFrameGivenUp bounds that). The paced default, and frames
// once the swap chain settled, present without the flag.
func TestUncappedResizePresentRefused(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func(on bool) { pacing.Uncapped = on }(pacing.Uncapped)
	defer func(d time.Duration) { settleDelay = d }(settleDelay)
	// The swap chain settles only once the test stops changing the size.
	settleDelay = time.Second
	defer func(f func(_, _ time.Duration)) { awaitRefresh = f }(awaitRefresh)
	awaitRefresh = func(_, _ time.Duration) {}
	// A hidden window's frame latency object is not signaled: paced frames
	// wait for it no longer than this.
	defer func(ms uintptr) { latencyTimeout = ms }(latencyTimeout)
	latencyTimeout = 20
	// A frame waits to present for as long as Present refuses it.
	defer func(d time.Duration) { maxHeld = d }(maxHeld)
	maxHeld = wait.Guard
	var p refusingPresent
	defer func(f func(_, _, _ uintptr, _ *presentParameters) uintptr) { presentCall = f }(presentCall)
	presentCall = p.call(presentCall)
	for _, uncapped := range []bool{false, true} {
		pacing.Uncapped = uncapped
		for _, newRenderer := range []func(uintptr) (*Renderer, error){New, NewComposed} {
			r, err := newRenderer(hiddenWindow(t, 200, 150))
			if err != nil {
				t.Skip("no Direct3D 11:", err)
			}
			p = refusingPresent{}
			presentRefused(t, r, &p)
			r.Release()
		}
	}
}

// refusingPresent stands for Present: while refuse, it refuses presents
// with DXGI_PRESENT_DO_NOT_WAIT, counting them, and, given stall, waits
// that long in those without it, as DXGI would for DWM, counting them
// (waited); it counts those it took (presented); flags are those of every
// call.
type refusingPresent struct {
	refuse             bool
	stall              time.Duration
	refused, presented int
	waited             int
	flags              []uintptr
}

// call returns what stands for Present, calling present for the calls it
// takes.
func (p *refusingPresent) call(present func(_, _, _ uintptr, _ *presentParameters) uintptr) func(_, _, _ uintptr, _ *presentParameters) uintptr {
	return func(sc, sync, flags uintptr, params *presentParameters) uintptr {
		p.flags = append(p.flags, flags)
		if flags&presentDoNotWait != 0 {
			if p.refuse {
				p.refused++
				return dxgiErrorWasStillDrawing
			}
			// Only the test refuses: real DXGI, which may refuse too on a
			// busy runner (TestUncappedResizePaced), takes the frame as it
			// would without the flag.
			flags &^= presentDoNotWait
		} else if p.refuse && p.stall > 0 {
			p.waited++
			time.Sleep(p.stall)
		}
		p.presented++
		return present(sc, sync, flags, params)
	}
}

// noWait reports whether a call to Present since the from'th had
// DXGI_PRESENT_DO_NOT_WAIT.
func (p *refusingPresent) noWait(from int) bool {
	for _, f := range p.flags[from:] {
		if f&presentDoNotWait != 0 {
			return true
		}
	}
	return false
}

func presentRefused(t *testing.T, r *Renderer, p *refusingPresent) {
	t.Helper()
	kind := "window"
	if r.composed {
		kind = "composition"
	}
	if !r.uncapped {
		kind += ", paced"
	}
	posted := make(chan func(), 16)
	r.SetPost(func(f func()) { posted <- f })
	// A refresh, which Present is tried for, and time for a slow machine
	// to draw, far below the 2 s seen.
	tooLong := r.cost.refreshPeriod() + 200*time.Millisecond
	colors := []scene.Color{{R: 24, G: 24, B: 27, A: 255}, {R: 250, G: 250, B: 250, A: 255}}
	n := 0
	// render draws a frame of w×h, changing what it starts with and the
	// background, at once, as the window host does within SetWindowPos: a
	// swap chain for composition shows no background, and a frame changing
	// it alone would present nothing.
	render := func(w, h int) {
		t.Helper()
		n++
		start := time.Now()
		if err := r.Render(&scene.Scene{Width: w, Height: h, Clear: colors[n%2], Background: colors[n%2]}); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); r.uncapped && d > tooLong {
			t.Errorf("%s: a %dx%d frame took %v, resizing %v; want at most %v", kind, w, h, d, r.resizing, tooLong)
		}
	}
	// ready has the next frame wait until it can draw, as the window host
	// does, running what the renderer posts meanwhile.
	ready := func() {
		t.Helper()
		woke := false
		if r.Ready(func() { woke = true }) {
			return
		}
		for !woke {
			wait.For(t, kind+": what the frame after waits for", posted)()
		}
	}
	frame := func(w, h int) {
		t.Helper()
		ready()
		render(w, h)
	}
	frame(200, 150)
	if r.swapChain2 == 0 {
		t.Skip("no IDXGISwapChain2: the swap chain resizes whole at each size")
	}
	for range 3 {
		frame(200, 150)
	}
	if p.noWait(0) {
		t.Fatalf("%s: before the resize, a present with DXGI_PRESENT_DO_NOT_WAIT: %#x", kind, p.flags)
	}
	w, h := 200, 150
	if !r.uncapped {
		// Paced, a live resize presents as it did, whatever Present says.
		p.refuse = true
		for range 4 {
			w, h = w+17, h+9
			frame(w, h)
			frame(w, h)
		}
		if !r.resizing || p.noWait(0) || p.refused != 0 || r.unpresented {
			t.Fatalf("%s: resizing %v, presents with DXGI_PRESENT_DO_NOT_WAIT %v, %d refused, unpresented %v; want resizing, none",
				kind, r.resizing, p.noWait(0), p.refused, r.unpresented)
		}
		return
	}

	// A frame of a new size, which Present refuses for longer than a
	// refresh, returns to the window's messages within about that, and
	// waits to present.
	p.refuse = true
	w, h = w+17, h+9
	ready()
	from := len(p.flags)
	render(w, h)
	if !r.resizing || !r.unpresented || !r.refreshing || p.refused == 0 || !p.noWait(from) {
		t.Fatalf("%s: a %dx%d frame of a new size Present refused: resizing %v, unpresented %v, refreshing %v, %d refused, flags %#x; want resizing, unpresented, refreshing, refused with DXGI_PRESENT_DO_NOT_WAIT",
			kind, w, h, r.resizing, r.unpresented, r.refreshing, p.refused, p.flags[from:])
	}
	if info := r.FrameInfo(); !strings.Contains(info, "Present still drawing") || !strings.Contains(info, "presenting once DXGI can") {
		t.Errorf("%s: a frame Present refused says %q", kind, info)
	}
	woke := false
	if r.Ready(func() { woke = true }) {
		t.Fatalf("%s: the frame after one waiting to present is ready", kind)
	}
	// Tried again after the composition, it waits again while refused.
	refused := p.refused
	start := time.Now()
	wait.For(t, kind+": the composition the frame waits for", posted)()
	if d := time.Since(start); d > tooLong {
		t.Errorf("%s: trying the frame again took %v; want at most %v", kind, d, tooLong)
	}
	if woke || !r.unpresented || !r.refreshing || p.refused == refused {
		t.Fatalf("%s: tried again while refused: woken %v, unpresented %v, refreshing %v, %d more refused; want waiting, unpresented, refused",
			kind, woke, r.unpresented, r.refreshing, p.refused-refused)
	}
	// Once DXGI takes it, it presents, and the frame after waits for the
	// composition that shows it.
	p.refuse = false
	presented := p.presented
	for !woke {
		wait.For(t, kind+": the frame's present and its composition", posted)()
	}
	if r.unpresented || p.presented != presented+1 {
		t.Fatalf("%s: once taken: unpresented %v, %d presents; want one", kind, r.unpresented, p.presented-presented)
	}

	// A frame not of a new size, refused, gives way to the frame of the
	// next size, drawn whole at once within SetWindowPos, and is never
	// presented.
	p.refuse = true
	frame(w, h)
	if !r.unpresented {
		t.Fatalf("%s: a %dx%d frame Present refused does not wait to present", kind, w, h)
	}
	p.refuse = false
	presented = p.presented
	w, h = w-13, h-7
	render(w, h)
	if r.unpresented || r.redrew != redrewWhole || p.presented != presented+1 {
		t.Fatalf("%s: a %dx%d frame of a new size after one refused: unpresented %v, redrew %v, %d presents; want whole, one",
			kind, w, h, r.unpresented, r.redrew, p.presented-presented)
	}
	ready()
	if p.presented != presented+1 {
		t.Fatalf("%s: the frame refused presented after the one that took its place", kind)
	}

	// The frames after the last change, until one settles the swap chain,
	// then present without the flag.
	for end := time.Now().Add(settleDelay/2 + 20*time.Millisecond); time.Now().Before(end); {
		frame(w, h)
	}
	frame(w, h)
	if r.resizing {
		t.Fatalf("%s: the swap chain did not settle", kind)
	}
	from = len(p.flags)
	for range 3 {
		frame(w, h)
	}
	if p.noWait(from) || r.unpresented || r.noWaitRefused {
		t.Fatalf("%s: settled: presents with DXGI_PRESENT_DO_NOT_WAIT %v, unpresented %v, the flag refused %v; want none",
			kind, p.noWait(from), r.unpresented, r.noWaitRefused)
	}
}

// TestUncappedHeldFrameGivenUp checks that uncapped (MYGO_UNCAPPED), a
// frame that Present goes on refusing while the window changes size, as
// at the end of a resize, is given up on once it waited maxHeld, and is
// never presented, and that frames go on: the frame after, which waits
// for it, draws then, whole; with none waiting, the window is asked for a
// frame (askFrame). Without that bound, a SetWindowPos resize on a Radeon
// 780M held its last frame, and nothing drew again.
// TestUncappedRefusedThroughSettle has Present refuse past the settle.
func TestUncappedHeldFrameGivenUp(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func(on bool) { pacing.Uncapped = on }(pacing.Uncapped)
	defer func(d time.Duration) { settleDelay = d }(settleDelay)
	settleDelay = time.Second
	defer func(d time.Duration) { maxHeld = d }(maxHeld)
	defer func(f func(_, _ time.Duration)) { awaitRefresh = f }(awaitRefresh)
	awaitRefresh = func(_, _ time.Duration) {}
	asked := 0
	defer func(f func(uintptr)) { askFrame = f }(askFrame)
	askFrame = func(uintptr) { asked++ }
	var p refusingPresent
	defer func(f func(_, _, _ uintptr, _ *presentParameters) uintptr) { presentCall = f }(presentCall)
	presentCall = p.call(presentCall)
	pacing.Uncapped = true
	for _, newRenderer := range []func(uintptr) (*Renderer, error){New, NewComposed} {
		r, err := newRenderer(hiddenWindow(t, 200, 150))
		if err != nil {
			t.Skip("no Direct3D 11:", err)
		}
		p = refusingPresent{}
		heldGivenUp(t, r, &p, &asked)
		r.Release()
	}
}

func heldGivenUp(t *testing.T, r *Renderer, p *refusingPresent, asked *int) {
	t.Helper()
	kind := "window"
	if r.composed {
		kind = "composition"
	}
	posted := make(chan func(), 16)
	r.SetPost(func(f func()) { posted <- f })
	colors := []scene.Color{{R: 24, G: 24, B: 27, A: 255}, {R: 250, G: 250, B: 250, A: 255}}
	n := 0
	// render draws a frame of w×h, changing what it starts with and the
	// background, at once.
	render := func(w, h int) {
		t.Helper()
		n++
		if err := r.Render(&scene.Scene{Width: w, Height: h, Clear: colors[n%2], Background: colors[n%2]}); err != nil {
			t.Fatal(err)
		}
	}
	// pump runs what the renderer posts until done.
	pump := func(what string, done func() bool) {
		t.Helper()
		for !done() {
			wait.For(t, kind+": "+what, posted)()
		}
	}
	// waiting has the frame after wait as the window host does, and
	// returns whether it can draw.
	waiting := func() func() bool {
		t.Helper()
		woke := false
		if r.Ready(func() { woke = true }) {
			t.Fatalf("%s: the frame after one waiting to present is ready", kind)
		}
		return func() bool { return woke }
	}
	ready := func() {
		t.Helper()
		woke := false
		if !r.Ready(func() { woke = true }) {
			pump("what the frame after waits for", func() bool { return woke })
		}
	}
	frame := func(w, h int) {
		t.Helper()
		ready()
		render(w, h)
	}
	frame(200, 150)
	if r.swapChain2 == 0 {
		t.Skip("no IDXGISwapChain2: the swap chain resizes whole at each size")
	}
	for range 3 {
		frame(200, 150)
	}
	w, h := 200, 150
	// resizeEnd draws a frame of a new size, which presents, and then one
	// that Present refuses for good, the resize's last.
	resizeEnd := func() {
		t.Helper()
		p.refuse = false
		w, h = w+17, h+9
		frame(w, h)
		p.refuse = true
		frame(w, h)
		if !r.resizing || !r.unpresented {
			t.Fatalf("%s: a %dx%d frame Present refused: resizing %v, unpresented %v; want resizing, waiting to present", kind, w, h, r.resizing, r.unpresented)
		}
	}
	// next draws the frame after one given up on, which Present takes, and
	// checks that it draws whole and presents, and that frames go on.
	next := func(presented int) {
		t.Helper()
		p.refuse = false
		render(w, h)
		if info := r.FrameInfo(); r.redrew != redrewWhole || p.presented != presented+1 || !strings.Contains(info, "after a frame DXGI never took") {
			t.Fatalf("%s: the frame after one given up on: redrew %v, %d presents, says %q; want whole, one, after a frame given up on", kind, r.redrew, p.presented-presented, info)
		}
		if r.stalled || !r.gaveUp || !r.resizing {
			t.Fatalf("%s: the frame after one given up on, taken: stalled %v, gave up %v, resizing %v; want DXGI taking frames, paced until the swap chain settles",
				kind, r.stalled, r.gaveUp, r.resizing)
		}
		for range 3 {
			frame(w, h)
		}
		if p.presented != presented+4 || r.unpresented {
			t.Fatalf("%s: frames after one given up on: %d presents, unpresented %v; want 4", kind, p.presented-presented, r.unpresented)
		}
	}

	// The frame after waits for the frame refused, which is given up on
	// once it waited maxHeld.
	maxHeld = 50 * time.Millisecond
	resizeEnd()
	heldAt, presented, before := r.heldAt, p.presented, *asked
	pump("the frame refused given up on, and the frame after woken", waiting())
	if d := time.Since(heldAt); d < maxHeld || r.unpresented || p.presented != presented || *asked != before+1 {
		t.Fatalf("%s: the frame after one refused woke %v after it, unpresented %v, %d presents, %d frames asked; want after %v, given up on, none, one",
			kind, d, r.unpresented, p.presented-presented, *asked-before, maxHeld)
	}
	next(presented)

	// With no frame waiting for it, the window is asked for one.
	resizeEnd()
	presented, before = p.presented, *asked
	pump("the frame refused given up on, and a frame asked for", func() bool { return *asked != before })
	if r.unpresented || r.refreshing || p.presented != presented {
		t.Fatalf("%s: a frame refused, none waiting: unpresented %v, refreshing %v, %d presents; want given up on, nothing waited for, none",
			kind, r.unpresented, r.refreshing, p.presented-presented)
	}
	if !r.Ready(func() { t.Errorf("%s: the frame asked for waited", kind) }) {
		t.Fatalf("%s: the frame asked for after one given up on waits", kind)
	}
	next(presented)
}

// TestUncappedRefusedThroughSettle checks that uncapped (MYGO_UNCAPPED),
// when Present goes on refusing frames after the window's last change of
// size, past the time the swap chain would settle, no frame waits on the
// main thread for DXGI or DWM, and frames go on: the frames given up on
// (presentHeld) are followed by others, paced, none presenting without
// DXGI_PRESENT_DO_NOT_WAIT; the buffers resize neither to settle nor to
// grow, the frames that would being put off (resizeWaits), until DXGI took
// a frame again and the GPU drew the frames before; then the swap chain
// settles, though the window draws only the frames asked for, and frames
// present uncapped again, with tearing where the system has it. Present
// without the flag and ResizeBuffers wait a second here while Present
// refuses, as on a Radeon 780M, where a resize's last frames were given up
// on and the frame that settled the swap chain then took 994 ms.
func TestUncappedRefusedThroughSettle(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func(on bool) { pacing.Uncapped = on }(pacing.Uncapped)
	defer func(d time.Duration) { settleDelay = d }(settleDelay)
	settleDelay = 400 * time.Millisecond
	defer func(d time.Duration) { maxHeld = d }(maxHeld)
	maxHeld = 50 * time.Millisecond
	defer func(f func(_, _ time.Duration)) { awaitRefresh = f }(awaitRefresh)
	awaitRefresh = func(_, _ time.Duration) {}
	asked := 0
	defer func(f func(uintptr)) { askFrame = f }(askFrame)
	askFrame = func(uintptr) { asked++ }
	var p refusingPresent
	defer func(f func(_, _, _ uintptr, _ *presentParameters) uintptr) { presentCall = f }(presentCall)
	presentCall = p.call(presentCall)
	// ResizeBuffers waits as Present without the flag does while Present
	// refuses frames.
	var b resizeCounts
	defer func(f func(_, _, _, _, _ uintptr) uintptr) { resizeBuffersCall = f }(resizeBuffersCall)
	resizeBuffers := resizeBuffersCall
	resizeBuffersCall = func(sc, n, w, h, flags uintptr) uintptr {
		b.resized++
		if p.refuse {
			b.waited++
			time.Sleep(p.stall)
		}
		return resizeBuffers(sc, n, w, h, flags)
	}
	defer func(f func(_, _ uintptr) bool) { eventDone = f }(eventDone)
	eventDone = func(_, _ uintptr) bool { return !b.gpuBusy }
	pacing.Uncapped = true
	for _, newRenderer := range []func(uintptr) (*Renderer, error){New, NewComposed} {
		r, err := newRenderer(hiddenWindow(t, 200, 150))
		if err != nil {
			t.Skip("no Direct3D 11:", err)
		}
		p = refusingPresent{stall: time.Second}
		b = resizeCounts{}
		refusedThroughSettle(t, r, &p, &b, &asked)
		r.Release()
	}
}

// resizeCounts counts the calls to ResizeBuffers, and those made while
// Present refused frames (waited); gpuBusy has the GPU not done.
type resizeCounts struct {
	resized, waited int
	gpuBusy         bool
}

func refusedThroughSettle(t *testing.T, r *Renderer, p *refusingPresent, b *resizeCounts, asked *int) {
	t.Helper()
	kind := "window"
	if r.composed {
		kind = "composition"
	}
	// A refresh, which Present is tried for, and time for a slow machine
	// to draw, far below the 994 ms seen.
	tooLong := r.cost.refreshPeriod() + 200*time.Millisecond
	posted := make(chan func(), 16)
	r.SetPost(func(f func()) { posted <- f })
	colors := []scene.Color{{R: 24, G: 24, B: 27, A: 255}, {R: 250, G: 250, B: 250, A: 255}}
	n := 0
	// timed runs what the main thread runs, a frame or what the renderer
	// posted, and checks that it returns soon.
	timed := func(what string, f func()) {
		t.Helper()
		start := time.Now()
		f()
		if d := time.Since(start); d > tooLong {
			t.Errorf("%s: %s took %v (resizing %v, gave up %v, stalled %v); want at most %v", kind, what, d, r.resizing, r.gaveUp, r.stalled, tooLong)
		}
	}
	ready := func() {
		t.Helper()
		woke := false
		if r.Ready(func() { woke = true }) {
			return
		}
		for !woke {
			timed("what the renderer posted", wait.For(t, kind+": what the frame after waits for", posted))
		}
	}
	// frame draws a frame of w×h once it can, changing what it starts with
	// and the background, as an animation does.
	frame := func(w, h int) {
		t.Helper()
		ready()
		n++
		timed("a frame", func() {
			if err := r.Render(&scene.Scene{Width: w, Height: h, Clear: colors[n%2], Background: colors[n%2]}); err != nil {
				t.Fatal(err)
			}
		})
	}
	// served draws the frame the window asked for since the last served,
	// as for content that does not change, and reports whether there was
	// one.
	served := *asked
	serve := func(w, h int) bool {
		t.Helper()
		if *asked == served {
			return false
		}
		served = *asked
		frame(w, h)
		return true
	}
	frame(200, 150)
	if r.swapChain2 == 0 {
		t.Skip("no IDXGISwapChain2: the swap chain resizes whole at each size")
	}
	for range 3 {
		frame(200, 150)
	}
	wantTearing := !r.composed && tearingSupported(r.factory)
	// uncappedAgain checks that frames present uncapped, and with tearing
	// where the system has it.
	uncappedAgain := func(w, h int) {
		t.Helper()
		from := len(p.flags)
		for range 5 {
			frame(w, h)
			if r.refreshing || r.unpresented {
				t.Fatalf("%s: after the resize, a frame waits (refreshing %v, unpresented %v)", kind, r.refreshing, r.unpresented)
			}
		}
		if p.noWait(from) || (r.presentFlags(0) == presentTearing) != wantTearing || strings.Contains(r.FrameInfo(), "paced") {
			t.Fatalf("%s: after the resize: DXGI_PRESENT_DO_NOT_WAIT %v, present flags %#x, says %q; want uncapped, tearing %v",
				kind, p.noWait(from), r.presentFlags(0), r.FrameInfo(), wantTearing)
		}
		for _, f := range p.flags[from:] {
			if wantTearing && f != presentTearing {
				t.Fatalf("%s: after the resize, a present with flags %#x; want tearing", kind, f)
			}
		}
	}

	// A live resize, growing past the buffers, whose last frame Present
	// refuses, and every one after, past the time the swap chain would
	// settle: frames go on, put off once they would settle it.
	w, h := 200, 150
	for range 6 {
		w, h = w+23, h+11
		frame(w, h)
		frame(w, h)
	}
	if !r.resizing || r.gaveUp {
		t.Fatalf("%s: resizing %v, gave up %v; want resizing", kind, r.resizing, r.gaveUp)
	}
	p.refuse = true
	bw, bh := r.bw, r.bh
	frames, putOff, after := 0, 0, 0
	for time.Since(r.resizedAt) < settleDelay/2+6*maxHeld {
		frame(w, h)
		frames++
		info := r.FrameInfo()
		if r.putOff {
			putOff++
			if !strings.Contains(info, "put off") {
				t.Errorf("%s: a frame put off says %q", kind, info)
			}
		}
		if strings.Contains(info, "after a frame DXGI never took") {
			after++
		}
	}
	t.Logf("%s: %d frames while Present refused, %d after one given up on, %d put off", kind, frames, after, putOff)
	if !r.resizing || !r.gaveUp || !r.stalled || !r.putOff || r.bw != bw || r.bh != bh || p.waited != 0 || b.waited != 0 {
		t.Fatalf("%s: Present refusing past the settle: resizing %v, gave up %v, stalled %v, put off %v, buffers %dx%d (were %dx%d), %d presents and %d ResizeBuffers waiting; want resizing, given up, stalled, put off, none waiting",
			kind, r.resizing, r.gaveUp, r.stalled, r.putOff, r.bw, r.bh, bw, bh, p.waited, b.waited)
	}
	if after == 0 || frames < 5 {
		t.Fatalf("%s: Present refusing past the settle: %d frames, %d after one given up on; want frames going on", kind, frames, after)
	}

	// Once Present takes frames, the frame put off presents the frame last
	// drawn, while the GPU is not done yet: the frames that would settle the
	// swap chain wait for it, though no frame comes but those asked for.
	p.refuse, b.gpuBusy = false, true
	presented := p.presented
	served = *asked
	frame(w, h) // asked for by the last put off
	if !r.putOff || r.stalled || p.presented != presented+1 || !r.resizing || r.bw != bw {
		t.Fatalf("%s: Present taking frames again: put off %v, stalled %v, %d presents, resizing %v, buffers %dx%d; want the frame last drawn presented, the settle put off",
			kind, r.putOff, r.stalled, p.presented-presented, r.resizing, r.bw, r.bh)
	}
	resized := b.resized
	for range 3 {
		if !serve(w, h) {
			t.Fatalf("%s: no frame asked for after one put off", kind)
		}
		if !r.putOff || b.resized != resized || !r.resizing || p.presented != presented+1 {
			t.Fatalf("%s: a frame while the GPU is not done: put off %v, %d ResizeBuffers, resizing %v, %d presents; want put off, none",
				kind, r.putOff, b.resized-resized, r.resizing, p.presented-presented-1)
		}
	}
	// Once it is, the frame asked for settles the swap chain, and presents
	// paced, with DXGI_PRESENT_DO_NOT_WAIT, which Present takes at once:
	// frames present uncapped from then on.
	b.gpuBusy = false
	from := len(p.flags)
	if !serve(w, h) {
		t.Fatalf("%s: no frame asked for after one put off", kind)
	}
	if info := r.FrameInfo(); r.resizing || r.bw != w || r.bh != h || b.resized != resized+1 || r.gaveUp || r.unpresented || !p.noWait(from) || !strings.Contains(info, "paced until DXGI takes a frame at once") {
		t.Fatalf("%s: settling: resizing %v, buffers %dx%d, %d ResizeBuffers, gave up %v, unpresented %v, DXGI_PRESENT_DO_NOT_WAIT %v, says %q; want settled, %dx%d, one, presented with the flag",
			kind, r.resizing, r.bw, r.bh, b.resized-resized, r.gaveUp, r.unpresented, p.noWait(from), info, w, h)
	}
	uncappedAgain(w, h)

	// Another resize whose last frame Present refuses for good: a frame of a
	// size the buffers do not hold is put off too, the window showing the
	// last frame meanwhile, and draws once DXGI takes a frame again; the
	// settle timer's frame then settles the swap chain.
	w, h = w+5, h+3
	frame(w, h)
	p.refuse = true
	for !r.gaveUp {
		frame(w, h)
	}
	bw, bh = r.bw, r.bh
	w2, h2 := bw+40, bh+20
	frame(w2, h2)
	if !r.putOff || r.w != w || r.h != h || r.bw != bw || r.bh != bh || b.waited != 0 || p.waited != 0 {
		t.Fatalf("%s: a %dx%d frame past the %dx%d buffers while Present refuses: put off %v, showing %dx%d, buffers %dx%d, %d ResizeBuffers and %d presents waiting; want put off, showing %dx%d, none waiting",
			kind, w2, h2, bw, bh, r.putOff, r.w, r.h, r.bw, r.bh, b.waited, p.waited, w, h)
	}
	p.refuse = false
	presented = p.presented
	frame(w2, h2)
	if !r.putOff || r.stalled || p.presented != presented+1 {
		t.Fatalf("%s: a %dx%d frame past the buffers once Present takes frames: put off %v, stalled %v, %d presents; want the frame last drawn presented",
			kind, w2, h2, r.putOff, r.stalled, p.presented-presented)
	}
	frame(w2, h2)
	if r.putOff || r.w != w2 || r.h != h2 || r.bw < w2 || r.bh < h2 || !r.resizing || !r.gaveUp {
		t.Fatalf("%s: a %dx%d frame past the buffers once DXGI took one: put off %v, showing %dx%d in %dx%d buffers, resizing %v, gave up %v; want drawn, resizing, paced",
			kind, w2, h2, r.putOff, r.w, r.h, r.bw, r.bh, r.resizing, r.gaveUp)
	}
	time.Sleep(settleDelay/2 - time.Since(r.resizedAt) + 10*time.Millisecond)
	frame(w2, h2) // the settle timer's
	if r.resizing || r.bw != w2 || r.bh != h2 || r.gaveUp {
		t.Fatalf("%s: the settle timer's frame: resizing %v, buffers %dx%d, gave up %v; want settled, %dx%d",
			kind, r.resizing, r.bw, r.bh, r.gaveUp, w2, h2)
	}
	uncappedAgain(w2, h2)
}
