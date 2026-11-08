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
// present with sync interval 0 all the same. A frame of a new size still
// waits for DWM to show it (Render), as a live resize needs.
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
// tearing's for one at once (sync 0), where the swap chain allows it.
func (r *Renderer) presentFlags(sync uintptr) uintptr {
	if sync == 0 && r.tearing {
		return presentTearing
	}
	return 0
}
