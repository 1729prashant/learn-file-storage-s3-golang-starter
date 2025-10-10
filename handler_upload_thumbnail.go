package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"

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

	// TODO: implement the upload here
	// START Lesson Implementations
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

	// Read the file content into a byte slice
	fileBytesThumbnail, err := io.ReadAll(multipartFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't read file", err)
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

	// instruction - Because the thumbnail_url has all the data we need, delete the global thumbnail map and the GET route for thumbnails.
	// Save the thumbnail to the global map
	// Create a new thumbnail struct with the image data and media type
	// Add the thumbnail to the global map, using the video's ID as the key
	// videoThumbnails[videoID] = thumbnail{
	// data:      fileBytesThumbnail,
	// mediaType: mediaType,
	//}

	// Update the video metadata so that it has a new thumbnail URL, then update the record in
	// the database by using the cfg.db.UpdateVideo function. The thumbnail URL should have this format:
	// http://localhost:<port>/api/thumbnails/{videoID}
	// This will all work because the /api/thumbnails/{videoID} endpoint serves thumbnails from that global map.
	// url := fmt.Sprintf("http://localhost:%s/api/thumbnails/%s", cfg.port, videoID)
	base64Thumbnail := base64.StdEncoding.EncodeToString(fileBytesThumbnail)
	dataURL := "data:" + mediaType + ";base64," + base64Thumbnail
	video.ThumbnailURL = &dataURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		// delete(videoThumbnails, videoID) instruction - Because the thumbnail_url has all the data we need, delete the global thumbnail map and the GET route for thumbnails.
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	// Respond with updated JSON of the video's metadata. Use the provided respondWithJSON function and pass it the updated database. Video struct to marshal.
	// before change - respondWithJSON(w, http.StatusOK, struct{}{})
	respondWithJSON(w, http.StatusOK, video)

	// END   Lesson Implementations

}
