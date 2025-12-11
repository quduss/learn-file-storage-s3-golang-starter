package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Limit upload size to 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30) // 1 << 30 bytes == 1 GiB
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
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
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You do not own this video", nil)
		return
	}
	// 4) Parse multipart form (small memory; file will be streamed)
	const maxMemory = 32 << 20 // 32 MB for form parsing, file still streamed by FormFile
	if err := r.ParseMultipartForm(maxMemory); err != nil {
		respondWithError(w, http.StatusBadRequest, "Error parsing multipart form", err)
		return
	}
	file, fileHdr, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Missing video file", err)
		return
	}
	defer file.Close()

	contentType := fileHdr.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Unsupported file type. Only MP4 allowed.", nil)
		return
	}
	tmpFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temp file", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// copy file data to temp file
	if _, err := io.Copy(tmpFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to write temp file", err)
		return
	}
	// Rewind the temp file to the beginning so S3 PutObject can read from it
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to seek temp file", err)
		return
	}

	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to generate random file name", err)
		return
	}
	keyBase := fmt.Sprintf("%x", randomBytes) // hex string
	key := fmt.Sprintf("%s.mp4", keyBase)

	putInput := &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        tmpFile, // file pointer implements io.Reader
		ContentType: aws.String(mediaType),
	}
	_, err = cfg.s3Client.PutObject(context.Background(), putInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload to S3", err)
		return
	}
	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, key)
	video.VideoURL = &videoURL

	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video metadata", err)
		return
	}

	// 11) Respond with updated video metadata
	respondWithJSON(w, http.StatusOK, video)
}
