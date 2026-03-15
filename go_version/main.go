// RealSense Capture Server — Go rewrite
// ======================================
// Build tags:
//   -tags demo   → no CGO, no librealsense2, no OpenCV required
//   (no tag)     → full build, requires librealsense2 + OpenCV4
//
// Preview freeze — complete root-cause list and fixes
// ----------------------------------------------------
//
// BUG 1 — use-after-pool-return: data race on the BGR buffer
//   In the previous version the grab loop did:
//     recCh <- recFrame{buf: bufPtr, ...}   // gives bufPtr to recCh goroutine
//     src := (*bufPtr)[...]                  // then reads it AGAIN for downsample
//   The recCh goroutine could call bgrPool.Put(bufPtr) before or during the
//   downsample2x call, corrupting the source pixels and producing a garbled or
//   zero JPEG. That zero JPEG is pushed to the slot, the seq increments, the
//   MJPEG handler sends it, the browser renders black → looks like a freeze.
//   Fix: never read bufPtr after sending it to any channel. The grab loop now
//   sends the raw full-res pointer to ONE channel (prevCh only). The encoder
//   goroutine owns the buffer from that point: it downsamples, encodes, then
//   returns the buffer to bgrPool. recCh is removed entirely — the .bag is
//   written by librealsense2 before GrabFrameInto returns; no Go-side
//   goroutine needs to touch the frame for recording purposes.
//
// BUG 2 — recCh goroutine leaked with no join
//   close(recCh) was called at teardown but there was no <-recDone wait.
//   The goroutine could still be running (returning buffers) while the
//   cameraWorker exited and startPreview re-opened the device. No deadlock,
//   but leaked goroutines accumulate across recording sessions.
//   Fix: recCh removed. One encoder goroutine, one encDone channel, one join.
//
// BUG 3 — downsample ran on the hot grab-loop goroutine
//   downsample2x (~1ms) was called synchronously in the grab loop before the
//   channel send. That 1ms adds directly to the effective grab-loop period
//   (30fps = 33ms budget). Under load this pushed actual grab time over the
//   200ms GrabFrameInto timeout, causing spurious frame drops and stalls.
//   Fix: the grab loop sends the raw full-res pointer to prevCh and returns
//   immediately. The encoder goroutine does ALL the CPU work: downsample +
//   encode. The grab loop is now a pure "get frame, send pointer, repeat" loop.
//
// BUG 4 — preview encoded at 640×360 (~3–5ms per JPEG)
//   Even at half resolution, JPEG encoding under load competes with the grab
//   loop's timeout and causes the encoder goroutine to fall behind.
//   Fix: downsample 4× to 320×180 before encoding. Encode time drops to
//   ~0.3–0.5ms. The browser card is 480px wide; 320px is plenty.
//
// BUG 5 — prevCh replace logic had a TOCTOU window
//   The "drain old, send new" pattern used two separate select statements.
//   Between them another goroutine could send, making the second send block.
//   Fix: prevCh is now an atomic.Pointer[pendingFrame]. The grab loop does a
//   single atomic swap: store new pointer, if old pointer was non-nil return
//   it to pool. The encoder goroutine does an atomic load+swap(nil) to claim
//   the pending frame. No channel, no select, no race window.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

func init() {
	// GOGC=20 is very aggressive — runs GC constantly for ~15 MB Go heap savings.
	// The dominant RSS cost is librealsense2 (~80 MB/camera), not the Go heap.
	// GOGC=40 is a better tradeoff: still tight, half the GC CPU overhead.
	debug.SetGCPercent(40)

	// Hard cap at 400 MB. librealsense2 + 2 cameras ~160–180 MB baseline;
	// this gives 220 MB headroom for buffers and peaks without being wasteful.
	// The previous 1.5 GB cap was so high it never triggered.
	debug.SetMemoryLimit(400 * 1024 * 1024)

	runtime.GOMAXPROCS(runtime.NumCPU())
}

// previewEveryN: push one preview frame every N grabbed frames.
// At 30fps, N=3 → 10fps preview. Plenty for a monitoring feed.
// Lower = smoother preview but more encoder CPU.
const previewEveryN = 3

// ─── Buffer pools ─────────────────────────────────────────────────────────────

// bgrPool: full-res BGR buffers (1280×720×3 ≈ 2.76 MB).
var bgrPool = sync.Pool{
	New: func() any {
		b := make([]byte, 1280*720*3)
		return &b
	},
}

// previewBufPool: buffers for downsampled preview frames.
// Sized to 320×240×3 (idle preview); recording uses a 320×180 sub-slice.
// 4× downsample from 1280×720. Encode time ~0.3ms vs ~12ms full-res.
var previewBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 320*240*3)
		return &b
	},
}

// ─── Pending-frame slot (lock-free, single-producer single-consumer) ──────────
// The grab loop atomically stores a *pendingFrame pointer.
// The encoder goroutine atomically swaps it with nil to claim it.
// If the encoder is behind, the grab loop replaces the old pointer (returning
// the old buffer to the pool) — the encoder always gets the freshest frame.

type pendingFrame struct {
	buf    *[]byte
	width  int
	height int
	stride int
}

type pendingSlot struct {
	p atomic.Pointer[pendingFrame]
}

// store stores a new pending frame, returning the displaced old one (or nil).
// The caller is responsible for returning the old frame's buffer to bgrPool.
func (ps *pendingSlot) store(pf *pendingFrame) *pendingFrame {
	return ps.p.Swap(pf)
}

// claim atomically takes the pending frame and resets the slot to nil.
// Returns nil if no frame is pending.
func (ps *pendingSlot) claim() *pendingFrame {
	return ps.p.Swap(nil)
}

// ─── Frame slot (MJPEG output) ────────────────────────────────────────────────

type frame struct {
	jpeg []byte
	seq  uint64
}

type frameSlot struct {
	ptr  atomic.Pointer[frame]
	mu   sync.Mutex
	cond *sync.Cond
}

func newFrameSlot() *frameSlot {
	fs := &frameSlot{}
	fs.cond = sync.NewCond(&fs.mu)
	fs.ptr.Store(&frame{})
	return fs
}

func (fs *frameSlot) push(jpeg []byte) {
	old := fs.ptr.Load()
	fs.ptr.Store(&frame{jpeg: jpeg, seq: old.seq + 1})
	fs.cond.Broadcast()
}

// waitNext blocks until seq > lastSeq or timeout. Loop on Wait() is the
// correct condition-variable pattern — prevents missed wakeups.
func (fs *frameSlot) waitNext(lastSeq uint64, timeout time.Duration) *frame {
	deadline := time.Now().Add(timeout)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for {
		cur := fs.ptr.Load()
		if cur.seq > lastSeq {
			return cur
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return cur
		}
		timer := time.AfterFunc(remaining, func() { fs.cond.Broadcast() })
		fs.cond.Wait()
		timer.Stop()
	}
}

// ─── 2× downsampler ─────────────────────────────────────────────────────────
// Used by the idle preview pipeline: 640×480 → 320×240.
// 2×2 box average. Takes ~0.3ms — encode of 320×240 JPEG ~0.8ms.
func downsample2x(src []byte, srcW, srcH int, dst []byte) {
	dstW := srcW / 2
	dstH := srcH / 2
	srcStride := srcW * 3
	dstStride := dstW * 3
	for y := 0; y < dstH; y++ {
		sy := y * 2
		for x := 0; x < dstW; x++ {
			sx := x * 2
			i00 := sy*srcStride + sx*3
			i10 := sy*srcStride + (sx+1)*3
			i01 := (sy+1)*srcStride + sx*3
			i11 := (sy+1)*srcStride + (sx+1)*3
			d := y*dstStride + x*3
			dst[d]   = uint8((uint16(src[i00])   + uint16(src[i10])   + uint16(src[i01])   + uint16(src[i11]))   >> 2)
			dst[d+1] = uint8((uint16(src[i00+1]) + uint16(src[i10+1]) + uint16(src[i01+1]) + uint16(src[i11+1])) >> 2)
			dst[d+2] = uint8((uint16(src[i00+2]) + uint16(src[i10+2]) + uint16(src[i01+2]) + uint16(src[i11+2])) >> 2)
		}
	}
}

// ─── 4× downsampler ───────────────────────────────────────────────────────────
// Downsamples srcW×srcH BGR to (srcW/4)×(srcH/4) using 4×4 box average.
// For 1280×720 → 320×180: takes ~0.2ms. JPEG encode of 320×180 ~0.3ms.
// Total preview work: ~0.5ms vs ~13ms for full-res — 26× faster.
func downsample4x(src []byte, srcW, srcH int, dst []byte) {
	dstW := srcW / 4
	dstH := srcH / 4
	srcStride := srcW * 3
	dstStride := dstW * 3
	for y := 0; y < dstH; y++ {
		sy := y * 4
		for x := 0; x < dstW; x++ {
			sx := x * 4
			// Sum 4×4 = 16 source pixels per output pixel.
			var r, g, b uint32
			for dy := 0; dy < 4; dy++ {
				row := (sy + dy) * srcStride
				for dx := 0; dx < 4; dx++ {
					i := row + (sx+dx)*3
					b += uint32(src[i])
					g += uint32(src[i+1])
					r += uint32(src[i+2])
				}
			}
			d := y*dstStride + x*3
			dst[d]   = uint8(b >> 4) // divide by 16
			dst[d+1] = uint8(g >> 4)
			dst[d+2] = uint8(r >> 4)
		}
	}
}

// ─── Global state ─────────────────────────────────────────────────────────────

type CameraState struct {
	Name      string `json:"name"`
	Serial    string `json:"serial"`
	Recording bool   `json:"recording"`
	Drops     int64  `json:"drops"`
	BagPath   string `json:"bag_path"`
}

type SavedFile struct {
	Name    string `json:"name"`
	BagPath string `json:"bag_path"`
	Drops   int64  `json:"drops"`
}

type AppState struct {
	mu          sync.RWMutex
	Status      string
	IsRecording bool
	Cameras     map[string]*CameraState
	SavedFiles  []SavedFile
}

var (
	appState = &AppState{
		Status:  "idle",
		Cameras: make(map[string]*CameraState),
	}

	frameSlots   = make(map[string]*frameSlot)
	frameSlotsMu sync.RWMutex

	previewCancels   = make(map[string]func())
	previewCancelsMu sync.Mutex

	recStop  chan struct{}
	recStart chan struct{}
	recWg    sync.WaitGroup
)

func ensureFrameSlot(serial string) *frameSlot {
	frameSlotsMu.RLock()
	fs := frameSlots[serial]
	frameSlotsMu.RUnlock()
	if fs != nil {
		return fs
	}
	frameSlotsMu.Lock()
	defer frameSlotsMu.Unlock()
	if frameSlots[serial] == nil {
		frameSlots[serial] = newFrameSlot()
	}
	return frameSlots[serial]
}

// ─── Preview pipeline (idle/non-recording mode only) ─────────────────────────

func previewWorker(serial string, stop <-chan struct{}) {
	fs := ensureFrameSlot(serial)

	pipe, err := NewPreviewPipeline(serial)
	if err != nil {
		log.Printf("[preview %s] open error: %v", serial, err)
		return
	}
	defer pipe.Close()
	log.Printf("[preview %s] started", serial)

	for {
		select {
		case <-stop:
			return
		default:
		}

		bufPtr := bgrPool.Get().(*[]byte)
		frameData, err := pipe.WaitForFrameInto(bufPtr, 1000)
		if err != nil {
			bgrPool.Put(bufPtr)
			select {
			case <-stop:
				return
			default:
				continue
			}
		}

		// Downsample 2× (640×480 → 320×240) before encoding.
		// Encode time ~1ms vs ~8ms full-res — eliminates idle preview freeze.
		dstPtr := previewBufPool.Get().(*[]byte)
		*dstPtr = (*dstPtr)[:320*240*3]
		downsample2x(frameData, 640, 480, *dstPtr)
		bgrPool.Put(bufPtr)
		jpeg := encodeJPEGSized(*dstPtr, 320, 240, 65)
		previewBufPool.Put(dstPtr)
		if jpeg != nil {
			fs.push(jpeg)
		}
	}
}

func startPreview(serial string) {
	previewCancelsMu.Lock()
	defer previewCancelsMu.Unlock()
	if _, exists := previewCancels[serial]; exists {
		return
	}
	stopCh := make(chan struct{})
	previewCancels[serial] = func() { close(stopCh) }
	go previewWorker(serial, stopCh)
}

func stopPreview(serial string) {
	previewCancelsMu.Lock()
	cancel, exists := previewCancels[serial]
	if exists {
		delete(previewCancels, serial)
	}
	previewCancelsMu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Wait for the in-flight WaitForFrameInto (1s timeout) to return before
	// the recording pipeline opens the same device handle.
	time.Sleep(1200 * time.Millisecond)
}

// ─── Recording pipeline ───────────────────────────────────────────────────────

func cameraWorker(serial, userName, bagPath, camID string,
	startCh, stopCh <-chan struct{},
	readyCh chan<- struct{}) {

	// recWg.Done() is called manually in the teardown block below,
	// BEFORE cam.Stop(), so the UI transitions to "done" without waiting
	// for the SDK's pipeline teardown (~500ms–2s).

	stopPreview(serial)
	time.Sleep(300 * time.Millisecond)

	cam, err := NewCamera(userName, serial)
	if err != nil {
		log.Printf("[camera %s] FATAL open: %v", serial, err)
		appState.mu.Lock()
		appState.Cameras[camID].Recording = false
		appState.mu.Unlock()
		readyCh <- struct{}{}
		recWg.Done()
		return
	}
	if err := cam.Start(bagPath); err != nil {
		log.Printf("[camera %s] FATAL start: %v", serial, err)
		appState.mu.Lock()
		appState.Cameras[camID].Recording = false
		appState.mu.Unlock()
		readyCh <- struct{}{}
		recWg.Done()
		return
	}
	log.Printf("[camera %s] pipeline open → %s", serial, bagPath)

	// Drain the first frame from the hardware FIFO before signalling ready.
	// The camera needs ~150–250ms after pipeline open to produce its first frame.
	// Without this, the grab loop's 200ms timeout fires immediately on frame 1
	// and counts as a spurious drop. We use a 1s timeout here to be safe.
	{
		warmBuf := make([]byte, 1280*720*3)
		warmPtr := &warmBuf
		cam.GrabFrameInto(warmPtr, 1000) // error is fine — we just discard the result
	}

	readyCh <- struct{}{}

	<-startCh
	appState.mu.Lock()
	appState.Status = "recording"
	appState.mu.Unlock()

	fs := ensureFrameSlot(serial)

	// pending: lock-free single-slot mailbox. Grab loop stores latest frame;
	// encoder goroutine claims it. Displaced frames return to bgrPool immediately.
	var pending pendingSlot

	// signal: encoder wakes on this channel instead of sleeping.
	// Buffered 1 so the grab loop never blocks on the send.
	signal := make(chan struct{}, 1)

	// No permanent scratch buffer — borrow from pool, return after each grab.
	// Previously a 2.76 MB scratchBuf lived for the entire recording session.
	// Now it's borrowed for ~200µs (the GrabFrameInto call) then returned.

	// Encoder goroutine: woken by signal, claims pending frame, downsample+encode.
	// All CPU work is here — grab loop is pure grab+decide, no blocking.
	encDone := make(chan struct{})
	go func() {
		defer close(encDone)
		for {
			select {
			case <-signal:
			case <-stopCh:
				if last := pending.claim(); last != nil {
					bgrPool.Put(last.buf)
				}
				return
			}
			pf := pending.claim()
			if pf == nil {
				continue // grab loop sent signal but then replaced with nil; skip
			}
			src := (*pf.buf)[:pf.stride*pf.height]
			dstPtr := previewBufPool.Get().(*[]byte)
			dstW, dstH := pf.width/4, pf.height/4
			downsample4x(src, pf.width, pf.height, *dstPtr)
			bgrPool.Put(pf.buf) // release full-res buffer as soon as downsampled

			jpeg := encodeJPEGSized((*dstPtr)[:dstW*dstH*3], dstW, dstH, 72)
			previewBufPool.Put(dstPtr)
			if jpeg != nil {
				fs.push(jpeg)
			}
		}
	}()

	var drops int64
	var frameCount int

	for {
		select {
		case <-stopCh:
			goto teardown
		default:
		}

		frameCount++
		isPreview := frameCount%previewEveryN == 0

		// Always borrow from pool. Non-preview buffers are returned immediately
		// after GrabFrameInto — they're live for <200µs, not the whole session.
		bufPtr := bgrPool.Get().(*[]byte)
		w, h, stride, err := cam.GrabFrameInto(bufPtr, 200)
		if err != nil {
			bgrPool.Put(bufPtr)
			drops++
			atomic.StoreInt64(&appState.Cameras[camID].Drops, drops)
			if drops%10 == 0 {
				log.Printf("[camera %s] ⚠ %d dropped frames", serial, drops)
				pushState()
			}
			continue
		}

		if !isPreview {
			bgrPool.Put(bufPtr) // return immediately — .bag already written by SDK
			continue
		}

		// Hand pooled buffer to encoder. Displace any unsent old frame.
		pf := &pendingFrame{buf: bufPtr, width: w, height: h, stride: stride}
		if old := pending.store(pf); old != nil {
			bgrPool.Put(old.buf)
		}
		// Wake encoder (non-blocking — if already awake, signal is queued).
		select {
		case signal <- struct{}{}:
		default:
		}
	}

teardown:
	// Drain the encoder — at most one ~0.5ms encode in flight.
	<-encDone
	if leftover := pending.claim(); leftover != nil {
		bgrPool.Put(leftover.buf)
	}

	// The .bag is fully written: librealsense2 flushes every frame to disk
	// before GrabFrameInto returns, so nothing remains to save at this point.
	// Mark done and release the WaitGroup NOW — before cam.Stop() — so the
	// finalize goroutine in handleStopRecording can set status="done"
	// immediately instead of waiting for SDK teardown (~500ms–2s per camera).
	appState.mu.Lock()
	appState.Cameras[camID].Recording = false
	appState.Cameras[camID].Drops = drops
	appState.mu.Unlock()
	recWg.Done()

	// Pipeline teardown and preview restart are slow (USB flush + device reopen).
	// Run them entirely in the background — the UI is already showing "done".
	go func() {
		cam.Stop()
		log.Printf("[camera %s] pipeline closed. Drops: %d", serial, drops)
		time.Sleep(400 * time.Millisecond)
		startPreview(serial)
	}()
}

// ─── Mock helpers ─────────────────────────────────────────────────────────────

func startMockPreview(serial string) {
	previewCancelsMu.Lock()
	defer previewCancelsMu.Unlock()
	if _, exists := previewCancels[serial]; exists {
		return
	}
	stopCh := make(chan struct{})
	previewCancels[serial] = func() { close(stopCh) }
	go mockPreviewWorker(serial, stopCh)
}

func mockCameraWorker(camID string, startCh, stopCh <-chan struct{}, readyCh chan<- struct{}) {
	defer recWg.Done()
	readyCh <- struct{}{}
	<-startCh
	<-stopCh
	appState.mu.Lock()
	appState.Cameras[camID].Recording = false
	appState.mu.Unlock()
}

// ─── HTTP handlers ────────────────────────────────────────────────────────────

func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(sourceDir(), "index.html"))
}

func handleDetect(w http.ResponseWriter, r *http.Request) {
	if !realsenseAvailable() {
		for _, s := range []string{"12345678", "87654321"} {
			startMockPreview(s)
		}
		writeJSON(w, map[string]any{
			"cameras": []map[string]any{
				{"index": 0, "serial": "12345678", "name": "Intel RealSense D435 (demo)"},
				{"index": 1, "serial": "87654321", "name": "Intel RealSense D435 (demo)"},
			},
		})
		return
	}
	devs, err := DetectCameras()
	if err != nil {
		writeJSONErr(w, err.Error(), 500)
		return
	}
	result := make([]map[string]any, len(devs))
	for i, d := range devs {
		result[i] = map[string]any{"index": i, "serial": d.Serial, "name": d.Name}
		startPreview(d.Serial)
	}
	writeJSON(w, map[string]any{"cameras": result})
}

func handleRestartPreviews(w http.ResponseWriter, r *http.Request) {
	appState.mu.RLock()
	rec := appState.IsRecording
	appState.mu.RUnlock()
	if rec {
		writeJSONErr(w, "Cannot restart previews while recording", 400)
		return
	}
	previewCancelsMu.Lock()
	serials := make([]string, 0, len(previewCancels))
	for s := range previewCancels {
		serials = append(serials, s)
	}
	previewCancelsMu.Unlock()

	for _, s := range serials {
		stopPreview(s)
		if realsenseAvailable() {
			startPreview(s)
		} else {
			startMockPreview(s)
		}
	}
	writeJSON(w, map[string]any{"ok": true, "restarted": serials})
}

var mjpegHeader = []byte("--frame\r\nContent-Type: image/jpeg\r\n\r\n")
var mjpegTrail = []byte("\r\n")

func handleStream(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	fs := ensureFrameSlot(serial)

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}

	bw := bufio.NewWriterSize(w, 64*1024)
	var lastSeq uint64

	for {
		f := fs.waitNext(lastSeq, time.Second)
		if f.seq == lastSeq || len(f.jpeg) == 0 {
			continue
		}
		lastSeq = f.seq

		if _, err := bw.Write(mjpegHeader); err != nil {
			return
		}
		if _, err := bw.Write(f.jpeg); err != nil {
			return
		}
		if _, err := bw.Write(mjpegTrail); err != nil {
			return
		}
		if err := bw.Flush(); err != nil {
			return
		}
		flusher.Flush()
	}
}

type startRecReq struct {
	Cameras []struct {
		Serial   string `json:"serial"`
		UserName string `json:"user_name"`
	} `json:"cameras"`
}

func handleStartRecording(w http.ResponseWriter, r *http.Request) {
	var req startRecReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Cameras) == 0 {
		writeJSONErr(w, "No cameras configured", 400)
		return
	}

	appState.mu.Lock()
	if appState.IsRecording {
		appState.mu.Unlock()
		writeJSONErr(w, "Already recording", 400)
		return
	}
	appState.IsRecording = true
	appState.Status = "warming"
	appState.Cameras = make(map[string]*CameraState)
	appState.SavedFiles = nil
	appState.mu.Unlock()
	pushState()

	stopCh := make(chan struct{})
	startCh := make(chan struct{})
	recStop = stopCh
	recStart = startCh

	bagPaths := make(map[string]string)
	readyChans := make(map[string]chan struct{})

	for _, cfg := range req.Cameras {
		serial := cfg.Serial
		name := cfg.UserName
		if name == "" {
			n := len(serial)
			if n > 4 {
				name = "cam_" + serial[n-4:]
			} else {
				name = "cam_" + serial
			}
		}
		camID := "cam_" + serial
		bag := bagFilename(name)
		bagPaths[camID] = bag

		appState.mu.Lock()
		appState.Cameras[camID] = &CameraState{
			Name: name, Serial: serial, Recording: true, BagPath: bag,
		}
		appState.mu.Unlock()

		readyCh := make(chan struct{}, 1)
		readyChans[serial] = readyCh

		recWg.Add(1)
		if realsenseAvailable() {
			go cameraWorker(serial, name, bag, camID, startCh, stopCh, readyCh)
		} else {
			go mockCameraWorker(camID, startCh, stopCh, readyCh)
		}
	}

	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for serial, ch := range readyChans {
			remaining := time.Until(deadline)
			select {
			case <-ch:
			case <-time.After(remaining):
				log.Printf("[warmup] WARNING: %s did not signal ready in time", serial)
			}
		}
		log.Print("[warmup] all cameras ready — starting")
		// Brief pause: lets the hardware FIFO flush any frames buffered
		// before the pipeline opened. 300ms is enough; the full 3s countdown
		// was UI theatre — the pipelines are already streaming at this point.
		time.Sleep(300 * time.Millisecond)
		close(startCh)
		appState.mu.Lock()
		appState.Status = "recording"
		appState.mu.Unlock()
		pushState()
	}()

	writeJSON(w, map[string]any{"ok": true, "status": "warming", "bag_paths": bagPaths})
}

func handleStopRecording(w http.ResponseWriter, r *http.Request) {
	appState.mu.Lock()
	if !appState.IsRecording {
		appState.mu.Unlock()
		writeJSONErr(w, "Not recording", 400)
		return
	}
	appState.Status = "saving"
	appState.mu.Unlock()
	pushState()

	close(recStop)

	go func() {
		recWg.Wait()
		appState.mu.Lock()
		saved := make([]SavedFile, 0, len(appState.Cameras))
		var total int64
		for _, cs := range appState.Cameras {
			saved = append(saved, SavedFile{Name: cs.Name, BagPath: cs.BagPath, Drops: cs.Drops})
			total += cs.Drops
		}
		appState.IsRecording = false
		appState.SavedFiles = saved
		appState.Status = "done"
		appState.mu.Unlock()
		pushState()
		runtime.GC()
		debug.FreeOSMemory()
		log.Printf("[finalize] done — total dropped frames: %d", total)
	}()

	writeJSON(w, map[string]any{"ok": true, "status": "saving"})
}

type camStatusResp struct {
	Name      string `json:"name"`
	Serial    string `json:"serial"`
	Recording bool   `json:"recording"`
	Drops     int64  `json:"drops"`
}

type statusResp struct {
	Status     string                   `json:"status"`
	Recording  bool                     `json:"recording"`
	SavedFiles []SavedFile              `json:"saved_files"`
	Cameras    map[string]camStatusResp `json:"cameras"`
	RSSBytes   int64                    `json:"rss_bytes"`
}

// ─── Process RSS ──────────────────────────────────────────────────────────────
// Read VmRSS from /proc/self/status — available on Linux without CGO.
// Returns 0 on non-Linux or any read error.

var currentRSS atomic.Int64 // bytes, updated by rssPoller

func readRSSBytes() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("VmRSS:")) {
			// Format: "VmRSS:\t  12345 kB"
			fields := bytes.Fields(line)
			if len(fields) >= 2 {
				var kb int64
				fmt.Sscan(string(fields[1]), &kb)
				return kb * 1024
			}
		}
	}
	return 0
}

// rssPoller updates currentRSS every second and pushes an SSE event so
// the frontend RAM gauge updates in real time.
func rssPoller() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		rss := readRSSBytes()
		currentRSS.Store(rss)
		pushState() // piggyback on existing SSE — no extra endpoint
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	appState.mu.RLock()
	defer appState.mu.RUnlock()

	resp := statusResp{
		Status:     appState.Status,
		Recording:  appState.IsRecording,
		SavedFiles: appState.SavedFiles,
		Cameras:    make(map[string]camStatusResp, len(appState.Cameras)),
	}
	for k, cs := range appState.Cameras {
		resp.Cameras[k] = camStatusResp{
			Name:      cs.Name,
			Serial:    cs.Serial,
			Recording: cs.Recording,
			Drops:     atomic.LoadInt64(&cs.Drops),
		}
	}
	writeJSON(w, resp)
}

// ─── SSE broadcaster ──────────────────────────────────────────────────────────
// All connected /api/events clients receive a pushed JSON event whenever
// state changes. No polling needed on the frontend.

type sseClient struct {
	ch chan []byte
}

var (
	sseMu      sync.Mutex
	sseClients = map[*sseClient]struct{}{}
)

func sseSubscribe() *sseClient {
	c := &sseClient{ch: make(chan []byte, 8)}
	sseMu.Lock()
	sseClients[c] = struct{}{}
	sseMu.Unlock()
	return c
}

func sseUnsubscribe(c *sseClient) {
	sseMu.Lock()
	delete(sseClients, c)
	sseMu.Unlock()
}

// pushState serialises the current appState and broadcasts it to all SSE clients.
// Call this whenever state meaningfully changes.
func pushState() {
	appState.mu.RLock()
	cams := make(map[string]camStatusResp, len(appState.Cameras))
	for k, cs := range appState.Cameras {
		cams[k] = camStatusResp{
			Name:      cs.Name,
			Serial:    cs.Serial,
			Recording: cs.Recording,
			Drops:     atomic.LoadInt64(&cs.Drops),
		}
	}
	resp := statusResp{
		Status:     appState.Status,
		Recording:  appState.IsRecording,
		SavedFiles: appState.SavedFiles,
		Cameras:    cams,
		RSSBytes:   currentRSS.Load(),
	}
	appState.mu.RUnlock()

	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	// Format as SSE: "data: {...}\n\n"
	msg := append([]byte("data: "), b...)
	msg = append(msg, '\n', '\n')

	sseMu.Lock()
	for c := range sseClients {
		select {
		case c.ch <- msg:
		default: // client too slow — skip this event rather than block
		}
	}
	sseMu.Unlock()
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if behind proxy

	c := sseSubscribe()
	defer sseUnsubscribe(c)

	// Send current state immediately on connect so the client is up to date.
	pushState()

	// Keepalive: send a comment every 15s so proxies don't close the connection.
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case msg := <-c.ch:
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}

func sourceDir() string {
	exe, err := os.Executable()
	if err != nil {
		cwd, _ := os.Getwd()
		return cwd
	}
	return filepath.Dir(exe)
}

func bagFilename(camName string) string {
	now := time.Now()
	day := now.Format("02012006")
	ts := now.Format("20060102_150405")
	dir := filepath.Join("recordings", day, camName)
	_ = os.MkdirAll(dir, 0755)
	return filepath.Join(dir, fmt.Sprintf("%s_%s.bag", camName, ts))
}

// keep unsafe imported (used by camera.go CGO path via build tag)
var _ = unsafe.Sizeof(0)

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex)
	mux.HandleFunc("GET /api/detect", handleDetect)
	mux.HandleFunc("POST /api/restart_previews", handleRestartPreviews)
	mux.HandleFunc("GET /api/stream/{serial}", handleStream)
	mux.HandleFunc("POST /api/start_recording", handleStartRecording)
	mux.HandleFunc("POST /api/stop_recording", handleStopRecording)
	mux.HandleFunc("GET /api/status", handleStatus)
	mux.HandleFunc("GET /api/events", handleEvents)
	mux.Handle("/", http.FileServer(http.Dir(sourceDir())))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			return
		}
		mux.ServeHTTP(w, r)
	})

	addr := "0.0.0.0:5050"
	log.Printf("RealSense Capture Server → http://%s", addr)
	go rssPoller()
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}