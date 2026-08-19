package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestFullPathRejectsTraversal 验证路径遍历/绝对路径等恶意输入被拒绝。
func TestFullPathRejectsTraversal(t *testing.T) {
	s := NewAttachmentStorage(filepath.Join(t.TempDir(), "attachments"))

	valid := uuid.New().String() + ".pdf"
	bad := []string{
		"../secret.txt",
		"../../etc/passwd",
		"..",
		"....//x",
		"/etc/passwd",
		"a/../b.txt",
		"sub/file.png",
		"",
		".", "..\\..\\x", // windows style
		"00000000-0000-0000-0000-000000000000.exe\r\nBcc: x@y.com",
		"garbage",
		"00000000-0000-0000-0000-000000000000.%2e%2e",
	}
	for _, p := range bad {
		if _, err := s.FullPath(p); err == nil {
			t.Errorf("FullPath(%q) should be rejected", p)
		}
	}

	// 合法文件名必须通过
	full, err := s.FullPath(valid)
	if err != nil {
		t.Fatalf("FullPath(%q) rejected: %v", valid, err)
	}
	if !strings.HasPrefix(full, s.baseDir+string(os.PathSeparator)) {
		t.Fatalf("FullPath(%q) = %q escapes baseDir", valid, full)
	}
}

// TestSaveSanitizesExtension 验证恶意扩展名不会进入文件名。
func TestSaveSanitizesExtension(t *testing.T) {
	s := NewAttachmentStorage(filepath.Join(t.TempDir(), "attachments"))

	// 换行/路径分隔符等非法字符的扩展名应被丢弃
	rel, err := s.Save("evil.pdf\r\nBcc: x@y.com", []byte("data"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if strings.ContainsAny(rel, "\r\n/\\") {
		t.Fatalf("saved name contains dangerous chars: %q", rel)
	}
	if !savedFileRe.MatchString(rel) {
		t.Fatalf("saved name %q does not match allowed pattern", rel)
	}
	// 后续 Read 应能按返回的路径读取
	if _, err := s.Read(rel); err != nil {
		t.Fatalf("Read after Save: %v", err)
	}

	// 正常扩展名保留
	rel2, err := s.Save("report.pdf", []byte("data"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.HasSuffix(rel2, ".pdf") {
		t.Fatalf("extension lost: %q", rel2)
	}
}

// TestReadDeleteRoundTrip 正常读写删流程。
func TestReadDeleteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewAttachmentStorage(filepath.Join(dir, "attachments"))

	rel, err := s.Save("a.txt", []byte("hello"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := s.Read(rel)
	if err != nil || string(data) != "hello" {
		t.Fatalf("Read = %q, %v", data, err)
	}
	if err := s.Delete(rel); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 删除后路径仍然合法（删除不存在文件不算错误）
	if err := s.Delete(rel); err != nil {
		t.Fatalf("Delete again: %v", err)
	}
}
