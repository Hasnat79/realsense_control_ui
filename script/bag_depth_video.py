#!/usr/bin/env python3

import sys
import os
import cv2
import pyrealsense2 as rs
import numpy as np
from pathlib import Path


def bag_to_depth_video(bag_path, output_path, fps=30, colormap=cv2.COLORMAP_JET):

    pipeline = rs.pipeline()
    config = rs.config()
    config.enable_device_from_file(str(bag_path), repeat_playback=False)

    profile = pipeline.start(config)
    playback = profile.get_device().as_playback()
    playback.set_real_time(False)

    depth_stream = profile.get_stream(rs.stream.depth).as_video_stream_profile()
    width = depth_stream.width()
    height = depth_stream.height()

    fourcc = cv2.VideoWriter_fourcc(*"mp4v")
    writer = cv2.VideoWriter(str(output_path), fourcc, fps, (width, height))

    if not writer.isOpened():
        raise RuntimeError("Failed to open VideoWriter")

    try:
        while True:
            try:
                frames = pipeline.wait_for_frames()
            except RuntimeError:
                break

            depth_frame = frames.get_depth_frame()
            if not depth_frame:
                continue

            depth_image = np.asanyarray(depth_frame.get_data())

            max_range_m = 5.0
            depth_scaled = np.clip(
                depth_image * (255.0 / (max_range_m * 1000.0)),
                0,
                255
            ).astype(np.uint8)

            depth_color = cv2.applyColorMap(depth_scaled, colormap)

            writer.write(depth_color)

    finally:
        pipeline.stop()
        writer.release()

    print(f"Saved: {output_path}")


def process_directory(bag_dir, output_dir):

    bag_dir = Path(bag_dir)
    output_dir = Path(output_dir)

    output_dir.mkdir(parents=True, exist_ok=True)

    bag_files = sorted(bag_dir.glob("*.bag"))

    if not bag_files:
        print("No .bag files found.")
        return

    print(f"Found {len(bag_files)} bag files")

    for bag_file in bag_files:

        out_file = output_dir / f"{bag_file.stem}.mp4"

        print(f"Processing {bag_file.name} → {out_file.name}")

        try:
            bag_to_depth_video(bag_file, out_file)
        except Exception as e:
            print(f"Failed: {bag_file} ({e})")


if __name__ == "__main__":

    if len(sys.argv) < 2:
        print("Usage:")
        print("python bag_to_depth_video.py path/to/bag_dir [output_dir]")
        sys.exit(1)

    bag_directory = sys.argv[1]
    output_directory = sys.argv[2] if len(sys.argv) > 2 else "depth_videos"

    process_directory(bag_directory, output_directory)