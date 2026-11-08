//go:build windows

package d3d11

import (
	"runtime"
	"testing"
	"unsafe"

	"github.com/egoist/mygo/internal/pacing"
	"github.com/egoist/mygo/internal/scene"
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
