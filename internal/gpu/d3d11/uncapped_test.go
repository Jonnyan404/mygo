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
// frame may take near as long.
func TestUncappedResizePaced(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func(on bool) { pacing.Uncapped = on }(pacing.Uncapped)
	defer func(d time.Duration) { settleDelay = d }(settleDelay)
	var composed atomic.Int32
	defer func(f func(_, _ time.Duration)) { awaitRefresh = f }(awaitRefresh)
	awaitRefresh = func(_, _ time.Duration) { composed.Add(1) }
	pacing.Uncapped = true
	// Frames come far sooner than half of it after the last change, so the
	// swap chain settles only once the size stops changing.
	settleDelay = time.Second
	for _, newRenderer := range []func(uintptr) (*Renderer, error){New, NewComposed} {
		r, err := newRenderer(hiddenWindow(t, 200, 150))
		if err != nil {
			t.Skip("no Direct3D 11:", err)
		}
		uncappedResize(t, r, &composed)
		r.Release()
	}
}

func uncappedResize(t *testing.T, r *Renderer, composed *atomic.Int32) {
	t.Helper()
	kind := "window"
	if r.composed {
		kind = "composition"
	}
	posted := make(chan func(), 16)
	r.SetPost(func(f func()) { posted <- f })
	// ready has the next frame wait until it can draw, as the window host
	// does, and reports whether it waited.
	ready := func() bool {
		t.Helper()
		woke := false
		if r.Ready(func() { woke = true }) {
			return false
		}
		for !woke {
			wait.For(t, kind+": the composition a frame waits for", posted)()
		}
		return true
	}
	const tooLong = 200 * time.Millisecond
	colors := []scene.Color{{R: 24, G: 24, B: 27, A: 255}, {R: 250, G: 250, B: 250, A: 255}}
	n := 0
	// frame draws and presents a frame of w×h, which changes the
	// background unless same, and reports whether it waited to start.
	frame := func(w, h int, same bool) bool {
		t.Helper()
		waited := ready()
		if !same {
			n++
		}
		start := time.Now()
		if err := r.Render(&scene.Scene{Width: w, Height: h, Background: colors[n%2]}); err != nil {
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
	for i := range 24 {
		switch {
		case i%6 == 5:
			w, h = w-11, h-5
		default:
			w, h = w+9, h+6
		}
		frame(w, h, false)
		if !r.resizing || r.presentFlags(0) != presentDoNotWait || r.pending || r.refreshing || r.unpresented {
			t.Fatalf("%s: a frame of a new size, %dx%d: resizing %v, present flags %#x, pending %v, refreshing %v, unpresented %v; want resizing, no tearing, DXGI_PRESENT_DO_NOT_WAIT, no wait",
				kind, w, h, r.resizing, r.presentFlags(0), r.pending, r.refreshing, r.unpresented)
		}
		if !strings.Contains(r.FrameInfo(), "paced while resizing") {
			t.Fatalf("%s: a frame of a new size says %q", kind, r.FrameInfo())
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
		if !r.resizing || shown.Load() != before+1 {
			t.Fatalf("%s: a %dx%d frame of a new size: resizing %v, %d waits for DWM to show it; want resizing, one",
				kind, w, h, r.resizing, shown.Load()-before)
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
// is held up. The paced default, and frames once the swap chain settled,
// present without the flag.
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
	var p refusingPresent
	defer func(f func(_, _, _ uintptr, _ *presentParameters) uintptr) { presentCall = f }(presentCall)
	present := presentCall
	presentCall = func(sc, sync, flags uintptr, params *presentParameters) uintptr {
		p.flags = append(p.flags, flags)
		if p.refuse && flags&presentDoNotWait != 0 {
			p.refused++
			return dxgiErrorWasStillDrawing
		}
		p.presented++
		return present(sc, sync, flags, params)
	}
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
// with DXGI_PRESENT_DO_NOT_WAIT, counting them, and it counts those it
// took (presented); flags are those of every call.
type refusingPresent struct {
	refuse             bool
	refused, presented int
	flags              []uintptr
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
	// render draws a frame of w×h, changing the background, at once, as
	// the window host does within SetWindowPos.
	render := func(w, h int) {
		t.Helper()
		n++
		start := time.Now()
		if err := r.Render(&scene.Scene{Width: w, Height: h, Background: colors[n%2]}); err != nil {
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
