# sudo apt install -y \
#     build-essential cmake git \
#     libssl-dev libusb-1.0-0-dev libudev-dev pkg-config

conda create -n test python=3.10 -y
conda activate cam
pip install pyrealsense2
sudo apt install -y ffmpeg
conda install -y -c conda-forge opencv
pip install "flask>=3.0.0"
pip install "flask-cors>=4.0.0"
