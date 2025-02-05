package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func getVideoAspectRatio(filepath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filepath)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffprobe error: %w", err)
	}

	var output struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"Height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return "", fmt.Errorf("could not parse ffprobe output: %w", err)
	}
	ratio := float64(output.Streams[0].Width) / float64(output.Streams[0].Height)
	if ratio > 1.77 && ratio < 1.79 {
		return "16:9", nil
	} else if ratio > 0.56 && ratio < 0.57 {
		return "9:16", nil
	} else {
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	outPath := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outPath)
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffmpeg error: %w", err)
	}
	return outPath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	request, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}
	return request.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	vals := strings.Split(*video.VideoURL, ",")
	if len(vals) < 2 {
		return video, nil
	}
	bucket := vals[0]
	key := vals[1]
	presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, time.Minute*5)
	if err != nil {
		return video, err
	}
	video.VideoURL = &presignedURL
	return video, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Set upload limit
	const maxMemory int64 = 1 << 30 // 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)

	// Get VideoID
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// Authenticate user
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

	// Validate user is owner of video
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't get video", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not owner of video", nil)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	// Parse video
	srcFile, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't read video", err)
		return
	}
	defer srcFile.Close()

	mediaType, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Missing Content-Type for video", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusUnsupportedMediaType, "Unsupported file type", nil)
		return
	}

	// Save uploaded file to temp file on disk
	tmpFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to open temp file", nil)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()
	_, err = io.Copy(tmpFile, srcFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to upload to server", nil)
		return
	}

	_, err = tmpFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error reading file", err)
		return
	}
	processedFileName, err := processVideoForFastStart(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing file", err)
		return
	}
	processedFile, err := os.Open(processedFileName)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed file", err)
		return
	}
	defer os.Remove(processedFile.Name())
	defer processedFile.Close()

	// Get video aspect ratio
	videoAspectRatio, err := getVideoAspectRatio(processedFile.Name())
	if err != nil {

	}
	prefixMap := map[string]string{
		"16:9":  "landscape",
		"9:16":  "portrait",
		"other": "other",
	}

	prefix := prefixMap[videoAspectRatio]

	// Upload file to S3 storage
	bytes := make([]byte, 32)
	_, err = rand.Read(bytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate filename", err)
		return
	}

	fileName := base64.RawURLEncoding.EncodeToString(bytes)
	fileExtention := strings.Split(mediaType, "/")
	fileNameWithExt := fmt.Sprintf("%s/%s.%s", prefix, fileName, fileExtention[1])

	fmt.Println(fileName, fileExtention, fileNameWithExt, cfg.s3Bucket, mediaType)

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(fileNameWithExt),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload to S3", err)
		return
	}
	// newVideoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, fileNameWithExt)
	newVideoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, fileNameWithExt)
	fmt.Println(newVideoURL)
	video.VideoURL = &newVideoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update link to video in database", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate signed videoURL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
