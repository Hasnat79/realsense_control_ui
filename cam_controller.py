

from utils import detect_connected_cameras,generate_camera_ids, bag_to_depth_video
import pyrealsense2 as rs
from camera import Camera
import time
import threading


def camera_worker(cam: Camera, start_event: threading.Event, duration: float, bag_path: str):
    # setup
    cam.start(output_bag=bag_path)

    try:
        # wait until main thread says "GO"
        start_event.wait()

        t0 = time.time()
        while time.time() - t0 < duration:
            frames = cam.grab_frame()
            # optional processing...
    finally:
        cam.stop()



if __name__ == "__main__":

    devices = detect_connected_cameras()
    camera_ids = generate_camera_ids(devices)
    cam_1 = Camera('cam_1', camera_ids['cam_1'])
    cam_2 = Camera('cam_2', camera_ids['cam_2'])

    start_event = threading.Event()
    duration = 10

    t1 = threading.Thread(target=camera_worker,
                          args=(cam_1, start_event, duration, "cam_1_recording.bag"))
    t2 = threading.Thread(target=camera_worker,
                          args=(cam_2, start_event, duration, "cam_2_recording.bag"))

    t1.start()
    t2.start()

    # when both threads have called start() and are waiting,
    # release them at (almost) the same time:
    time.sleep(0.5)  # small delay to let them init
    start_event.set()

    t1.join()
    t2.join()

    cam_1.export_bag_to_mp4("cam_1_recording.bag", "cam_1_depth_video.mp4")
    cam_2.export_bag_to_mp4("cam_2_recording.bag", "cam_2_depth_video.mp4")