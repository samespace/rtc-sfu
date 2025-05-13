package recording

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pion/logging"
)

// S3Config contains configuration for S3 storage
type S3Config struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
	Bucket          string
	Prefix          string
}

// Uploader handles uploading files to S3
type Uploader struct {
	client *minio.Client
	config S3Config
	logger logging.LeveledLogger
}

// NewUploader creates a new S3 uploader
func NewUploader(ctx context.Context, config S3Config, logger logging.LeveledLogger) (*Uploader, error) {
	client, err := minio.New(config.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(config.AccessKeyID, config.SecretAccessKey, ""),
		Secure: config.UseSSL,
	})

	if err != nil {
		return nil, fmt.Errorf("failed to create S3 client: %w", err)
	}

	// Check if bucket exists
	exists, err := client.BucketExists(ctx, config.Bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to check if bucket exists: %w", err)
	}

	if !exists {
		return nil, fmt.Errorf("bucket %s does not exist", config.Bucket)
	}

	return &Uploader{
		client: client,
		config: config,
		logger: logger,
	}, nil
}

// UploadFile uploads a file to S3
func (u *Uploader) UploadFile(ctx context.Context, localFilePath string, objectKey string) (string, error) {
	// Open the local file
	file, err := os.Open(localFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Get file information
	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	// Create object key with prefix
	if u.config.Prefix != "" {
		if !strings.HasSuffix(u.config.Prefix, "/") {
			objectKey = u.config.Prefix + "/" + objectKey
		} else {
			objectKey = u.config.Prefix + objectKey
		}
	}

	// Set content type based on file extension
	contentType := "application/octet-stream"
	if strings.HasSuffix(localFilePath, ".ogg") {
		contentType = "audio/ogg"
	} else if strings.HasSuffix(localFilePath, ".json") {
		contentType = "application/json"
	}

	// Upload the file
	_, err = u.client.PutObject(
		ctx,
		u.config.Bucket,
		objectKey,
		file,
		fileInfo.Size(),
		minio.PutObjectOptions{ContentType: contentType},
	)

	if err != nil {
		return "", fmt.Errorf("failed to upload file: %w", err)
	}

	// Return the S3 URL
	return fmt.Sprintf("%s/%s/%s", u.client.EndpointURL(), u.config.Bucket, objectKey), nil
}

// UploadRecording uploads a recording directory to S3
func (u *Uploader) UploadRecording(ctx context.Context, recordingID, basePath string, includeMergedOnly bool) ([]string, error) {
	recordingDir := filepath.Join(basePath, recordingID)

	// Check if directory exists
	info, err := os.Stat(recordingDir)
	if err != nil {
		return nil, fmt.Errorf("failed to access recording directory: %w", err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("recording path is not a directory")
	}

	// List of uploaded file URLs
	uploadedFiles := make([]string, 0)

	if includeMergedOnly {
		// Upload only the merged file
		mergedFilePath := filepath.Join(basePath, recordingID, "merged.ogg")
		if _, err := os.Stat(mergedFilePath); os.IsNotExist(err) {
			return nil, fmt.Errorf("merged file does not exist")
		}

		objectKey := recordingID + "/merged.ogg"
		url, err := u.UploadFile(ctx, mergedFilePath, objectKey)
		if err != nil {
			return nil, err
		}

		uploadedFiles = append(uploadedFiles, url)

		// Also upload the merged metadata if it exists
		mergedMetadataPath := mergedFilePath + ".json"
		if _, err := os.Stat(mergedMetadataPath); err == nil {
			objectKey = recordingID + "/merged.ogg.json"
			url, err := u.UploadFile(ctx, mergedMetadataPath, objectKey)
			if err != nil {
				u.logger.Warnf("Failed to upload merged metadata: %v", err)
			} else {
				uploadedFiles = append(uploadedFiles, url)
			}
		}
	} else {
		// Upload all files
		err = filepath.Walk(recordingDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if !info.IsDir() {
				// Create object key by removing base path
				relPath, err := filepath.Rel(basePath, path)
				if err != nil {
					return err
				}

				objectKey := relPath
				url, err := u.UploadFile(ctx, path, objectKey)
				if err != nil {
					return err
				}

				uploadedFiles = append(uploadedFiles, url)
			}

			return nil
		})

		if err != nil {
			return uploadedFiles, fmt.Errorf("failed to walk directory: %w", err)
		}
	}

	return uploadedFiles, nil
}
