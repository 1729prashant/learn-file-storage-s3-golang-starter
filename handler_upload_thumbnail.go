package main

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	// Parse the form data
	const maxMemory = 10 * (1 << 20) // 1 << 20 is 1024 * 1024 (1 MB)
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse form", err)
		return
	}

	// Get the image data from the form
	multipartFile, multipartFileHeader, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse thumbnail", err)
	}
	defer multipartFile.Close()

	mediaType := multipartFileHeader.Header.Get("Content-Type")
	if mediaType == "" {
		respondWithError(w, http.StatusBadRequest, "Missing Content-Type for thumbnail", nil)
		return
	}

	mediaTypeCheck, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to read mime in Content-Type", nil)
		return
	}
	if mediaTypeCheck != "image/jpeg" && mediaTypeCheck != "image/png" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}

	assetPath := getAssetPath(videoID, mediaType)
	thumbnailDiskPath := cfg.getAssetDiskPath(assetPath)

	thumbnailFile, err := os.Create(thumbnailDiskPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create file on server", err)
		return
	}
	defer thumbnailFile.Close()
	if _, err = io.Copy(thumbnailFile, multipartFile); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error saving file", err)
		return
	}

	// Get the video's metadata from the SQLite database. The apiConfig's db has a GetVideo method you can use
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}
	// If the authenticated user is not the video owner, return a http.StatusUnauthorized response
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", nil)
		return
	}

	dataURL := cfg.getAssetURL(assetPath)
	video.ThumbnailURL = &dataURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)

}
