package main

import (
	"bytes"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
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
			prefix = "landscape/"
		case h > w:
			prefix = "portrait/"
		default:
			prefix = "other/"
		}
	} else {
		prefix = "other/"
	}

	// ---- 9. Generate S3 key ----
	videoKey := prefix + fmt.Sprintf("%x%s", uuid.New(), filepath.Ext(videoHeader.Filename))

	// ---- 10. Upload to S3 ----
	putInput := &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(videoKey),
		Body:        tempFile,
		ContentType: aws.String(mediaType),
	}

	_, err = cfg.s3Client.PutObject(r.Context(), putInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload video to S3", err)
		return
	}

	// ---- 11. Update DB with S3 URL ----
	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, videoKey)
	video.VideoURL = &videoURL

	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video record", err)
		return
	}

	// ---- 12. Respond with updated video ----
	respondWithJSON(w, http.StatusOK, video)
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
