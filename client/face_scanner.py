import sys
import json
import cv2
import face_recognition
import base64
import numpy as np

def process_video(video_path):
    video_capture = cv2.VideoCapture(video_path)
    if not video_capture.isOpened():
        print(json.dumps({"error": "Could not open video file"}))
        return

    # Process every 1 second
    interval = 1.0 # seconds
    next_process_time = 0.0
    
    results = []

    while True:
        success, frame = video_capture.read()
        if not success:
            break
            
        timestamp = video_capture.get(cv2.CAP_PROP_POS_MSEC) / 1000.0
        
        if timestamp >= next_process_time:
            # Convert the image from BGR color (which OpenCV uses) to RGB color (which face_recognition uses)
            rgb_frame = frame[:, :, ::-1]
            
            # Find all the faces and face encodings in the current frame of video
            # model='hog' is faster, 'cnn' is more accurate (but requires GPU/takes longer)
            face_locations = face_recognition.face_locations(rgb_frame, model='hog')
            face_encodings = face_recognition.face_encodings(rgb_frame, face_locations)
            
            for (top, right, bottom, left), face_encoding in zip(face_locations, face_encodings):
                # correct types for json dump
                encoding_list = face_encoding.tolist()
                
                # Create thumbnail
                # Ensure coordinates are within bounds
                h, w, _ = frame.shape
                top = max(0, top)
                left = max(0, left)
                bottom = min(h, bottom)
                right = min(w, right)
                
                face_image = frame[top:bottom, left:right]
                
                # Resize to reduce size, e.g., max 150px dimension
                fh, fw = face_image.shape[:2]
                if fh > 0 and fw > 0:
                    scale = min(150.0/fh, 150.0/fw)
                    # Only scale down if processing large faces
                    if scale < 1.0:
                        face_image = cv2.resize(face_image, (0,0), fx=scale, fy=scale)
                    
                    # Encode to jpg
                    _, buffer = cv2.imencode('.jpg', face_image, [int(cv2.IMWRITE_JPEG_QUALITY), 80])
                    thumb_b64 = base64.b64encode(buffer).decode('utf-8')
                    
                    results.append({
                        "timestamp": round(timestamp, 2),
                        "encoding": encoding_list,
                        "thumb": thumb_b64
                    })
            
            next_process_time += interval

    video_capture.release()
    print(json.dumps(results))

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(json.dumps({"error": "Usage: python face_scanner.py <video_path>"}))
        sys.exit(1)
        
    process_video(sys.argv[1])
