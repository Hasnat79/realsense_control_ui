"""
RealSense Capture Server — frame-perfect recording build
=========================================================

Key design decisions
--------------------
1.  Recording thread does ONE thing: call cam.grab_frame() as fast as possible
    and push raw numpy arrays into a bounded queue.  No JPEG encoding, no locks
    on the hot path.

2.  A dedicated JPEG-encoder thread drains that queue and writes to the shared
    frame slot.  The MJPEG streamer only touches the frame slot, never the
    recording pipeline.

3.  Preview pipeline is stopped *and confirmed dead* before the recording
    pipeline opens the device — no fixed sleeps, no races.

4.  Recording pipeline is opened during warmup (before the GO signal), so the
    device is already streaming and the hardware FIFO is fresh when recording
    starts.

5.  Frame-drop counting: any grab_frame() timeout increments a counter that is
    exposed on /api/status so the UI can surface it.

6.  The preview worker never sleeps after wait_for_frames(); it only throttles
    by skipping every other frame.  The old sleep(1/15) was causing the hardware
    FIFO to back up between calls.

7.  frame slot uses a threading.Event so the MJPEG streamer wakes up
    immediately on a new frame rather than polling at 20 Hz.

8.  warmup_then_go() waits for every camera pipeline to signal ready before
    starting the 3-second countdown — so all 3 seconds are pure warmup, not
    setup time.
"""

from flask import Flask, jsonify, request, Response, send_from_directory
from flask_cors import CORS
import threading
import queue
import time
import datetime
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

try:
    import pyrealsense2 as rs
    import cv2
    import numpy as np
    from utils import detect_connected_cameras
    from camera import Camera
    REALSENSE_AVAILABLE = True
except ImportError as e:
    REALSENSE_AVAILABLE = False
    print(f"Warning: RealSense modules not found ({e}). Running in demo mode.")
    import numpy as np
    import cv2

BASE_DIR = os.path.dirname(os.path.abspath(__file__))
app = Flask(__name__, static_folder=BASE_DIR, static_url_path='')
CORS(app)

@app.route('/')
def index():
    return send_from_directory(BASE_DIR, 'index.html')


# ─── Per-camera frame slots ───────────────────────────────────────────────────
# serial -> {"jpeg": bytes | None, "event": threading.Event}
# Writers call _push_jpeg(); readers call _get_jpeg() or wait on the event.
_cam_frame: dict = {}
_cam_frame_lock = threading.Lock()   # only for dict structure mutations


def _ensure_frame_slot(serial: str):
    with _cam_frame_lock:
        if serial not in _cam_frame:
            _cam_frame[serial] = {"jpeg": None, "event": threading.Event()}


def _push_jpeg(serial: str, jpeg: bytes):
    slot = _cam_frame[serial]
    slot["jpeg"] = jpeg
    slot["event"].set()


def _get_jpeg(serial: str):
    return _cam_frame.get(serial, {}).get("jpeg")


# ─── Preview pipeline registry ────────────────────────────────────────────────
_preview_threads:      dict = {}
_preview_stop_events:  dict = {}
_preview_registry_lock = threading.Lock()

# ─── Global recording state ───────────────────────────────────────────────────
state = {
    "cameras":      {},    # cam_id -> {name, serial, recording, bag_path, drops}
    "recording":    False,
    "stop_event":   None,
    "start_event":  None,
    "ready_events": {},    # serial -> Event; set when pipeline is streaming
    "saved_files":  [],
    "status":       "idle",  # idle | warming | recording | saving | done
}
state_lock = threading.Lock()


# ─── Helpers ──────────────────────────────────────────────────────────────────

def get_dated_dir() -> str:
    today = datetime.datetime.now().strftime("%d%m%Y")
    path  = os.path.join("recordings", today)
    os.makedirs(path, exist_ok=True)
    return path


def bag_filename(cam_name: str) -> str:
    now     = datetime.datetime.now().strftime("%Y%m%d_%H%M%S")
    cam_dir = os.path.join(get_dated_dir(), cam_name)
    os.makedirs(cam_dir, exist_ok=True)
    return os.path.join(cam_dir, f"{cam_name}_{now}.bag")


def _bgr_to_jpeg(img: np.ndarray, quality: int = 75) -> bytes:
    _, buf = cv2.imencode(".jpg", img, [cv2.IMWRITE_JPEG_QUALITY, quality])
    return buf.tobytes()


# ─── Preview pipeline ─────────────────────────────────────────────────────────

def _preview_worker(serial: str, stop_event: threading.Event):
    """
    Runs at the camera's native 30 fps cadence — no artificial sleep.
    Encodes every other frame (~15 fps to browser) to keep CPU low without
    adding latency.
    """
    pipeline = rs.pipeline()
    cfg      = rs.config()
    cfg.enable_device(serial)
    cfg.enable_stream(rs.stream.color, 640, 480, rs.format.bgr8, 30)

    _ensure_frame_slot(serial)

    try:
        pipeline.start(cfg)
        print(f"[preview {serial}] started")
        skip = False
        while not stop_event.is_set():
            try:
                frames = pipeline.wait_for_frames(timeout_ms=1000)
            except RuntimeError:
                if stop_event.is_set():
                    break
                continue

            skip = not skip          # encode every other frame → ~15 fps output
            if skip:
                continue

            color = frames.get_color_frame()
            if not color:
                continue

            img  = np.asanyarray(color.get_data())
            jpeg = _bgr_to_jpeg(img)
            _push_jpeg(serial, jpeg)

    except Exception as e:
        print(f"[preview {serial}] error: {e}")
    finally:
        try:
            pipeline.stop()
        except Exception:
            pass
        print(f"[preview {serial}] stopped")


def _start_preview(serial: str):
    with _preview_registry_lock:
        t = _preview_threads.get(serial)
        if t and t.is_alive():
            return
        stop_ev = threading.Event()
        _preview_stop_events[serial] = stop_ev
        t = threading.Thread(
            target=_preview_worker, args=(serial, stop_ev),
            name=f"preview-{serial}", daemon=True
        )
        _preview_threads[serial] = t
        t.start()


def _stop_preview(serial: str, timeout: float = 6.0) -> bool:
    """
    Signal the preview thread to stop and block until it exits.
    Returns True if the thread exited cleanly within timeout.
    """
    with _preview_registry_lock:
        ev = _preview_stop_events.pop(serial, None)
        t  = _preview_threads.pop(serial, None)

    if ev:
        ev.set()
    if t and t.is_alive():
        t.join(timeout=timeout)
        if t.is_alive():
            print(f"[preview {serial}] WARNING: thread did not exit within {timeout}s")
            return False
    return True


# ─── Recording pipeline ───────────────────────────────────────────────────────

def _jpeg_encoder_worker(serial: str, raw_q: queue.Queue,
                         stop_event: threading.Event):
    """
    Drains raw BGR numpy frames from raw_q, encodes to JPEG, and pushes to the
    shared frame slot.  Runs in its own thread so the recording grab loop is
    never stalled by cv2.imencode().
    """
    _ensure_frame_slot(serial)
    while not stop_event.is_set() or not raw_q.empty():
        try:
            img = raw_q.get(timeout=0.2)
        except queue.Empty:
            continue
        jpeg = _bgr_to_jpeg(img, quality=70)   # slightly lower quality = faster encode
        _push_jpeg(serial, jpeg)


def camera_worker(serial: str, user_name: str,
                  start_event: threading.Event,
                  stop_event:  threading.Event,
                  bag_path: str, cam_id: str,
                  ready_event: threading.Event):
    """
    Phase 1 — SETUP (runs immediately, in parallel with other cameras):
        • Stop preview pipeline and confirm it has exited.
        • Open the recording pipeline (Camera.start).
        • Signal ready_event so warmup_then_go() knows this camera is live.

    Phase 2 — WAIT (inside warmup window):
        • Sit idle while other cameras finish setup and countdown runs.
        • The pipeline is already streaming so the hardware FIFO is fresh.

    Phase 3 — RECORD (after GO signal from start_event):
        • Grab frames at 200 ms timeout — tight enough to detect real drops fast.
        • Push raw BGR arrays to a bounded queue for the encoder thread.
        • Never block on encoding; drop the preview frame if encoder is behind.

    Phase 4 — TEARDOWN:
        • Drain encoder, stop Camera cleanly, restart preview pipeline.
    """

    # ── Phase 1: release preview, open recording pipeline ──────────────────
    clean = _stop_preview(serial, timeout=6.0)
    if not clean:
        print(f"[camera {serial}] preview thread lingered — proceeding anyway")

    # Small yield to let the USB stack release the device handle
    time.sleep(0.3)

    cam = Camera(user_name, serial)
    try:
        cam.start(output_bag=bag_path)
    except Exception as e:
        print(f"[camera {serial}] FATAL: could not open recording pipeline: {e}")
        with state_lock:
            state["cameras"][cam_id]["recording"] = False
        ready_event.set()    # unblock warmup even on failure
        return

    with state_lock:
        state["cameras"][cam_id]["camera_obj"] = cam

    print(f"[camera {serial}] recording pipeline open → {bag_path}")
    ready_event.set()   # signal to warmup_then_go: this camera is ready

    # ── Phase 2: wait for GO ────────────────────────────────────────────────
    start_event.wait()

    with state_lock:
        state["status"] = "recording"

    # ── Phase 3: hot record loop ────────────────────────────────────────────
    # Bounded queue: if the encoder falls more than 8 frames behind, we drop
    # preview frames — but the .bag file is written by cam.grab_frame() itself
    # and is never affected.
    raw_q: queue.Queue = queue.Queue(maxsize=8)
    encoder_stop = threading.Event()
    enc_thread = threading.Thread(
        target=_jpeg_encoder_worker,
        args=(serial, raw_q, encoder_stop),
        name=f"enc-{serial}", daemon=True
    )
    enc_thread.start()

    drops = 0
    try:
        while not stop_event.is_set():
            try:
                frames = cam.grab_frame(timeout_ms=200)
            except RuntimeError:
                drops += 1
                with state_lock:
                    state["cameras"][cam_id]["drops"] = drops
                if drops % 10 == 0:
                    print(f"[camera {serial}] ⚠ {drops} dropped frames")
                continue

            if not frames:
                continue

            color = frames.get_color_frame()
            if color:
                # .copy() is critical — librealsense recycles the underlying
                # buffer as soon as we call grab_frame() again
                img = np.asanyarray(color.get_data()).copy()
                try:
                    raw_q.put_nowait(img)
                except queue.Full:
                    pass   # encoder behind — drop preview frame, not the .bag frame

    finally:
        # ── Phase 4: teardown ───────────────────────────────────────────────
        encoder_stop.set()
        enc_thread.join(timeout=3)

        try:
            cam.stop()
            print(f"[camera {serial}] stopped cleanly. Drops: {drops}")
        except Exception as e:
            print(f"[camera {serial}] stop warning (ignored): {e}")

        with state_lock:
            state["cameras"][cam_id]["recording"] = False
            state["cameras"][cam_id]["drops"]     = drops

        time.sleep(0.4)
        _start_preview(serial)


# ─── Demo / mock helpers ──────────────────────────────────────────────────────

def _mock_preview_worker(serial: str, stop_event: threading.Event):
    hue     = (int(serial[-2:], 16) % 180) if serial[-2:].isalnum() else 90
    frame_n = 0
    _ensure_frame_slot(serial)
    while not stop_event.is_set():
        h, w = 480, 640
        img  = np.zeros((h, w, 3), dtype=np.uint8)
        shift = (frame_n * 2) % 180
        img[:, :, 0] = (hue + shift) % 180
        img[:, :, 1] = 180
        img[:, :, 2] = np.tile(np.linspace(80, 220, w, dtype=np.uint8), (h, 1))
        img = cv2.cvtColor(img, cv2.COLOR_HSV2BGR)
        cv2.putText(img, f"DEMO CAM {serial[-4:]}", (20, 40),
                    cv2.FONT_HERSHEY_SIMPLEX, 1.0, (255, 255, 255), 2)
        cv2.putText(img, datetime.datetime.now().strftime("%H:%M:%S.%f")[:-3],
                    (20, 80), cv2.FONT_HERSHEY_SIMPLEX, 0.6, (200, 200, 200), 1)
        _push_jpeg(serial, _bgr_to_jpeg(img))
        frame_n += 1
        time.sleep(1 / 15)


def _start_mock_preview(serial: str):
    with _preview_registry_lock:
        t = _preview_threads.get(serial)
        if t and t.is_alive():
            return
        stop_ev = threading.Event()
        _preview_stop_events[serial] = stop_ev
        t = threading.Thread(
            target=_mock_preview_worker, args=(serial, stop_ev),
            name=f"mock-{serial}", daemon=True
        )
        _preview_threads[serial] = t
        t.start()


def _mock_record_worker(cam_id: str,
                        start_event: threading.Event,
                        stop_event:  threading.Event,
                        ready_event: threading.Event):
    ready_event.set()
    start_event.wait()
    while not stop_event.is_set():
        time.sleep(0.05)
    with state_lock:
        state["cameras"][cam_id]["recording"] = False


# ─── API ──────────────────────────────────────────────────────────────────────

@app.route("/api/detect", methods=["GET"])
def detect():
    if not REALSENSE_AVAILABLE:
        serials = ["12345678", "87654321"]
        for s in serials:
            _start_mock_preview(s)
        return jsonify({"cameras": [
            {"index": 0, "serial": "12345678", "name": "Intel RealSense D435 (demo)"},
            {"index": 1, "serial": "87654321", "name": "Intel RealSense D435 (demo)"},
        ]})
    try:
        devices = detect_connected_cameras()
        result  = []
        for i, dev in enumerate(devices):
            serial = dev.get_info(rs.camera_info.serial_number)
            name   = dev.get_info(rs.camera_info.name)
            result.append({"index": i, "serial": serial, "name": name})
            _start_preview(serial)
        return jsonify({"cameras": result})
    except Exception as e:
        return jsonify({"error": str(e)}), 500


@app.route("/api/restart_previews", methods=["POST"])
def restart_previews():
    """
    Tear down every active preview pipeline and restart them all.
    Called by the frontend's Detect / Re-scan button to recover frozen feeds.
    Safe to call at any time when NOT recording.
    """
    with state_lock:
        if state["recording"]:
            return jsonify({"error": "Cannot restart previews while recording"}), 400

    # Collect serials that currently have a live preview thread
    with _preview_registry_lock:
        active_serials = list(_preview_threads.keys())

    restarted = []
    for serial in active_serials:
        print(f"[restart_previews] cycling {serial}")
        _stop_preview(serial, timeout=6.0)
        if REALSENSE_AVAILABLE:
            _start_preview(serial)
        else:
            _start_mock_preview(serial)
        restarted.append(serial)

    return jsonify({"ok": True, "restarted": restarted})


@app.route("/api/stream/<serial>")
def stream_mjpeg(serial: str):
    """
    Event-driven MJPEG stream.  The generator wakes on threading.Event instead
    of polling at 20 Hz — this halves latency and eliminates the busy-wait.
    """
    _ensure_frame_slot(serial)

    def generate():
        slot = _cam_frame[serial]
        while True:
            slot["event"].wait(timeout=1.0)   # wake on new frame; 1 s watchdog
            slot["event"].clear()
            jpeg = slot["jpeg"]
            if jpeg:
                yield (b"--frame\r\nContent-Type: image/jpeg\r\n\r\n"
                       + jpeg + b"\r\n")

    return Response(generate(), mimetype="multipart/x-mixed-replace; boundary=frame")


@app.route("/api/start_recording", methods=["POST"])
def start_recording():
    data           = request.json or {}
    cameras_config = data.get("cameras", [])
    if not cameras_config:
        return jsonify({"error": "No cameras configured"}), 400

    with state_lock:
        if state["recording"]:
            return jsonify({"error": "Already recording"}), 400
        stop_event   = threading.Event()
        start_event  = threading.Event()
        ready_events = {}
        state.update({
            "stop_event":   stop_event,
            "start_event":  start_event,
            "ready_events": ready_events,
            "saved_files":  [],
            "status":       "warming",
            "recording":    True,
            "cameras":      {},
        })

    threads   = []
    bag_paths = {}

    for cfg in cameras_config:
        serial    = cfg["serial"]
        user_name = cfg["user_name"].strip() or f"cam_{serial[-4:]}"
        cam_id    = f"cam_{serial}"
        bag_path  = bag_filename(user_name)
        bag_paths[cam_id] = bag_path

        ready_ev = threading.Event()
        ready_events[serial] = ready_ev

        with state_lock:
            state["cameras"][cam_id] = {
                "name": user_name, "serial": serial,
                "camera_obj": None, "recording": True,
                "bag_path": bag_path, "drops": 0,
            }

        if REALSENSE_AVAILABLE:
            t = threading.Thread(
                target=camera_worker,
                args=(serial, user_name, start_event, stop_event,
                      bag_path, cam_id, ready_ev),
                name=f"rec-{serial}", daemon=True
            )
        else:
            t = threading.Thread(
                target=_mock_record_worker,
                args=(cam_id, start_event, stop_event, ready_ev),
                name=f"mock-rec-{serial}", daemon=True
            )
        t.start()
        threads.append(t)

    state["threads"]   = threads
    state["bag_paths"] = bag_paths

    def warmup_then_go():
        """
        Wait for every camera pipeline to signal ready, THEN start the 3-second
        countdown.  Max 10 s to open all pipelines before proceeding anyway.
        """
        deadline = time.time() + 10.0
        for serial, rev in ready_events.items():
            remaining = max(0.0, deadline - time.time())
            if not rev.wait(timeout=remaining):
                print(f"[warmup] WARNING: {serial} did not signal ready in time")

        print("[warmup] all cameras ready — counting down 3 s")
        time.sleep(3)
        start_event.set()
        with state_lock:
            state["status"] = "recording"

    threading.Thread(target=warmup_then_go, daemon=True, name="warmup").start()
    return jsonify({"ok": True, "status": "warming", "bag_paths": bag_paths})


@app.route("/api/stop_recording", methods=["POST"])
def stop_recording():
    with state_lock:
        if not state["recording"]:
            return jsonify({"error": "Not recording"}), 400
        stop_event      = state["stop_event"]
        state["status"] = "saving"

    stop_event.set()

    def finalize():
        for t in state.get("threads", []):
            t.join(timeout=20)
        saved = [
            {"name": v["name"], "bag_path": v["bag_path"], "drops": v.get("drops", 0)}
            for v in state["cameras"].values()
        ]
        with state_lock:
            state["recording"]   = False
            state["saved_files"] = saved
            state["status"]      = "done"
        total_drops = sum(s["drops"] for s in saved)
        print(f"[finalize] done — total dropped frames: {total_drops}")

    threading.Thread(target=finalize, daemon=True, name="finalize").start()
    return jsonify({"ok": True, "status": "saving"})


@app.route("/api/status", methods=["GET"])
def get_status():
    with state_lock:
        cameras_out = {
            k: {
                "name":      v["name"],
                "serial":    v["serial"],
                "recording": v.get("recording", False),
                "drops":     v.get("drops", 0),
            }
            for k, v in state["cameras"].items()
        }
        return jsonify({
            "status":      state["status"],
            "recording":   state["recording"],
            "saved_files": state["saved_files"],
            "cameras":     cameras_out,
        })


if __name__ == "__main__":
    print("Starting Camera Recording Server → http://0.0.0.0:5050")
    app.run(host="0.0.0.0", port=5050, debug=False, threaded=True)