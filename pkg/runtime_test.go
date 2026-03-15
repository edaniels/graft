package graft

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCollectCommandsFromPATH(t *testing.T) {
	tempDir := t.TempDir()

	exec1, err := os.Create(filepath.Join(tempDir, "exec1"))
	if err != nil {
		t.Fatal(err)
	}

	exec2, err := os.Create(filepath.Join(tempDir, "exec2"))
	if err != nil {
		t.Fatal(err)
	}

	_ = exec1
	_ = exec2
	newPath := tempDir

	curPath := os.Getenv("PATH")
	if curPath != "" {
		newPath = newPath + ":" + curPath
	}

	t.Setenv("PATH", newPath)

	collected := collectCommandsFromPATH()
	if slices.Contains(collected, exec1.Name()) {
		t.Fatalf("collected should not contain %s but it did", exec1.Name())
	}

	if slices.Contains(collected, exec2.Name()) {
		t.Fatalf("collected should not contain %s but it did", exec1.Name())
	}

	if err := exec1.Chmod(0o777); err != nil {
		t.Fatal(err)
	}

	if err := exec2.Chmod(0o777); err != nil {
		t.Fatal(err)
	}

	collected = collectCommandsFromPATH()
	if !slices.Contains(collected, exec1.Name()) {
		t.Fatalf("collected should contain %s but it didn't", exec1.Name())
	}

	if !slices.Contains(collected, exec2.Name()) {
		t.Fatalf("collected should contain %s but it didn't", exec1.Name())
	}
}
