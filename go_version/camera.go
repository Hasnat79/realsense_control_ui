//go:build !demo

// camera.go — CGO bridge to librealsense2
// Build requires: librealsense2-dev, libopencv-dev
// Install: sudo apt install librealsense2-dev libopencv-dev
//          brew install librealsense opencv   (macOS)

package main

/*
#cgo CFLAGS:  -I/usr/local/include
#cgo LDFLAGS: -L/usr/local/lib -lrealsense2

#include <librealsense2/rs.h>
#include <librealsense2/h/rs_pipeline.h>
#include <stdlib.h>
#include <string.h>

// ── device detection ────────────────────────────────────────────────────────
typedef struct { char serial[32]; char name[128]; } rs_dev_info_t;

static int list_devices(rs_dev_info_t* out, int max_n) {
    rs2_error* e = NULL;
    rs2_context* ctx = rs2_create_context(RS2_API_VERSION, &e);
    if (e) { rs2_free_error(e); return 0; }
    rs2_device_list* dl = rs2_query_devices(ctx, &e);
    if (e) { rs2_free_error(e); rs2_delete_context(ctx); return 0; }
    int n = rs2_get_device_count(dl, &e);
    if (n > max_n) n = max_n;
    for (int i = 0; i < n; i++) {
        rs2_device* dev = rs2_create_device(dl, i, &e);
        if (e) { rs2_free_error(e); e = NULL; continue; }
        const char* sn   = rs2_get_device_info(dev, RS2_CAMERA_INFO_SERIAL_NUMBER, &e);
        const char* name = rs2_get_device_info(dev, RS2_CAMERA_INFO_NAME, &e);
        if (sn)   strncpy(out[i].serial, sn,   sizeof(out[i].serial)-1);
        if (name) strncpy(out[i].name,   name,  sizeof(out[i].name)-1);
        rs2_delete_device(dev);
    }
    rs2_delete_device_list(dl);
    rs2_delete_context(ctx);
    return n;
}

// ── shared frame grab helper ─────────────────────────────────────────────────
// Copies the latest color frame into a malloc'd buffer. Caller must free().
static void* grab_color(rs2_pipeline* pipe, int timeout_ms,
                        int* w, int* h, int* stride) {
    rs2_error* e = NULL;
    rs2_frame* frames = rs2_pipeline_wait_for_frames(pipe, timeout_ms, &e);
    if (e) { rs2_free_error(e); return NULL; }
    void* data = NULL;
    int n = rs2_embedded_frames_count(frames, &e);
    for (int i = 0; i < n; i++) {
        rs2_frame* f = rs2_extract_frame(frames, i, &e);
        rs2_stream_profile* p =
            (rs2_stream_profile*)rs2_get_frame_stream_profile(f, &e);
        rs2_stream s; rs2_format fmt; int idx, uid, fps;
        rs2_get_stream_profile_data(p, &s, &fmt, &idx, &uid, &fps, &e);
        if (s == RS2_STREAM_COLOR && data == NULL) {
            *w      = rs2_get_frame_width(f, &e);
            *h      = rs2_get_frame_height(f, &e);
            *stride = rs2_get_frame_stride_in_bytes(f, &e);
            int sz  = (*stride) * (*h);
            data    = malloc(sz);
            if (data) memcpy(data, rs2_get_frame_data(f, &e), sz);
        }
        rs2_release_frame(f);
    }
    rs2_release_frame(frames);
    return data;
}

// ── preview pipeline ─────────────────────────────────────────────────────────
typedef struct {
    rs2_pipeline*         pipe;
    rs2_pipeline_profile* profile;
    rs2_config*           cfg;
} preview_pipe_t;

static preview_pipe_t* preview_open(const char* serial) {
    rs2_error* e = NULL;
    preview_pipe_t* pp = (preview_pipe_t*)calloc(1, sizeof(preview_pipe_t));
    rs2_context* ctx = rs2_create_context(RS2_API_VERSION, &e);
    pp->pipe    = rs2_create_pipeline(ctx, &e);
    rs2_delete_context(ctx);
    pp->cfg     = rs2_create_config(&e);
    rs2_config_enable_device(pp->cfg, serial, &e);
    rs2_config_enable_stream(pp->cfg, RS2_STREAM_COLOR,
                             -1, 640, 480, RS2_FORMAT_BGR8, 30, &e);
    pp->profile = rs2_pipeline_start_with_config(pp->pipe, pp->cfg, &e);
    if (e) { rs2_free_error(e); free(pp); return NULL; }
    return pp;
}

static void* preview_grab(preview_pipe_t* pp, int timeout_ms,
                          int* w, int* h, int* stride) {
    return grab_color(pp->pipe, timeout_ms, w, h, stride);
}

static void preview_close(preview_pipe_t* pp) {
    if (!pp) return;
    rs2_error* e = NULL;
    rs2_pipeline_stop(pp->pipe, &e);
    rs2_delete_pipeline_profile(pp->profile);
    rs2_delete_config(pp->cfg);
    rs2_delete_pipeline(pp->pipe);
    free(pp);
}

// ── recording pipeline ────────────────────────────────────────────────────────
typedef struct {
    rs2_pipeline*         pipe;
    rs2_pipeline_profile* profile;
    rs2_config*           cfg;
} rec_pipe_t;

static rec_pipe_t* rec_open(const char* serial, const char* bag_path) {
    rs2_error* e = NULL;
    rec_pipe_t* rp = (rec_pipe_t*)calloc(1, sizeof(rec_pipe_t));
    rs2_context* ctx = rs2_create_context(RS2_API_VERSION, &e);
    rp->pipe    = rs2_create_pipeline(ctx, &e);
    rs2_delete_context(ctx);
    rp->cfg     = rs2_create_config(&e);
    rs2_config_enable_device(rp->cfg, serial, &e);
    rs2_config_enable_record_to_file(rp->cfg, bag_path, &e);
    rs2_config_enable_stream(rp->cfg, RS2_STREAM_COLOR,
                             -1, 1280, 720, RS2_FORMAT_BGR8, 30, &e);
    rs2_config_enable_stream(rp->cfg, RS2_STREAM_DEPTH,
                             -1, 1280, 720, RS2_FORMAT_Z16,  30, &e);
    rp->profile = rs2_pipeline_start_with_config(rp->pipe, rp->cfg, &e);
    if (e) { rs2_free_error(e); free(rp); return NULL; }
    return rp;
}

static void* rec_grab(rec_pipe_t* rp, int timeout_ms,
                      int* w, int* h, int* stride) {
    return grab_color(rp->pipe, timeout_ms, w, h, stride);
}

static void rec_close(rec_pipe_t* rp) {
    if (!rp) return;
    rs2_error* e = NULL;
    rs2_pipeline_stop(rp->pipe, &e);
    rs2_delete_pipeline_profile(rp->profile);
    rs2_delete_config(rp->cfg);
    rs2_delete_pipeline(rp->pipe);
    free(rp);
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

func realsenseAvailable() bool { return true }

// ─── Device detection ─────────────────────────────────────────────────────────

type DeviceInfo struct{ Serial, Name string }

func DetectCameras() ([]DeviceInfo, error) {
	const maxDevs = 16
	arr := make([]C.rs_dev_info_t, maxDevs)
	n := int(C.list_devices(&arr[0], C.int(maxDevs)))
	devs := make([]DeviceInfo, n)
	for i := range devs {
		devs[i] = DeviceInfo{
			Serial: C.GoString(&arr[i].serial[0]),
			Name:   C.GoString(&arr[i].name[0]),
		}
	}
	return devs, nil
}

// ─── Preview pipeline ─────────────────────────────────────────────────────────

type PreviewPipeline struct{ ptr *C.preview_pipe_t }

func NewPreviewPipeline(serial string) (*PreviewPipeline, error) {
	cs := C.CString(serial)
	defer C.free(unsafe.Pointer(cs))
	ptr := C.preview_open(cs)
	if ptr == nil {
		return nil, errors.New("failed to open preview pipeline: " + serial)
	}
	return &PreviewPipeline{ptr: ptr}, nil
}

func (p *PreviewPipeline) WaitForFrame(timeoutMS int) ([]byte, error) {
	var w, h, stride C.int
	data := C.preview_grab(p.ptr, C.int(timeoutMS), &w, &h, &stride)
	if data == nil {
		return nil, errors.New("timeout")
	}
	defer C.free(data)
	sz := int(stride) * int(h)
	buf := make([]byte, sz)
	copy(buf, unsafe.Slice((*byte)(data), sz))
	return buf, nil
}

func (p *PreviewPipeline) Close() { C.preview_close(p.ptr) }

// ─── Recording pipeline ───────────────────────────────────────────────────────

type Camera struct {
	name, serial string
	ptr          *C.rec_pipe_t
}

func NewCamera(name, serial string) (*Camera, error) {
	return &Camera{name: name, serial: serial}, nil
}

func (c *Camera) Start(bagPath string) error {
	cs := C.CString(c.serial)
	cb := C.CString(bagPath)
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(cb))
	c.ptr = C.rec_open(cs, cb)
	if c.ptr == nil {
		return errors.New("failed to open recording pipeline: " + c.serial)
	}
	return nil
}

func (c *Camera) GrabFrame(timeoutMS int) ([]byte, error) {
	var w, h, stride C.int
	data := C.rec_grab(c.ptr, C.int(timeoutMS), &w, &h, &stride)
	if data == nil {
		return nil, errors.New("frame timeout")
	}
	defer C.free(data)
	sz := int(stride) * int(h)
	buf := make([]byte, sz)
	copy(buf, unsafe.Slice((*byte)(data), sz))
	return buf, nil
}

func (c *Camera) Stop() {
	if c.ptr != nil {
		C.rec_close(c.ptr)
		c.ptr = nil
	}
}
