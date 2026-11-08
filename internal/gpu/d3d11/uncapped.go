//go:build windows

package d3d11

import "unsafe"

// Uncapped (MYGO_UNCAPPED, package pacing), a frame presents as soon as it
// is drawn, and the next draws at once: the swap chain is made without the
// frame latency object, so that no frame waits for the screen to have room
// (latency.go), and every frame presents with sync interval 0, none waiting
// for DWM to compose the last (waitComposed) nor, when it changed nothing,
// for the next refresh (waitRefresh). Where the system shows frames torn
// (DXGI_FEATURE_PRESENT_ALLOW_TEARING, Windows 10 1703 on with a driver
// that can), the window's swap chain allows tearing and its frames present
// with it, flipping to the screen mid-refresh once DWM gives the window the
// display (independent flip, as a borderless window covering the screen
// has); a window DWM composes still shows the newest frame at each refresh.
// A swap chain for composition (NewComposed) has no tearing: its frames
// present with sync interval 0 all the same.
//
// While the window changes size, from its first change until the swap
// chain settles (resize, settleDelay), frames present paced
// (presentsUncapped), as without MYGO_UNCAPPED: none with tearing, a frame
// of a new size waiting for DWM to show it (Render, waitShown), the others
// each waiting for the composition that shows it (waitComposed), and one
// that changed nothing for the next refresh (waitRefresh). Uncapped, a live
// resize had one present take 2 s, on a Radeon 780M and a 59 Hz display,
// with the window resized every 50 ms while it animated, and frames then
// came at 92 to 95 a second, about two a composition besides the resize's,
// where paced frames took no more than a refresh. The resize's buffers are
// larger than the window and shown clipped, so DWM composes the window and
// flips none of its frames to the screen torn: frames presented faster than
// DWM composes only queue for it, filling the buffers, while DWM resizes the
// window, and the frame of the next size, which waits for DWM to show it,
// waits behind them. Paced, at most one frame a composition reaches DWM.
// Before ResizeBuffers, no view of the buffers stays bound (unbindBuffers),
// as DXGI asks.
//
// Unlike a paced frame of a new size, which calls DwmFlush on the main
// thread and returns once DWM composed, an uncapped one waits for DWM at
// most two refreshes, DwmFlush called on another thread (awaitRefresh,
// shared with waitComposed's): every wait of an uncapped frame on DWM is
// bounded. With that wait unbounded, a resize the app made with
// SetWindowPos every 50 ms still had one present take 2.0 s, at its first
// frame or after its last change, on the Radeon 780M, though its other
// frames presented paced, while the same resize without MYGO_UNCAPPED,
// and a drag of the window's edge uncapped, had none. The frame of a new size draws
// within SetWindowPos (WM_SIZE), and its DwmFlush was the only wait of an
// uncapped frame with no bound: it returns at DWM's next composition,
// which frames flipped to the screen torn, as the uncapped ones before the
// resize, do not bring about, and DWM may itself wait on the window's
// thread while the window changes size; 2 s exactly looks like the end of
// such a wait. A frame that waits for DWM no longer than two refreshes,
// and then returns to the window's messages, leaves DWM nothing to wait
// on, and a composition missed costs it two refreshes.
//
// With two buffers, a frame presented with sync interval 0 that DWM has not
// shown yet holds one and the screen the other, and the next frame waits
// for the screen to give its buffer back, at the next refresh: the swap
// chain has three (uncappedBuffers), so that a frame replaces the one
// queued, which DWM drops. The device's frame latency, one frame queued at
// most (init), still keeps the main thread from running ahead of the GPU:
// Present blocks for it, and the GPU's pace is the frames'.
const (
	// swapChainTearing is DXGI_SWAP_CHAIN_FLAG_ALLOW_TEARING, and
	// presentTearing DXGI_PRESENT_ALLOW_TEARING.
	swapChainTearing = 0x800
	presentTearing   = 0x200
	// factory5CheckFeatureSupport is IDXGIFactory5::CheckFeatureSupport,
	// and featureTearing DXGI_FEATURE_PRESENT_ALLOW_TEARING.
	factory5CheckFeatureSupport = 28
	featureTearing              = 0
	// uncappedBuffers is how many buffers an uncapped swap chain has at
	// least.
	uncappedBuffers = 3
)

var iidIDXGIFactory5 = guid{0x7632e1f5, 0xee65, 0x4dca, [8]byte{0x87, 0xfd, 0x84, 0xcd, 0x75, 0xf8, 0x83, 0x8d}}

// tearingSupported reports whether the system shows frames presented with
// tearing, as the factory's IDXGIFactory5 (DXGI 1.5) tells.
func tearingSupported(factory uintptr) bool {
	var f5 uintptr
	if failed(call(factory, 0, uintptr(unsafe.Pointer(&iidIDXGIFactory5)), uintptr(unsafe.Pointer(&f5)))) {
		return false
	}
	defer free(&f5)
	var allow int32 // BOOL
	if failed(call(f5, factory5CheckFeatureSupport, featureTearing, uintptr(unsafe.Pointer(&allow)), unsafe.Sizeof(allow))) {
		return false
	}
	return allow != 0
}

// swapChainFlags returns the flags the swap chain is made and resized
// with: the frame latency object's, or uncapped, tearing's where it
// presents with tearing.
func (r *Renderer) swapChainFlags() uint32 {
	switch {
	case !r.uncapped:
		return swapChainWaitable
	case r.tearing:
		return swapChainTearing
	}
	return 0
}

// presentFlags returns the flags of a present after sync vertical blanks:
// tearing's for one at once (sync 0), where the swap chain allows it and
// frames present uncapped.
func (r *Renderer) presentFlags(sync uintptr) uintptr {
	if sync == 0 && r.tearing && r.presentsUncapped() {
		return presentTearing
	}
	return 0
}

// presentsUncapped reports whether frames present without waiting for the
// display: uncapped (MYGO_UNCAPPED), but while the window changes size,
// when they present paced.
func (r *Renderer) presentsUncapped() bool {
	return r.uncapped && !r.resizing
}

// unbindBuffers unbinds the render target views of the swap chain's
// buffers from the device context and submits the commands drawing into
// them, before ResizeBuffers, which needs no reference to the buffers
// left: a view bound to the context is one. The context waits for the GPU
// to be done with them by itself.
func (r *Renderer) unbindBuffers() {
	call(r.ctx, ctxOMSetRenderTargets, 0, 0, 0)
	r.drawing = 0
	call(r.ctx, ctxFlush)
}
