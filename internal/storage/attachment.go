package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// AttachmentStorage handles file operations for email attachments on disk.
type AttachmentStorage struct {
	baseDir string // cfg.Storage.AttachDir
}

// NewAttachmentStorage creates a new AttachmentStorage with the given base directory.
func NewAttachmentStorage(baseDir string) *AttachmentStorage {
	return &AttachmentStorage{baseDir: baseDir}
}

// Save writes attachment data to disk and returns the relative file path.
// The filename is generated as {uuid}{ext} to avoid collisions.
func (s *AttachmentStorage) Save(filename string, data []byte) (string, error) {
	// Ensure the base directory exists
	if err := os.MkdirAll(s.baseDir, 0755); err != nil {
		return "", fmt.Errorf("创建附件目录失败: %w", err)
	}

	// Generate a unique filename with the original extension
	ext := filepath.Ext(filename)
	uniqueName := uuid.New().String() + ext

	fullPath := filepath.Join(s.baseDir, uniqueName)
	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return "", fmt.Errorf("写入附件文件失败: %w", err)
	}

	return uniqueName, nil
}

// Read reads attachment data from disk given a relative path.
func (s *AttachmentStorage) Read(relPath string) ([]byte, error) {
	fullPath := s.FullPath(relPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("读取附件文件失败: %w", err)
	}
	return data, nil
}

// Delete removes an attachment file from disk given a relative path.
func (s *AttachmentStorage) Delete(relPath string) error {
	fullPath := s.FullPath(relPath)
	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除附件文件失败: %w", err)
	}
	return nil
}

// FullPath returns the absolute path for a given relative path.
func (s *AttachmentStorage) FullPath(relPath string) string {
	// Prevent directory traversal attacks
	cleanRel := filepath.Clean(relPath)
	if strings.HasPrefix(cleanRel, "..") {
		cleanRel = strings.TrimPrefix(cleanRel, "../")
	}
	return filepath.Join(s.baseDir, cleanRel)
}
