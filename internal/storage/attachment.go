package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// savedFileRe 匹配 Save 生成的文件名：UUID（小写十六进制）+ 可选白名单扩展名。
// 只允许这种格式的路径进入文件系统，杜绝路径遍历（../）、绝对路径等。
var savedFileRe = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}(\.[A-Za-z0-9._-]{1,32})?$`)

// AttachmentStorage handles file operations for email attachments on disk.
type AttachmentStorage struct {
	baseDir string // cfg.Storage.AttachDir
}

// NewAttachmentStorage creates a new AttachmentStorage with the given base directory.
func NewAttachmentStorage(baseDir string) *AttachmentStorage {
	return &AttachmentStorage{baseDir: baseDir}
}

// safeExt 提取并白名单化文件扩展名：只保留字母数字与 ._-，最长 32 字符。
// 非法字符（含 CR/LF、路径分隔符）直接丢弃扩展名。
func safeExt(filename string) string {
	ext := filepath.Ext(filename)
	if len(ext) > 33 {
		return ""
	}
	for _, r := range ext {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return ""
		}
	}
	return ext
}

// Save writes attachment data to disk and returns the relative file path.
// The filename is generated as {uuid}{ext} to avoid collisions.
func (s *AttachmentStorage) Save(filename string, data []byte) (string, error) {
	// Ensure the base directory exists
	if err := os.MkdirAll(s.baseDir, 0755); err != nil {
		return "", fmt.Errorf("创建附件目录失败: %w", err)
	}

	// Generate a unique filename with a sanitized extension
	ext := safeExt(filename)
	uniqueName := uuid.New().String() + ext

	fullPath := filepath.Join(s.baseDir, uniqueName)
	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return "", fmt.Errorf("写入附件文件失败: %w", err)
	}

	return uniqueName, nil
}

// Read reads attachment data from disk given a relative path.
func (s *AttachmentStorage) Read(relPath string) ([]byte, error) {
	fullPath, err := s.FullPath(relPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("读取附件文件失败: %w", err)
	}
	return data, nil
}

// Delete removes an attachment file from disk given a relative path.
func (s *AttachmentStorage) Delete(relPath string) error {
	fullPath, err := s.FullPath(relPath)
	if err != nil {
		return err
	}
	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除附件文件失败: %w", err)
	}
	return nil
}

// FullPath returns the absolute path for a relative path produced by Save.
// Paths that do not match the saved-file format (traversal attempts,
// absolute paths, unrelated names) are rejected with an error so they can
// never escape baseDir.
func (s *AttachmentStorage) FullPath(relPath string) (string, error) {
	if !savedFileRe.MatchString(relPath) {
		return "", fmt.Errorf("非法的附件路径: %q", relPath)
	}

	// 兜底校验：解析后的路径必须仍在 baseDir 内
	fullPath := filepath.Join(s.baseDir, relPath)
	if !strings.HasPrefix(fullPath, filepath.Clean(s.baseDir)+string(os.PathSeparator)) {
		return "", fmt.Errorf("附件路径越界: %q", relPath)
	}
	return fullPath, nil
}
