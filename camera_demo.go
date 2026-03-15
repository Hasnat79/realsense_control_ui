//go:build demo

// camera_demo.go — hardware stubs for -tags demo builds.
// No CGO, no librealsense2, no OpenCV required.

package main

import "errors"

func realsenseAvailable() bool { return false }

type DeviceInfo struct{ Serial, Name string }

func DetectCameras() ([]DeviceInfo, error) {
	return nil, errors.New("realsense not available in demo mode")
}

type PreviewPipeline struct{}

func NewPreviewPipeline(_ string) (*PreviewPipeline, error) {
	return nil, errors.New("realsense not available in demo mode")
}

func (p *PreviewPipeline) WaitForFrameInto(_ *[]byte, _ int) ([]byte, error) {
	return nil, errors.New("demo")
}

func (p *PreviewPipeline) Close() {}

type Camera struct{}

func NewCamera(_, _ string) (*Camera, error) { return &Camera{}, nil }

func (c *Camera) Start(_ string) error { return errors.New("realsense not available in demo mode") }

func (c *Camera) GrabFrameInto(_ *[]byte, _ int) (width, height, stride int, err error) {
	return 0, 0, 0, errors.New("demo")
}

func (c *Camera) Stop() {}