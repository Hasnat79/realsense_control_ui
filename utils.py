
import pyrealsense2 as rs
import numpy as np
import cv2

def detect_connected_cameras():
    # Placeholder for camera detection logic
    # This function should return a list of connected camera identifiers
    ctx = rs.context()
    devices = ctx.query_devices()
    if len(devices) == 0:
        raise RuntimeError("No RealSense device connected")
    print("Found RealSense devices:")
    for dev in devices:
        print(f"  {dev.get_info(rs.camera_info.name)} "
              f"({dev.get_info(rs.camera_info.serial_number)})")
    return devices





def generate_camera_ids(devices):
    # return cam_1, cam_2, and real ids as a dictionary etc. based on the number of devices
    return {f"cam_{i+1}": dev.get_info(rs.camera_info.serial_number) for i, dev in enumerate(devices)}


def bag_to_depth_video(bag_path, output_path="depth_colormap.mp4",
                       fps=30, colormap=cv2.COLORMAP_JET):
    # Set up pipeline to read from bag file
    pipeline = rs.pipeline()
    config = rs.config()
    config.enable_device_from_file(bag_path, repeat_playback=False)

    # Configure playback
    profile = pipeline.start(config)
    playback = profile.get_device().as_playback()
    playback.set_real_time(False)  # process as fast as possible

    depth_stream = profile.get_stream(rs.stream.depth).as_video_stream_profile()
    width = depth_stream.width()
    height = depth_stream.height()

    # OpenCV video writer (H.264 or MPEG-4)
    fourcc = cv2.VideoWriter_fourcc(*"mp4v")
    writer = cv2.VideoWriter(output_path, fourcc, fps, (width, height))

    if not writer.isOpened():
        raise RuntimeError("Failed to open VideoWriter; check codec/ffmpeg install.")

    try:
        while True:
            try:
                frames = pipeline.wait_for_frames()
            except RuntimeError:
                # End of bag or read error
                break

            depth_frame = frames.get_depth_frame()
            if not depth_frame:
                continue

            # Convert depth frame to numpy array
            depth_image = np.asanyarray(depth_frame.get_data())

            # Normalize depth to 0‑255 for visualization
            # Adjust max_range_m depending on scene
            max_range_m = 5.0
            depth_scaled = np.clip(depth_image * (255.0 / (max_range_m * 1000.0)),
                                   0, 255).astype(np.uint8)

            # Apply OpenCV color map
            depth_color = cv2.applyColorMap(depth_scaled, colormap)

            # Write frame to video
            writer.write(depth_color)

    finally:
        pipeline.stop()
        writer.release()

    print(f"Saved color‑mapped depth video to: {output_path}")