#!/bin/bash
set -e

# Ensure uv is installed
if ! command -v uv &> /dev/null; then
    echo "uv could not be found. Please install it (e.g., pip install uv)."
    exit 1
fi

echo "Creating virtual environment..."
rm -rf .venv
uv venv .venv
source .venv/bin/activate

echo "Installing dependencies..."
# dlib installation might take time. Assuming cmake is installed.
uv pip install face_recognition opencv-python pyinstaller setuptools

echo "Building executable..."
# --hidden-import might be needed for some face_recognition sub-dependencies if lazy loaded, 
# but usually face_recognition works fine.
pyinstaller --onefile --name face_scanner_tool --distpath dist face_scanner.py

echo "Build complete. Executable is at client/dist/face_scanner_tool"
