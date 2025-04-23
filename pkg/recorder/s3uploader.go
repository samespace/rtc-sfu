package recorder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3UploadConfig contains configuration for uploading recordings to S3
type S3UploadConfig struct {
	// Whether to upload recordings to S3
	Enabled bool
	// S3 bucket name
	BucketName string
	// S3 endpoint (e.g., "s3.amazonaws.com" for AWS or custom endpoint for Minio)
	Endpoint string
	// Region (optional, can be empty for Minio)
	Region string
	// S3 key prefix (folder path)
	KeyPrefix string
	// Credentials
	AccessKeyID     string
	SecretAccessKey string
	// Use SSL/TLS for connection
	UseSSL bool
	// Whether to delete local files after successful upload
	DeleteAfterUpload bool
}

// S3Uploader handles uploading recordings to S3
type S3Uploader struct {
	client        *minio.Client
	config        S3UploadConfig
	uploadQueue   chan uploadTask
	uploadResults map[string]uploadResult
	mu            sync.RWMutex
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

type uploadTask struct {
	localPath  string
	s3Key      string
	identifier string
}

type uploadResult struct {
	success   bool
	error     error
	key       string
	timestamp time.Time
}

// NewS3Uploader creates a new S3Uploader
func NewS3Uploader(ctx context.Context, config S3UploadConfig) (*S3Uploader, error) {
	if !config.Enabled {
		return nil, fmt.Errorf("s3 upload not enabled")
	}

	if config.BucketName == "" {
		return nil, fmt.Errorf("s3 bucket name required")
	}

	if config.Endpoint == "" {
		return nil, fmt.Errorf("s3 endpoint required")
	}

	ctx, cancel := context.WithCancel(ctx)

	uploader := &S3Uploader{
		config:        config,
		uploadQueue:   make(chan uploadTask, 100),
		uploadResults: make(map[string]uploadResult),
		ctx:           ctx,
		cancel:        cancel,
	}

	err := uploader.initS3Client()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to initialize S3 client: %w", err)
	}

	// Start worker goroutines
	uploader.wg.Add(1)
	go uploader.processUploads()

	return uploader, nil
}

// initS3Client initializes the S3 client
func (u *S3Uploader) initS3Client() error {
	// Create minio client
	client, err := minio.New(u.config.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(u.config.AccessKeyID, u.config.SecretAccessKey, ""),
		Secure: u.config.UseSSL,
		Region: u.config.Region,
	})

	if err != nil {
		return fmt.Errorf("failed to create minio client: %w", err)
	}

	// Check if bucket exists and we have access
	exists, err := client.BucketExists(u.ctx, u.config.BucketName)
	if err != nil {
		return fmt.Errorf("failed to check bucket existence: %w", err)
	}

	if !exists {
		return fmt.Errorf("bucket %s does not exist", u.config.BucketName)
	}

	u.client = client
	return nil
}

// UploadFile queues a file for upload to S3
func (u *S3Uploader) UploadFile(localPath, identifier string) error {
	if !u.config.Enabled {
		return fmt.Errorf("s3 upload not enabled")
	}

	// Determine S3 key from local path
	fileName := filepath.Base(localPath)
	s3Key := fileName
	if u.config.KeyPrefix != "" {
		prefix := u.config.KeyPrefix
		if !strings.HasSuffix(prefix, "/") {
			prefix = prefix + "/"
		}
		s3Key = prefix + s3Key
	}

	// Add room ID and identifier to key path
	if identifier != "" {
		s3Key = filepath.Join(filepath.Dir(s3Key), identifier, fileName)
	}

	// Queue the upload task
	select {
	case u.uploadQueue <- uploadTask{
		localPath:  localPath,
		s3Key:      s3Key,
		identifier: identifier,
	}:
		return nil
	case <-u.ctx.Done():
		return fmt.Errorf("uploader is shutting down")
	}
}

// UploadSingleFile uploads only the specified file to S3
func (u *S3Uploader) UploadSingleFile(localPath, identifier, targetFileName string) error {
	if !u.config.Enabled {
		return fmt.Errorf("s3 upload not enabled")
	}

	// Use the target filename if provided, otherwise use the original filename
	fileName := targetFileName
	if fileName == "" {
		fileName = filepath.Base(localPath)
	}

	s3Key := fileName
	if u.config.KeyPrefix != "" {
		prefix := u.config.KeyPrefix
		if !strings.HasSuffix(prefix, "/") {
			prefix = prefix + "/"
		}
		s3Key = prefix + s3Key
	}

	// Add identifier to key path if provided
	if identifier != "" {
		s3Key = filepath.Join(identifier, s3Key)
	}

	// Queue the upload task
	fmt.Printf("(Queueing) Uploading file to S3: %s\n", s3Key)
	select {
	case u.uploadQueue <- uploadTask{
		localPath:  localPath,
		s3Key:      s3Key,
		identifier: identifier,
	}:
		return nil
	case <-u.ctx.Done():
		return fmt.Errorf("uploader is shutting down")
	}
}

// UploadDirectory uploads all files in a directory to S3
func (u *S3Uploader) UploadDirectory(dirPath, identifier string) error {
	return filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			relPath, err := filepath.Rel(dirPath, path)
			if err != nil {
				return err
			}

			s3Key := relPath
			if u.config.KeyPrefix != "" {
				prefix := u.config.KeyPrefix
				if !strings.HasSuffix(prefix, "/") {
					prefix = prefix + "/"
				}
				s3Key = prefix + s3Key
			}

			if identifier != "" {
				s3Key = filepath.Join(identifier, s3Key)
			}

			select {
			case u.uploadQueue <- uploadTask{
				localPath:  path,
				s3Key:      s3Key,
				identifier: identifier,
			}:
				// Successfully queued
			case <-u.ctx.Done():
				return fmt.Errorf("uploader is shutting down")
			}
		}
		return nil
	})
}

// UploadMergedFile uploads only the merged.wav file with the specified name
func (u *S3Uploader) UploadMergedFile(dirPath, identifier, targetFileName string) error {
	mergedFilePath := filepath.Join(dirPath, "merged.wav")

	// Check if the merged file exists
	if _, err := os.Stat(mergedFilePath); os.IsNotExist(err) {
		return fmt.Errorf("merged file does not exist: %s", mergedFilePath)
	}

	return u.UploadSingleFile(mergedFilePath, identifier, targetFileName)
}

// GetUploadStatus returns the status of a specific upload
func (u *S3Uploader) GetUploadStatus(identifier string) (bool, error) {
	u.mu.RLock()
	defer u.mu.RUnlock()

	result, exists := u.uploadResults[identifier]
	if !exists {
		return false, fmt.Errorf("no upload found for identifier: %s", identifier)
	}

	return result.success, result.error
}

// processUploads handles uploading files from the queue
func (u *S3Uploader) processUploads() {
	defer u.wg.Done()

	// Map to keep track of uploaded paths for directory cleanup
	uploadedFiles := make(map[string]bool)
	pendingDirs := make(map[string]bool)

	for {
		select {
		case <-u.ctx.Done():
			return
		case task := <-u.uploadQueue:
			// Process upload
			success, err := u.uploadToS3(task.localPath, task.s3Key)

			// Store result
			u.mu.Lock()
			u.uploadResults[task.identifier] = uploadResult{
				success:   success,
				error:     err,
				key:       task.s3Key,
				timestamp: time.Now(),
			}
			u.mu.Unlock()

			// Delete local file if configured and upload was successful
			if success && u.config.DeleteAfterUpload {
				// Add to uploaded files
				uploadedFiles[task.localPath] = true

				// Track directory
				dir := filepath.Dir(task.localPath)
				pendingDirs[dir] = true

				// Delete file
				os.Remove(task.localPath)

				// Check if directory is empty and can be deleted
				u.cleanupEmptyDirs(dir, pendingDirs)
			}
		}
	}
}

// cleanupEmptyDirs checks if the directory is empty and deletes it if so,
// then recursively checks parent directories
func (u *S3Uploader) cleanupEmptyDirs(dir string, pendingDirs map[string]bool) {
	if !u.config.DeleteAfterUpload {
		return
	}

	// Check if directory exists
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		delete(pendingDirs, dir)
		return
	}

	// Check if directory is empty
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	// If directory is not empty, don't delete
	if len(entries) > 0 {
		return
	}

	// Delete empty directory
	if err := os.Remove(dir); err == nil {
		delete(pendingDirs, dir)

		// Recursively check parent directory
		parent := filepath.Dir(dir)
		if parent != dir && pendingDirs[parent] {
			u.cleanupEmptyDirs(parent, pendingDirs)
		}
	}
}

// uploadToS3 uploads a single file to S3
func (u *S3Uploader) uploadToS3(localPath, s3Key string) (bool, error) {
	fmt.Printf("(Uploading) Uploading file to S3: %s\n", s3Key)
	file, err := os.Open(localPath)
	if err != nil {
		return false, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Get file info for content type detection
	fileInfo, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("failed to get file info: %w", err)
	}

	// Determine content type based on file extension
	contentType := "application/octet-stream"
	switch filepath.Ext(localPath) {
	case ".wav":
		contentType = "audio/wav"
	case ".ogg":
		contentType = "audio/ogg"
	case ".json":
		contentType = "application/json"
	}

	// Upload file to S3
	_, err = u.client.PutObject(
		u.ctx,
		u.config.BucketName,
		s3Key,
		file,
		fileInfo.Size(),
		minio.PutObjectOptions{ContentType: contentType},
	)

	fmt.Printf("(Uploaded) Uploaded file to S3: %s\n", s3Key)

	if err != nil {
		return false, fmt.Errorf("failed to upload file to S3: %w", err)
	}

	return true, nil
}

// Close shuts down the uploader
func (u *S3Uploader) Close() {
	u.cancel()
	u.wg.Wait()
	close(u.uploadQueue)
}
