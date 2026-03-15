# RealSense Capture Studio — Go Server

A lean rewrite of the Python/Flask RealSense capture server using Go's native
concurrency and direct CGO calls to librealsense2. Same `index.html` frontend,
no virtualenv, single binary.

---

## Table of Contents

1. [Project Structure](#project-structure)
2. [Build Modes](#build-modes)
3. [Ubuntu Setup](#ubuntu-setup)
4. [macOS Setup](#macos-setup)
5. [Running the Server](#running-the-server)
6. [Troubleshooting](#troubleshooting)
7. [API Reference](#api-reference)
8. [Tuning & Configuration](#tuning--configuration)

---

## Project Structure

```
realsense-go/
├── main.go           # HTTP server, goroutine orchestration, SSE broadcaster
├── camera.go         # CGO bridge to librealsense2  (//go:build !demo)
├── camera_demo.go    # Hardware stubs               (//go:build demo)
├── encode.go         # Pure-Go JPEG encoder         (//go:build !demo)
├── encode_demo.go    # Pure-Go JPEG encoder         (//go:build demo)
├── encode.cpp        # cv::imencode via OpenCV      (CGO, !demo only)
├── mock.go           # Animated fake camera frames  (both builds)
├── index.html        # Frontend — SSE-driven, decoupled preview
├── install.sh        # One-shot Ubuntu dependency installer
├── go.mod
└── Makefile
```

### Build tag split

| File | Tag | Compiled when |
|------|-----|---------------|
| `camera.go` | `!demo` | Real hardware build |
| `encode.go` + `encode.cpp` | `!demo` | Real hardware build |
| `camera_demo.go` | `demo` | Demo build |
| `encode_demo.go` | `demo` | Demo build |
| `main.go`, `mock.go` | _(none)_ | Always |

> **Why the split?** Go refuses to compile `.cpp` files when CGO is inactive.
> The `//go:build demo` tag on `encode.go` deactivates CGO for that file,
> so Go ignores `encode.cpp` entirely in demo builds.

---

## Build Modes

| Command | Hardware needed | SDK needed | Use case |
|---------|----------------|------------|----------|
| `make run-demo` | ✗ | ✗ | Development, UI work, CI |
| `make run` | ✓ | ✓ | Production with real cameras |
| `make demo` | ✗ | ✗ | Just compile the demo binary |
| `make all` | ✗ | ✓ | Compile real binary (run separately) |

---

## Ubuntu Setup

Tested on Ubuntu 20.04, 22.04, and 24.04.

### Automated (recommended)

Run the provided installer script. It handles Go, librealsense2, OpenCV,
and udev rules in one shot:

```bash
chmod +x install.sh
./install.sh
```

Then reload your shell and run:

```bash
source ~/.bashrc
make run        # real hardware
make run-demo   # no hardware needed
```

That's it. The manual steps below are for reference or if you need to
install components individually.

---

### Manual installation

#### 1. Build essentials

```bash
sudo apt-get update
sudo apt-get install -y build-essential gcc g++ make git curl wget \
  ca-certificates gnupg pkg-config
```

#### 2. Go 1.22+

Ubuntu's apt version of Go is often outdated. Install from the official tarball:

```bash
wget https://go.dev/dl/go1.22.3.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.22.3.linux-amd64.tar.gz

# Add to PATH in ~/.bashrc
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc

go version   # should print go1.22.3 or later
```

For ARM64 (e.g. Raspberry Pi, Jetson), replace `amd64` with `arm64`.

#### 3. librealsense2

Intel provides an official apt repository:

```bash
sudo mkdir -p /etc/apt/keyrings

curl -sSf https://librealsense.intel.com/Debian/librealsense.pgp \
  | sudo tee /etc/apt/keyrings/librealsense.pgp > /dev/null

echo "deb [signed-by=/etc/apt/keyrings/librealsense.pgp] \
  https://librealsense.intel.com/Debian/apt-repo \
  $(lsb_release -cs) main" \
  | sudo tee /etc/apt/sources.list.d/librealsense.list

sudo apt-get update
sudo apt-get install -y librealsense2-dkms librealsense2-dev librealsense2-utils
```

Verify:
```bash
rs-enumerate-devices   # lists connected RealSense cameras
```

#### 4. OpenCV

```bash
sudo apt-get install -y libopencv-dev
```

#### 5. udev rules

Without these you'll get `Permission denied` when opening the camera:

```bash
# librealsense2-dkms installs rules automatically; if missing, add manually:
echo 'SUBSYSTEMS=="usb", ATTRS{idVendor}=="8086", MODE="0666", GROUP="plugdev"' \
  | sudo tee /etc/udev/rules.d/99-realsense.rules

sudo udevadm control --reload-rules && sudo udevadm trigger
sudo usermod -aG plugdev $USER
# Log out and back in for the group change to take effect
```

#### 6. Build and run

```bash
make run
```

---

### Ubuntu-specific notes

- **Kernel module** — `librealsense2-dkms` installs a kernel patch that
  improves USB isochronous transfer reliability. Without it you may see
  higher frame drop counts, especially on USB 2 ports.

- **USB 3 required** — D435/D455 need USB 3.0 for 1280×720@30fps. Check:
  ```bash
  lsusb -t   # Intel device should show 5000M (USB 3), not 480M (USB 2)
  ```

- **VMware / VirtualBox** — USB passthrough is unreliable for isochronous
  devices. Use bare-metal or WSL2 with `usbipd`.

- **WSL2** — requires `usbipd-win` on the Windows host:
  ```powershell
  # PowerShell (admin)
  winget install usbipd
  usbipd list              # find RealSense bus ID, e.g. 2-3
  usbipd bind --busid 2-3
  usbipd attach --wsl --busid 2-3
  ```
  Then run `install.sh` inside WSL2 as normal.

---

## macOS Setup

### 1. Install Homebrew

```bash
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
```

### 2. Install dependencies

```bash
brew install go librealsense opencv
```

### 3. Demo mode (no hardware)

```bash
make run-demo
```

### 4. Real hardware build

Verify headers exist:
```bash
ls $(brew --prefix)/include/librealsense2/rs.h
ls $(brew --prefix)/include/opencv4/opencv2/imgcodecs.hpp
```

If Homebrew installed to `/opt/homebrew` (Apple Silicon), update `Makefile`:

```makefile
RS_INC ?= /opt/homebrew/include
RS_LIB ?= /opt/homebrew/lib
CV_INC ?= /opt/homebrew/include/opencv4
CV_LIB ?= /opt/homebrew/lib
```

Then:
```bash
make run
```

### macOS-specific notes

- No udev rules needed — plug in the camera and it's available.
- If the terminal can't open the camera: System Settings → Privacy & Security
  → Camera → grant access to Terminal.
- If `brew install librealsense` fails:
  ```bash
  brew tap IntelRealSense/librealsense
  brew install librealsense
  ```

---

## Running the Server

### Demo mode (no hardware)

```bash
make run-demo
# → http://localhost:5050
```

Opens two animated fake camera feeds. All controls work — recording, saving,
and SSE updates — without writing any files.

### Real hardware mode

```bash
make run
```

1. Plug cameras in **before** starting the server
2. Click **Detect** in the UI — preview streams start immediately
3. Assign names to each camera (optional)
4. Click **Start Recording** — pipelines open, recording begins within ~1.5s
5. Click **Stop** — `.bag` files are complete instantly (written continuously
   by librealsense2 during recording, not flushed at stop)

Recordings are saved to `recordings/DDMMYYYY/<camera_name>/`.

### Running the binary directly

```bash
# Demo
DEMO_MODE=1 ./realsense-server-demo

# Real hardware
./realsense-server
```

> The binary serves `index.html` from the same directory it lives in.

---

## Troubleshooting

### `fatal error: 'librealsense2/rs.h' file not found`
You ran `make run` without librealsense2 installed. Run `./install.sh` or
use `make run-demo` instead.

### `C++ source files not allowed when not using cgo: encode.cpp`
The `-tags demo` flag is missing. Ensure `encode.go` has `//go:build !demo`
on line 1 (before `package main`).

### `go: cannot find main module`
Run `go mod init realsense-capture` in the project directory.

### Camera feed frozen / ⚠ FROZEN badge
The freeze watchdog fires after 4s without a new MJPEG frame. Click **Detect**
to restart preview pipelines. Check the terminal for
`[preview <serial>] error:` lines.

### `Permission denied` opening camera (Linux)
udev rules missing or group not applied. Run `./install.sh` or see
[Step 5](#5-udev-rules) above. Remember to log out and back in after
`usermod -aG plugdev`.

### Frame drops at recording start
One drop on the very first frame is normal — it's the hardware FIFO filling
after pipeline open. The server performs a warm-up grab automatically before
signalling GO to suppress this. If drops continue, see below.

### Sustained frame drops during recording
- Use a USB 3.0 port (blue connector)
- Don't share the USB controller with other high-bandwidth devices
- Lower recording resolution in `camera.go`: change `1280, 720` → `848, 480`
- The `.bag` file is never affected by preview drops — librealsense2 writes
  the bag before the frame reaches Go

### Port 5050 already in use
```bash
lsof -i :5050   # find the process
kill -9 <PID>
# or change the port: addr := "0.0.0.0:5051" in main.go
```

### `r.PathValue("serial")` compile error
Go < 1.22. Upgrade via `install.sh` or the manual Go install steps.

---

## API Reference

All endpoints on port `5050`.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Serves `index.html` |
| `GET` | `/api/detect` | Detect cameras, start preview pipelines |
| `POST` | `/api/restart_previews` | Restart all preview pipelines (not during recording) |
| `GET` | `/api/stream/{serial}` | MJPEG stream — persistent connection, always live |
| `POST` | `/api/start_recording` | Body: `{"cameras":[{"serial":"…","user_name":"…"}]}` |
| `POST` | `/api/stop_recording` | Stop recording; bag files are already complete |
| `GET` | `/api/status` | Snapshot of current state |
| `GET` | `/api/events` | **SSE stream** — pushed on every state change, 1s RSS update |

### SSE event format

```json
{
  "status": "recording",
  "recording": true,
  "saved_files": [],
  "cameras": {
    "cam_12345678": {
      "name": "top_view",
      "serial": "12345678",
      "recording": true,
      "drops": 0
    }
  },
  "rss_bytes": 245366784
}
```

### Status values

| Value | Meaning |
|-------|---------|
| `idle` | Server running, no recording |
| `warming` | Pipelines opening (~300ms hardware stabilisation) |
| `recording` | Actively grabbing and writing frames to `.bag` |
| `saving` | Stop received, goroutines draining |
| `done` | All `.bag` files finalised |

---

## Tuning & Configuration

All tuning is done by editing source files and rebuilding.

| What | Where | Default |
|------|-------|---------|
| Preview frame rate (recording) | `main.go` — `previewEveryN` | 3 (→10fps) |
| Preview resolution (idle) | `main.go` — `downsample2x` call | 320×240 |
| Preview resolution (recording) | `main.go` — `downsample4x` call | 320×180 |
| Preview JPEG quality | `main.go` — `encodeJPEGSized(…, 65)` | 65 |
| Recording resolution | `camera.go` — `rec_open` stream config | 1280×720 |
| Recording FPS | `camera.go` — last arg in `rec_open` | 30 |
| GC aggressiveness | `main.go` — `debug.SetGCPercent(40)` | 40 |
| RSS hard cap | `main.go` — `debug.SetMemoryLimit(400 MB)` | 400 MB |
| Hardware stabilisation pause | `main.go` — `300 * time.Millisecond` | 300ms |
| Pipeline open timeout | `main.go` — `10 * time.Second` in warmup | 10s |
| HTTP port | `main.go` — `addr := "0.0.0.0:5050"` | 5050 |
| Recordings directory | `main.go` — `bagFilename()` | `./recordings/DDMMYYYY/` |
| Mock camera FPS | `mock.go` — `time.NewTicker(time.Second / 15)` | 15fps |
| Freeze watchdog threshold | `index.html` — `FREEZE_MS` | 4000ms |
| RAM gauge cap | `index.html` — `RAM_CAP` | 400 MB |

### Memory profile (2 cameras, recording)

| Component | RSS | Notes |
|-----------|-----|-------|
| librealsense2 SDK × 2 | ~160 MB | Fixed — Intel's USB isochronous buffers |
| Go runtime + HTTP | ~15 MB | Minimal |
| BGR pool buffers | ~8 MB | 2.76 MB × ~3 live at a time |
| Preview pool buffers | <1 MB | 320×240 × pool depth |
| JPEG frame slots | <1 MB | ~60 KB × 2 cameras |
| **Total typical** | **~200–230 MB** | Hard cap: 400 MB |

The 400 MB hard cap (`debug.SetMemoryLimit`) triggers aggressive GC if the
Go heap approaches that threshold. The dominant RSS cost is the SDK itself,
which is not controllable from Go.