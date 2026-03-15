// RealSense Capture Server — Go rewrite
// ======================================
// Build tags:
//   -tags demo   → no CGO, no librealsense2, no OpenCV required
//   (no tag)     → full build, requires librealsense2 + OpenCV4

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Frame slot ───────────────────────────────────────────────────────────────
// Lock-free latest-frame store.
// Writers do an atomic pointer swap + Broadcast; readers block on Cond.Wait.

type frameSlot struct {
	ptr  atomic.Pointer[[]byte]
	mu   sync.Mutex
	cond *sync.Cond
}

func newFrameSlot() *frameSlot {
	fs := &frameSlot{}
	fs.cond = sync.NewCond(&fs.mu)
	return fs
}

func (fs *frameSlot) push(jpeg []byte) {
	fs.ptr.Store(&jpeg)
	fs.cond.Broadcast()
}

// wait blocks until a new frame arrives or the deadline passes.
func (fs *frameSlot) wait(deadline time.Time) []byte {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Watchdog: wake the Cond after deadline so we don't block forever.
	go func() {
		time.Sleep(time.Until(deadline))
		fs.cond.Broadcast()
	}()
	fs.cond.Wait()

	p := fs.ptr.Load()
	if p == nil {
		return nil
	}
	return *p
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

// ─── Preview pipeline ─────────────────────────────────────────────────────────

func previewWorker(serial string, stop <-chan struct{}) {
	fs := ensureFrameSlot(serial)

	pipe, err := NewPreviewPipeline(serial)
	if err != nil {
		log.Printf("[preview %s] open error: %v", serial, err)
		return
	}
	defer pipe.Close()
	log.Printf("[preview %s] started", serial)

	skipNext := false
	for {
		select {
		case <-stop:
			return
		default:
		}
		frame, err := pipe.WaitForFrame(1000)
		if err != nil {
			select {
			case <-stop:
				return
			default:
				continue
			}
		}
		skipNext = !skipNext
		if skipNext {
			continue // encode every other frame → ~15 fps to browser
		}
		jpeg := encodeJPEG(frame, 75)
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
	// Wait for the goroutine's WaitForFrame (1s timeout) to return
	time.Sleep(1200 * time.Millisecond)
}

// ─── Recording pipeline ───────────────────────────────────────────────────────

func cameraWorker(serial, userName, bagPath, camID string,
	startCh, stopCh <-chan struct{},
	readyCh chan<- struct{}) {

	defer recWg.Done()

	// Phase 1: stop preview, open recording pipeline
	stopPreview(serial)
	time.Sleep(300 * time.Millisecond)

	cam, err := NewCamera(userName, serial)
	if err != nil {
		log.Printf("[camera %s] FATAL open: %v", serial, err)
		appState.mu.Lock()
		appState.Cameras[camID].Recording = false
		appState.mu.Unlock()
		readyCh <- struct{}{}
		return
	}
	if err := cam.Start(bagPath); err != nil {
		log.Printf("[camera %s] FATAL start: %v", serial, err)
		appState.mu.Lock()
		appState.Cameras[camID].Recording = false
		appState.mu.Unlock()
		readyCh <- struct{}{}
		return
	}
	log.Printf("[camera %s] pipeline open → %s", serial, bagPath)
	readyCh <- struct{}{} // signal warmup: this camera is ready

	// Phase 2: wait for GO
	<-startCh
	appState.mu.Lock()
	appState.Status = "recording"
	appState.mu.Unlock()

	// Phase 3: hot grab loop
	rawCh := make(chan []byte, 8)
	encDone := make(chan struct{})
	fs := ensureFrameSlot(serial)

	go func() {
		defer close(encDone)
		for img := range rawCh {
			if jpeg := encodeJPEG(img, 70); jpeg != nil {
				fs.push(jpeg)
			}
		}
	}()

	var drops int64
	for {
		select {
		case <-stopCh:
			goto teardown
		default:
		}
		frame, err := cam.GrabFrame(200)
		if err != nil {
			drops++
			atomic.StoreInt64(&appState.Cameras[camID].Drops, drops)
			if drops%10 == 0 {
				log.Printf("[camera %s] ⚠ %d dropped frames", serial, drops)
			}
			continue
		}
		if frame == nil {
			continue
		}
		select {
		case rawCh <- frame:
		default: // encoder behind — drop preview frame, not the .bag
		}
	}

teardown:
	close(rawCh)
	<-encDone
	cam.Stop()
	log.Printf("[camera %s] stopped. Drops: %d", serial, drops)

	appState.mu.Lock()
	appState.Cameras[camID].Recording = false
	appState.Cameras[camID].Drops = drops
	appState.mu.Unlock()

	time.Sleep(400 * time.Millisecond)
	startPreview(serial)
}

// ─── Mock helpers (used by both demo and real builds) ─────────────────────────

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

	for {
		jpeg := fs.wait(time.Now().Add(time.Second))
		if jpeg == nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\n\r\n"); err != nil {
			return
		}
		if _, err := w.Write(jpeg); err != nil {
			return
		}
		if _, err := w.Write([]byte("\r\n")); err != nil {
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
		log.Print("[warmup] all cameras ready — counting down 3s")
		time.Sleep(3 * time.Second)
		close(startCh)
		appState.mu.Lock()
		appState.Status = "recording"
		appState.mu.Unlock()
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
		log.Printf("[finalize] done — total dropped frames: %d", total)
	}()

	writeJSON(w, map[string]any{"ok": true, "status": "saving"})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	appState.mu.RLock()
	defer appState.mu.RUnlock()

	cams := make(map[string]map[string]any, len(appState.Cameras))
	for k, cs := range appState.Cameras {
		cams[k] = map[string]any{
			"name":      cs.Name,
			"serial":    cs.Serial,
			"recording": cs.Recording,
			"drops":     atomic.LoadInt64(&cs.Drops),
		}
	}
	writeJSON(w, map[string]any{
		"status":      appState.Status,
		"recording":   appState.IsRecording,
		"saved_files": appState.SavedFiles,
		"cameras":     cams,
	})
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

// sourceDir returns the directory of the running binary.
// During `go run`, os.Executable points to a temp path, so we fall back to cwd.
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
	mux.Handle("/", http.FileServer(http.Dir(sourceDir())))

	// CORS wrapper
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
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}
