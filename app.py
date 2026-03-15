"""
RealSense Capture Server — updated to match new index.html
===========================================================

Changes from original:
- /api/events  — Server-Sent Events endpoint replaces 500 ms status polling.
                  State is pushed instantly on every transition and every second
                  for the RAM gauge. The browser never needs to poll.
- /api/config  — Returns default output directory (~/Documents).
- output_dir   — start_recording now accepts an optional output_dir in the
                  request body. Defaults to ~/Documents/DDMMYYYY/<cam>/.
- rss_bytes    — Status and SSE events include current process RSS memory.
- Warmup       — 3-second countdown replaced with a 300 ms hardware-FIFO
                  stabilisation pause. The UI no longer shows a countdown.
- saved_files  — Each entry now includes a `drops` field.
"""

from flask import Flask, jsonify, request, Response, send_from_directory, stream_with_context
from flask_cors import CORS
import threading
import queue
import time
import datetime
import os
import sys
import json
import resource

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

_cam_frame: dict = {}
_cam_frame_lock = threading.Lock()


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
    "cameras":      {},
    "recording":    False,
    "stop_event":   None,
    "start_event":  None,
    "ready_events": {},
    "saved_files":  [],
    "status":       "idle",   # idle | warming | recording | saving | done
}
state_lock = threading.Lock()


# ─── SSE broadcaster ──────────────────────────────────────────────────────────
# All connected /api/events clients receive a pushed JSON event on every
# state change. The UI never polls /api/status; it only consumes SSE.

_sse_clients:     list  = []
_sse_clients_lock = threading.Lock()


def _sse_subscribe() -> queue.Queue:
    q = queue.Queue(maxsize=16)
    with _sse_clients_lock:
        _sse_clients.append(q)
    return q


def _sse_unsubscribe(q: queue.Queue):
    with _sse_clients_lock:
        try:
            _sse_clients.remove(q)
        except ValueError:
            pass


def _get_rss_bytes() -> int:
    """Return process RSS in bytes. Uses /proc/self/status on Linux."""
    try:
        with open("/proc/self/status") as f:
            for line in f:
                if line.startswith("VmRSS:"):
                    kb = int(line.split()[1])
                    return kb * 1024
    except Exception:
        pass
    # Fallback: getrusage (returns KB on Linux, bytes on macOS)
    try:
        ru = resource.getrusage(resource.RUSAGE_SELF)
        return ru.ru_maxrss * 1024  # Linux reports in KB
    except Exception:
        return 0


def _build_state_payload() -> dict:
    """Serialise current state + RSS into the SSE event payload."""
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
        return {
            "status":      state["status"],
            "recording":   state["recording"],
            "saved_files": state["saved_files"],
            "cameras":     cameras_out,
            "rss_bytes":   _get_rss_bytes(),
        }


def _push_state():
    """Broadcast current state to all SSE clients."""
    payload = _build_state_payload()
    msg = "data: " + json.dumps(payload) + "\n\n"
    msg_bytes = msg.encode()
    with _sse_clients_lock:
        dead = []
        for q in _sse_clients:
            try:
                q.put_nowait(msg_bytes)
            except queue.Full:
                dead.append(q)
        for q in dead:
            try:
                _sse_clients.remove(q)
            except ValueError:
                pass


def _rss_poller():
    """Push an SSE event every second so the RAM gauge updates in real time."""
    while True:
        time.sleep(1)
        _push_state()


threading.Thread(target=_rss_poller, daemon=True, name="rss-poller").start()


@app.route("/api/events")
def sse_events():
    """
    Server-Sent Events stream. The browser connects once and receives pushed
    JSON state on every transition. Keepalive comments are sent every 15s.
    """
    q = _sse_subscribe()

    # Send current state immediately on connect.
    _push_state()

    def generate():
        try:
            while True:
                try:
                    msg = q.get(timeout=15)
                    yield msg
                except queue.Empty:
                    # Keepalive — prevents proxies from closing the connection.
                    yield b": keepalive\n\n"
        except GeneratorExit:
            pass
        finally:
            _sse_unsubscribe(q)

    return Response(
        stream_with_context(generate()),
        mimetype="text/event-stream",
        headers={
            "Cache-Control":    "no-cache",
            "X-Accel-Buffering": "no",
        }
    )


# ─── Config ───────────────────────────────────────────────────────────────────

def _default_output_dir() -> str:
    home = os.path.expanduser("~")
    return os.path.join(home, "Documents")


def _resolve_output_dir(raw: str) -> str:
    """Expand ~ and clean the path. Empty → ~/Documents."""
    if not raw or not raw.strip():
        return _default_output_dir()
    return os.path.normpath(os.path.expanduser(raw.strip()))


@app.route("/api/config", methods=["GET"])
def get_config():
    return jsonify({"default_output_dir": _default_output_dir()})


# ─── Helpers ──────────────────────────────────────────────────────────────────

def bag_filename(output_dir: str, cam_name: str) -> str:
    today   = datetime.datetime.now().strftime("%d%m%Y")
    ts      = datetime.datetime.now().strftime("%Y%m%d_%H%M%S")
    cam_dir = os.path.join(output_dir, today, cam_name)
    os.makedirs(cam_dir, exist_ok=True)
    return os.path.join(cam_dir, f"{cam_name}_{ts}.bag")


def _bgr_to_jpeg(img: np.ndarray, quality: int = 75) -> bytes:
    _, buf = cv2.imencode(".jpg", img, [cv2.IMWRITE_JPEG_QUALITY, quality])
    return buf.tobytes()


# ─── Preview pipeline ─────────────────────────────────────────────────────────

def _preview_worker(serial: str, stop_event: threading.Event):
    """
    Idle preview: runs at the camera's native 30fps cadence.
    Encodes every other frame (~15fps to browser) to keep CPU low.
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

            skip = not skip
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
    _ensure_frame_slot(serial)
    while not stop_event.is_set() or not raw_q.empty():
        try:
            img = raw_q.get(timeout=0.2)
        except queue.Empty:
            continue
        jpeg = _bgr_to_jpeg(img, quality=70)
        _push_jpeg(serial, jpeg)


def camera_worker(serial: str, user_name: str,
                  start_event: threading.Event,
                  stop_event:  threading.Event,
                  bag_path: str, cam_id: str,
                  ready_event: threading.Event):
    """
    Phase 1 — Stop preview, open recording pipeline.
    Phase 2 — Wait for GO signal (300ms hardware stabilisation, no 3s countdown).
    Phase 3 — Hot record loop: grab frames, push to encoder queue for preview.
    Phase 4 — Teardown: drain encoder, stop camera, restart preview.
    """

    # Phase 1
    clean = _stop_preview(serial, timeout=6.0)
    if not clean:
        print(f"[camera {serial}] preview thread lingered — proceeding anyway")
    time.sleep(0.3)

    cam = Camera(user_name, serial)
    try:
        cam.start(output_bag=bag_path)
    except Exception as e:
        print(f"[camera {serial}] FATAL: could not open recording pipeline: {e}")
        with state_lock:
            state["cameras"][cam_id]["recording"] = False
        ready_event.set()
        return

    with state_lock:
        state["cameras"][cam_id]["camera_obj"] = cam

    # Warm-up grab: drains the first frame from the hardware FIFO so the
    # record loop never hits a timeout on frame 1 (which counted as a drop).
    try:
        cam.grab_frame(timeout_ms=1000)
    except Exception:
        pass

    print(f"[camera {serial}] pipeline open → {bag_path}")
    ready_event.set()

    # Phase 2 — wait for GO
    start_event.wait()
    with state_lock:
        state["status"] = "recording"
    _push_state()

    # Phase 3 — record loop
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
                    _push_state()
                continue

            if not frames:
                continue

            color = frames.get_color_frame()
            if color:
                img = np.asanyarray(color.get_data()).copy()
                try:
                    raw_q.put_nowait(img)
                except queue.Full:
                    pass

    finally:
        # Phase 4 — teardown
        encoder_stop.set()
        enc_thread.join(timeout=3)

        with state_lock:
            state["cameras"][cam_id]["recording"] = False
            state["cameras"][cam_id]["drops"]     = drops

        # Signal done BEFORE stopping the camera so the UI updates immediately.
        # cam.stop() can block for ~500ms–2s while the SDK flushes USB buffers.
        _push_state()

        try:
            cam.stop()
            print(f"[camera {serial}] stopped cleanly. Drops: {drops}")
        except Exception as e:
            print(f"[camera {serial}] stop warning (ignored): {e}")

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
    with state_lock:
        if state["recording"]:
            return jsonify({"error": "Cannot restart previews while recording"}), 400

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
    """Event-driven MJPEG stream. Wakes on threading.Event instead of polling."""
    _ensure_frame_slot(serial)

    def generate():
        slot = _cam_frame[serial]
        while True:
            slot["event"].wait(timeout=1.0)
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
    output_dir_raw = data.get("output_dir", "")

    if not cameras_config:
        return jsonify({"error": "No cameras configured"}), 400

    output_dir = _resolve_output_dir(output_dir_raw)
    try:
        os.makedirs(output_dir, exist_ok=True)
    except OSError as e:
        return jsonify({"error": f"Cannot create output directory: {e}"}), 400

    print(f"[record] output directory: {output_dir}")

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

    _push_state()

    threads   = []
    bag_paths = {}

    for cfg in cameras_config:
        serial    = cfg["serial"]
        user_name = cfg.get("user_name", "").strip() or f"cam_{serial[-4:]}"
        cam_id    = f"cam_{serial}"
        bag_path  = bag_filename(output_dir, user_name)
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
        Wait for every camera to signal ready, then a brief 300ms hardware
        stabilisation pause — no 3-second countdown.
        """
        deadline = time.time() + 10.0
        for serial, rev in ready_events.items():
            remaining = max(0.0, deadline - time.time())
            if not rev.wait(timeout=remaining):
                print(f"[warmup] WARNING: {serial} did not signal ready in time")

        print("[warmup] all cameras ready — starting")
        time.sleep(0.3)
        start_event.set()
        with state_lock:
            state["status"] = "recording"
        _push_state()

    threading.Thread(target=warmup_then_go, daemon=True, name="warmup").start()
    return jsonify({"ok": True, "status": "warming", "bag_paths": bag_paths})


@app.route("/api/stop_recording", methods=["POST"])
def stop_recording():
    with state_lock:
        if not state["recording"]:
            return jsonify({"error": "Not recording"}), 400
        stop_event      = state["stop_event"]
        state["status"] = "saving"

    _push_state()
    stop_event.set()

    def finalize():
        for t in state.get("threads", []):
            t.join(timeout=20)
        saved = [
            {
                "name":     v["name"],
                "bag_path": v["bag_path"],
                "drops":    v.get("drops", 0),
            }
            for v in state["cameras"].values()
        ]
        with state_lock:
            state["recording"]   = False
            state["saved_files"] = saved
            state["status"]      = "done"
        total_drops = sum(s["drops"] for s in saved)
        print(f"[finalize] done — total dropped frames: {total_drops}")
        _push_state()

    threading.Thread(target=finalize, daemon=True, name="finalize").start()
    return jsonify({"ok": True, "status": "saving"})


@app.route("/api/status", methods=["GET"])
def get_status():
    """Legacy polling endpoint — still available but the UI uses /api/events."""
    return jsonify(_build_state_payload())


if __name__ == "__main__":
    print("Starting Camera Recording Server → http://0.0.0.0:5050")
    app.run(host="0.0.0.0", port=5050, debug=False, threaded=True)