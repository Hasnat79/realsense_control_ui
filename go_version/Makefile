# Makefile — RealSense Capture Server (Go)
# ==========================================
# Targets:
#   make demo       build demo binary (no hardware required)
#   make run-demo   build + run demo
#   make all        build real binary (requires librealsense2 + OpenCV4)
#   make run        build + run real
#   make clean      remove binaries

BINARY      := realsense-server
DEMO_BINARY := realsense-server-demo
GOFLAGS     := -trimpath -ldflags="-s -w"

# Adjust these paths if your libs are installed elsewhere
RS_INC  ?= /usr/local/include
RS_LIB  ?= /usr/local/lib
CV_INC  ?= /usr/local/include/opencv4
CV_LIB  ?= /usr/local/lib

.PHONY: all demo run run-demo clean

# ── Demo build (no CGO, no hardware libs) ─────────────────────────────────────
demo:
	go build $(GOFLAGS) -tags demo -o $(DEMO_BINARY) .

run-demo: demo
	DEMO_MODE=1 ./$(DEMO_BINARY)

# ── Real hardware build ────────────────────────────────────────────────────────
all:
	CGO_CFLAGS="-I$(RS_INC) -I$(CV_INC) -O3 -march=native" \
	CGO_LDFLAGS="-L$(RS_LIB) -L$(CV_LIB) -lrealsense2 -lopencv_core -lopencv_imgcodecs -Wl,-rpath,$(RS_LIB)" \
	go build $(GOFLAGS) -o $(BINARY) .

run: all
	./$(BINARY)

clean:
	rm -f $(BINARY) $(DEMO_BINARY)
