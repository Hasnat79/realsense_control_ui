
import cv2
import numpy as np
import pyrealsense2 as rs
import time
class Camera:
    def __init__(self, name, serial):
        self.name = name
        self.serial = serial
        self.pipeline = None
        self.config = None
    
    def start(self, width=640, height=480, fps=30, output_bag=None):
        self.pipeline = rs.pipeline()
        self.config = rs.config()
        self.config.enable_device(self.serial)
        self.config.enable_stream(rs.stream.depth, width, height, rs.format.z16, fps)
        self.config.enable_stream(rs.stream.color, width, height, rs.format.bgr8, fps)

        if output_bag is not None:
            print(f"[{self.name}] recording to {output_bag}")
            self.config.enable_record_to_file(output_bag)

        self.pipeline.start(self.config)
    
    def stop(self):
        if self.pipeline is not None:
            self.pipeline.stop()
            self.pipeline = None
            self.config = None
            print(f"[{self.name}] stopped.")

    def grab_frame(self, timeout_ms=5000):
        if self.pipeline is None:
            raise RuntimeError(f"{self.name} not started")
        return self.pipeline.wait_for_frames(timeout_ms=timeout_ms)
    def record_for(self, duration=6):
        start_time = time.time()
        while time.time() - start_time < duration:
            frames = self.grab_frame()
            # optional quick checks here
            if not frames:
                print(f"[{self.name}] Warning: no frames received")
        


    def enable_streams(self, width=640, height=480, fps=30):
        self.config.enable_stream(rs.stream.depth, width, height, rs.format.z16, fps)
        self.config.enable_stream(rs.stream.color, width, height, rs.format.bgr8, fps)

    def record(self, duration=10, width=640, height=480, fps=30, output_bag=None):
        pipeline = rs.pipeline()
        config = rs.config()
        # enable cam
        config.enable_device(self.serial)

        # enable common streams
        config.enable_stream(rs.stream.depth, width, height, rs.format.z16, fps)
        config.enable_stream(rs.stream.color, width, height, rs.format.bgr8, fps)
        print(f"Starting recording for {self.name} to {output_bag} for {duration} seconds...")
        config.enable_record_to_file(output_bag)

        profile = pipeline.start(config)

        try:
            start_time = time.time()
            while time.time() - start_time < duration:
                # Wait for the next set of frames; this also keeps recording going
                frames = pipeline.wait_for_frames(timeout_ms=5000)
                # You can add quick checks here if you want to verify frames
                if not frames:
                    print("Warning: no frames received")
            
        except Exception as e:
            print(f"Error during recording: {e}")
        finally:
            # 4. Stop pipeline and finalize file
            pipeline.stop()
            print("Recording finished and saved.")
            del pipeline
            del config

        
        # if export_mp4 and output_bag:
        #         self._export_bag_to_mp4(output_bag, f"{self.name}_depth_video.mp4")



    def export_bag_to_mp4(self, bag_path, output_path, fps=30,
                       colormap=cv2.COLORMAP_JET):
    # NEW, independent pipeline & config for playback
        pipeline = rs.pipeline()
        config = rs.config()

        config.enable_device_from_file(bag_path, repeat_playback=False)
        profile = pipeline.start(config)

        playback = profile.get_device().as_playback()
        playback.set_real_time(False)

        depth_stream = profile.get_stream(rs.stream.depth).as_video_stream_profile()
        width = depth_stream.width()
        height = depth_stream.height()

        fourcc = cv2.VideoWriter_fourcc(*"mp4v")
        writer = cv2.VideoWriter(output_path, fourcc, fps, (width, height))
        if not writer.isOpened():
            raise RuntimeError("Failed to open VideoWriter; check codec/ffmpeg install.")

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
                    0, 255
                ).astype(np.uint8)

                depth_color = cv2.applyColorMap(depth_scaled, colormap)
                writer.write(depth_color)
        finally:
            pipeline.stop()
            writer.release()
            del pipeline
            del config

        print(f"Saved color‑mapped depth video to: {output_path}")
