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
		if !r.resizing || r.presentFlags(0) != 0 || r.pending || r.refreshing {
			t.Fatalf("%s: a frame of a new size, %dx%d: resizing %v, present flags %#x, pending %v, refreshing %v; want resizing, no tearing, no wait",
				kind, w, h, r.resizing, r.presentFlags(0), r.pending, r.refreshing)
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
}
