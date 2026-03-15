# RealSense Capture Studio — Go Server

A lean, zero-dependency rewrite of the Python/Flask RealSense capture server.
Same HTTP API and `index.html` frontend — built with Go's native concurrency
instead of Python threads, and direct CGO calls to librealsense2/OpenCV instead
of going through the Python object layer.

---

## Table of Contents

1. [Project Structure](#project-structure)
2. [Build Modes](#build-modes)
3. [macOS Setup](#macos-setup)
4. [Ubuntu Setup](#ubuntu-setup)
5. [Running the Server](#running-the-server)
6. [Troubleshooting](#troubleshooting)
7. [API Reference](#api-reference)
8. [Tuning & Configuration](#tuning--configuration)

---

## Project Structure

```
realsense-go/
├── main.go           # HTTP server, goroutine orchestration, state management
├── camera.go         # CGO bridge to librealsense2  (real build only, //go:build !demo)
├── camera_demo.go    # Hardware stubs               (demo build only, //go:build demo)
├── encode.go         # CGO wrapper for OpenCV JPEG  (real build only, //go:build !demo)
├── encode_demo.go    # Pure-Go JPEG fallback         (demo build only, //go:build demo)
├── encode.cpp        # cv::imencode implementation   (compiled by CGO, !demo only)
├── mock.go           # Animated fake camera frames  (both builds)
├── index.html        # Frontend UI (unchanged from Python version)
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
> The `//go:build demo` tag on `encode.go` causes CGO to be inactive for that
> file, which means Go ignores `encode.cpp` entirely in demo builds.

---

## Build Modes

| Command | Hardware needed | SDK needed | Use case |
|---------|----------------|------------|----------|
| `make run-demo` | ✗ | ✗ | Development, UI work, CI |
| `make run` | ✓ | ✓ | Production with real cameras |
| `make demo` | ✗ | ✗ | Just build the demo binary |
| `make all` | ✗ | ✓ | Build real binary (run separately) |

---

## macOS Setup

### 1. Install Homebrew (if not already installed)

```bash
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
```

### 2. Install Go

```bash
brew install go
go version   # should print go1.22 or later
```

### 3. Demo mode — no extra dependencies needed

```bash
cd realsense-go
go mod init realsense-capture   # only needed once, creates go.mod
make run-demo
# → open http://localhost:5050
```

### 4. Real hardware build — install librealsense2 and OpenCV

```bash
brew install librealsense opencv
```

Verify the headers exist:
```bash
ls /opt/homebrew/include/librealsense2/rs.h   # Apple Silicon
ls /usr/local/include/librealsense2/rs.h       # Intel Mac
```

If Homebrew installed to `/opt/homebrew` (Apple Silicon M1/M2/M3), update the
Makefile paths:

```makefile
RS_INC ?= /opt/homebrew/include
RS_LIB ?= /opt/homebrew/lib
CV_INC ?= /opt/homebrew/include/opencv4
CV_LIB ?= /opt/homebrew/lib
```

Then build and run:
```bash
make run
```

### macOS-specific notes

- **USB permissions** — macOS does not require udev rules. Plug in the camera
  and it should be available immediately.
- **Apple Silicon (M1/M2/M3)** — Homebrew installs to `/opt/homebrew` instead
  of `/usr/local`. Always check your prefix with `brew --prefix`.
- **SIP / camera privacy** — if the terminal can't open the camera, go to
  System Settings → Privacy & Security → Camera and grant access to Terminal
  (or your IDE).
- **librealsense2 Homebrew formula** — as of 2024 the formula is `librealsense`.
  If `brew install librealsense` fails, try:
  ```bash
  brew tap IntelRealSense/librealsense
  brew install librealsense
  ```

---

## Ubuntu Setup

Tested on Ubuntu 20.04, 22.04, and 24.04.

### 1. Install Go

Ubuntu's apt version of Go is often outdated. Install from the official tarball:

```bash
# Replace 1.22.3 with the latest stable from https://go.dev/dl/
wget https://go.dev/dl/go1.22.3.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.22.3.linux-amd64.tar.gz

# Add to PATH — add these lines to ~/.bashrc or ~/.zshrc
export PATH=$PATH:/usr/local/go/bin
source ~/.bashrc

go version   # should print go1.22.3
```

### 2. Demo mode — no extra dependencies needed

```bash
cd realsense-go
go mod init realsense-capture   # only needed once
make run-demo
# → open http://localhost:5050
```

### 3. Real hardware build — install librealsense2

Intel provides an official apt repository:

```bash
# Register the Intel apt server key
sudo mkdir -p /etc/apt/keyrings
curl -sSf https://librealsense.intel.com/Debian/librealsense.pgp \
  | sudo tee /etc/apt/keyrings/librealsense.pgp > /dev/null

# Add the repository
echo "deb [signed-by=/etc/apt/keyrings/librealsense.pgp] \
  https://librealsense.intel.com/Debian/apt-repo \
  $(lsb_release -cs) main" \
  | sudo tee /etc/apt/sources.list.d/librealsense.list

sudo apt update
sudo apt install -y librealsense2-dkms librealsense2-dev librealsense2-utils
```

Verify:
```bash
rs-enumerate-devices   # should list connected RealSense cameras
```

### 4. Install OpenCV

```bash
sudo apt install -y libopencv-dev
```

Verify headers:
```bash
ls /usr/include/opencv4/opencv2/imgcodecs.hpp
```

If OpenCV installed to a non-standard path, update the Makefile:
```makefile
CV_INC ?= /usr/include/opencv4
CV_LIB ?= /usr/lib/x86_64-linux-gnu
```

### 5. Build and run

```bash
make run
```

### Ubuntu-specific notes

- **udev rules** — without these, you'll get a "Permission denied" error when
  opening the camera as a non-root user. Fix:
  ```bash
  sudo cp /etc/udev/rules.d/99-realsense-libusb.rules /etc/udev/rules.d/
  # or manually:
  echo 'SUBSYSTEMS=="usb", ATTRS{idVendor}=="8086", MODE="0666", GROUP="plugdev"' \
    | sudo tee /etc/udev/rules.d/99-realsense.rules
  sudo udevadm control --reload-rules && sudo udevadm trigger
  sudo usermod -aG plugdev $USER
  # log out and back in for the group change to take effect
  ```

- **Kernel module** — the `librealsense2-dkms` package installs a kernel patch
  that improves USB isochronous transfer reliability. If you skip it, you may
  see higher frame drop counts on USB 2 ports.

- **USB 3 required** — D435/D455 cameras need USB 3.0 for 1280×720@30fps.
  Plugging into a USB 2 port will either fail to open or produce heavy drops.
  Check:
  ```bash
  lsusb -t   # look for "Intel" at 5000M (USB 3) not 480M (USB 2)
  ```

- **VMware / VirtualBox** — USB passthrough often works but is unreliable for
  isochronous devices like RealSense. Bare-metal or WSL2 with USB passthrough
  (via `usbipd`) is more reliable.

- **WSL2 on Ubuntu** — requires `usbipd-win` on the Windows host to forward
  the USB device into WSL2:
  ```powershell
  # On Windows (PowerShell as admin)
  winget install usbipd
  usbipd list                      # find the RealSense bus ID e.g. 2-3
  usbipd bind --busid 2-3
  usbipd attach --wsl --busid 2-3
  ```
  Then inside WSL2, install librealsense2 as above.

---

## Running the Server

### Demo mode (no hardware)

```bash
make run-demo
```

Opens two animated fake camera feeds at http://localhost:5050.
All recording controls work — they simulate warmup, recording, and saving
without writing any files.

### Real hardware mode

```bash
make run
```

- Plug cameras in **before** starting the server
- Click **Detect Cameras** in the UI
- Assign names to each camera
- Click **Start Recording** — 3-second warmup, then recording begins
- Click **Stop Recording** — `.bag` files are saved to `recordings/DDMMYYYY/`

### Running the binary directly

```bash
# Demo
DEMO_MODE=1 ./realsense-server-demo

# Real (binary must be in same dir as index.html)
./realsense-server
```

> **Note:** The binary serves `index.html` from the same directory it lives in.
> If you move the binary, move `index.html` with it, or symlink it.

---

## Troubleshooting

### `fatal error: 'librealsense2/rs.h' file not found`
You ran `make run` (real build) without librealsense2 installed.
Use `make run-demo` instead, or install the SDK first.

### `C++ source files not allowed when not using cgo: encode.cpp`
The `encode.cpp` file must stay in the package root. The `//go:build !demo`
tag on `encode.go` causes Go to exclude the CGO compilation entirely in demo
builds, which makes it ignore `encode.cpp`. If you see this error, make sure
you are using `-tags demo` and that `encode.go` has the correct build tag at
the very top of the file (line 1, before the package declaration).

### `go: cannot find main module`
You need a `go.mod` file in the project directory:
```bash
go mod init realsense-capture
```

### `Makefile:XX: warning: overriding commands for target`
You have a duplicate target in your Makefile (e.g. two `demo:` rules from
copy-pasting fixes). Open the Makefile and remove the duplicate.

### Camera feed is frozen / shows ⚠ FROZEN badge
The browser's freeze watchdog fires after 3.5 seconds without a new frame.
Click **Detect Cameras** (or Re-scan) to restart the preview pipelines.
On the server side, check the terminal for `[preview <serial>] error:` lines.

### `Permission denied` opening camera (Linux)
udev rules are missing. See the [Ubuntu-specific notes](#ubuntu-specific-notes)
section above.

### High frame drop count
- Use a USB 3.0 port (blue tab inside the port)
- Don't share the USB controller with other high-bandwidth devices
- Lower recording resolution in `camera.go`: change `1280, 720` to `848, 480`
- The `.bag` file is never affected by preview drops — the bag is written
  directly by librealsense2 before the frame reaches Go

### Port 5050 already in use
```bash
lsof -i :5050        # find what's using it
kill -9 <PID>
# or change the port in main.go: addr := "0.0.0.0:5051"
```

### `r.PathValue("serial")` compile error
Your Go version is older than 1.22. `r.PathValue` was added in Go 1.22.
Upgrade Go (see Ubuntu setup step 1).

---

## API Reference

All endpoints are on port `5050`. The frontend talks to them automatically.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Serves `index.html` |
| `GET` | `/api/detect` | Detect cameras, start preview pipelines. Returns list of `{index, serial, name}`. |
| `POST` | `/api/restart_previews` | Tear down and restart all active preview pipelines. Safe to call when not recording. |
| `GET` | `/api/stream/{serial}` | MJPEG stream for one camera. Opens a persistent HTTP connection. |
| `POST` | `/api/start_recording` | Body: `{"cameras": [{"serial": "...", "user_name": "..."}]}`. Starts warmup + recording. |
| `POST` | `/api/stop_recording` | Stops recording, finalizes `.bag` files. |
| `GET` | `/api/status` | Returns `{status, recording, cameras, saved_files}`. Polled every 500ms by the UI. |

### Status values

| Value | Meaning |
|-------|---------|
| `idle` | Nothing happening |
| `warming` | Pipelines open, 3-second countdown running |
| `recording` | Actively grabbing and writing frames |
| `saving` | Stop signal sent, threads draining |
| `done` | All `.bag` files finalized |

---

## Tuning & Configuration

All tuning is done by editing source files and rebuilding.

| What | Where | Default |
|------|-------|---------|
| Preview FPS | `main.go` — `skipNext` toggle in `previewWorker` | ~15 fps (every other frame) |
| Preview resolution | `camera.go` — `rs2_config_enable_stream` in `preview_open` | 640×480 |
| Recording resolution | `camera.go` — `rs2_config_enable_stream` in `rec_open` | 1280×720 |
| Recording FPS | `camera.go` — last arg to `rs2_config_enable_stream` in `rec_open` | 30 |
| JPEG quality (preview) | `main.go` — `encodeJPEG(frame, 75)` | 75 |
| JPEG quality (rec overlay) | `main.go` — `encodeJPEG(img, 70)` | 70 |
| Frame drop buffer size | `main.go` — `make(chan []byte, 8)` in `cameraWorker` | 8 frames |
| Warmup timeout | `main.go` — `10 * time.Second` in `handleStartRecording` | 10s |
| Warmup countdown | `main.go` — `3 * time.Second` in the warmup goroutine | 3s |
| HTTP port | `main.go` — `addr := "0.0.0.0:5050"` | 5050 |
| Recordings directory | `main.go` — `bagFilename()` | `./recordings/DDMMYYYY/` |
| Mock camera FPS | `mock.go` — `time.NewTicker(time.Second / 15)` | 15 fps |
| Freeze watchdog threshold | `index.html` — `FREEZE_THRESHOLD` | 3500ms |
