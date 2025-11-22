package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// ---- 1. Limit upload size to 1GB ----
	const maxUploadSize = 1 << 30 // 1GB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	// ---- 2. Extract and validate videoID ----
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}

	// ---- 3. Authenticate user ----
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Missing or invalid authorization header", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	// ---- 4. Fetch video metadata from DB ----
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't find video", err)
		return
	}

	// ---- 5. Ensure the uploader owns the video ----
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to modify this video", nil)
		return
	}

	// ---- 6. Parse the uploaded video file ----
	err = r.ParseMultipartForm(maxUploadSize)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Failed to parse multipart form", err)
		return
	}

	videoFile, videoHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Missing 'video' file in form data", err)
		return
	}
	defer videoFile.Close()

	// ---- 7. Validate MIME type ----
	contentType := videoHeader.Header.Get("Content-Type")
	if contentType == "" {
		respondWithError(w, http.StatusBadRequest, "Missing Content-Type header", nil)
		return
	}

	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type: only video/mp4 allowed", nil)
		return
	}

	// ---- 8. Save to a temporary file ----
	tempFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temporary file", err)
		return
	}
	defer func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
	}()

	// Copy from multipart to temp file
	if _, err := io.Copy(tempFile, videoFile); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to write video to temporary file", err)
		return
	}

	// Reset pointer to beginning for re-read
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to reset file pointer", err)
		return
	}

	// ---- Get Aspect Ratio ----
	ratio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to read video metadata", err)
		return
	}

	// ---- Categorize Orientation ----
	// e.g., "1920:1080"
	parts := strings.Split(ratio, ":")
	var prefix string

	if len(parts) == 2 {
		w, _ := strconv.Atoi(parts[0])
		h, _ := strconv.Atoi(parts[1])

		switch {
		case w > h:
			prefix = "landscape-"
		case h > w:
			prefix = "portrait-"
		default:
			prefix = "other-"
		}
	} else {
		prefix = "other-"
	}

	// ---- Process video to faststart MP4 ----
	processedPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process video", err)
		return
	}
	defer os.Remove(processedPath)

	// ---- Upload processed video ----
	processedFile, err := os.Open(processedPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to read processed video", err)
		return
	}
	defer processedFile.Close()

	// ---- 9. Generate S3 key ----
	videoKey := prefix + fmt.Sprintf("%x%s", uuid.New(), filepath.Ext(videoHeader.Filename))

	// ---- 10. Upload to S3 ----
	putInput := &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(videoKey),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	}

	_, err = cfg.s3Client.PutObject(r.Context(), putInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload video to S3", err)
		return
	}

	// ---- 11. Update DB with S3 URL ----
	// videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, videoKey)
	// video.VideoURL = &videoURL
	// ---- presigneed url logic ----
	bucketAndKey := fmt.Sprintf("%s,%s", cfg.s3Bucket, videoKey)
	video.VideoURL = &bucketAndKey

	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video record", err)
		return
	}

	// ---- 12. Respond with updated video ----
	// respondWithJSON(w, http.StatusOK, video)
	signedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to sign video URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, signedVideo)
}

func getVideoAspectRatio(filePath string) (string, error) {

	type ffprobeOutput struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	// Ensure the file path is absolute for safety (optional)
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return "", err
	}

	// Prepare command
	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		absPath,
	)

	// Capture output
	var out bytes.Buffer
	cmd.Stdout = &out

	// Run command
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to execute ffprobe: %w", err)
	}

	// Parse JSON
	var data ffprobeOutput
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		return "", fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	if len(data.Streams) == 0 {
		return "", errors.New("no streams found in video")
	}

	width := data.Streams[0].Width
	height := data.Streams[0].Height

	if width == 0 || height == 0 {
		return "", errors.New("width or height is zero, cannot determine aspect ratio")
	}

	// Format aspect ratio
	return fmt.Sprintf("%d:%d", width, height), nil
}

func processVideoForFastStart(filePath string) (string, error) {

	// Ensure the file path is absolute for safety
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return "", err
	}

	processedPath := absPath + ".processing"

	// Prepare command
	cmd := exec.Command(
		"ffmpeg",
		"-i", absPath,
		"-c", "copy",
		"-movflags",
		"faststart",
		"-f",
		"mp4",
		processedPath,
	)

	// Run command
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to execute ffmpeg: %w", err)
	}

	return processedPath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presigner := s3.NewPresignClient(s3Client)

	req, err := presigner.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))

	if err != nil {
		return "", err
	}

	return req.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil || *video.VideoURL == "" {
		return video, nil
	}

	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) != 2 {
		return video, fmt.Errorf("invalid stored video URL format")
	}

	bucket := parts[0]
	key := parts[1]

	url, err := generatePresignedURL(cfg.s3Client, bucket, key, time.Hour)
	if err != nil {
		return video, err
	}

	video.VideoURL = &url
	return video, nil
}
